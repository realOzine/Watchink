package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"time"

	"watchink/internal/app"
	"watchink/internal/store"
)

// StageContext is what a running stage sees of its job.
type StageContext struct {
	// Job is the job row as it was when the stage started.
	Job store.Job
	// Stage is the name of the running stage.
	Stage string
	// Attempt is the job attempt (1 for the first run, +1 per retry);
	// StageAttempt counts the starts of this particular stage.
	Attempt      int
	StageAttempt int
	// IgnoreCache is true when the job's reprocess_from option covers this
	// stage (StageDef.ReprocessGroups): the stage must not reuse a
	// registered artifact and must produce a fresh one (§7.5).
	IgnoreCache bool
	// Logger carries the job_id and stage attributes.
	Logger *slog.Logger

	rs   *runStage
	snap *store.JobWithStages
}

// DepResult is the recorded outcome of an earlier stage.
type DepResult struct {
	ArtifactID  string
	Fingerprint string
	Reused      bool
}

// JobID returns the job ID.
func (sc *StageContext) JobID() string { return sc.Job.ID }

// VideoID returns the video of the job ("" for prepare_tools).
func (sc *StageContext) VideoID() string { return sc.Job.VideoID }

// DecodeOptions unmarshals the job options JSON into v.
func (sc *StageContext) DecodeOptions(v any) error {
	if err := json.Unmarshal(sc.Job.Options, v); err != nil {
		return app.E(app.CodeInternal, "任务参数无法解析：%v", err).WithCause(err)
	}
	return nil
}

// Dep returns the artifact and fingerprint recorded by a stage that had
// succeeded when this stage started — always the case for the declared
// dependencies and, transitively, theirs. ok is false otherwise. The values
// come from job_stages, so they are also there after a restart or a retry
// that skipped the dependency.
func (sc *StageContext) Dep(stage string) (res DepResult, ok bool) {
	row := sc.snap.Stage(stage)
	if row == nil || row.Status != app.StageSucceeded {
		return DepResult{}, false
	}
	return DepResult{ArtifactID: row.ArtifactID, Fingerprint: row.Fingerprint, Reused: row.Reused}, true
}

// Acquire is Scheduler.Acquire: an llm stage wraps each model request in
// Acquire(ctx, jobs.ResourceLLM) / release.
func (sc *StageContext) Acquire(ctx context.Context, resource string) (release func(), err error) {
	return sc.rs.s.Acquire(ctx, resource)
}

// Progress reports the stage progress in [0, 1]. Calls are cheap: the value
// is persisted and published at most once per Options.ProgressInterval
// (about twice a second), the latest value winning. Success sets 1 by itself.
func (sc *StageContext) Progress(p float64) {
	if math.IsNaN(p) {
		return
	}
	p = min(max(p, 0), 1)
	rs := sc.rs
	rs.pmu.Lock()
	defer rs.pmu.Unlock()
	if rs.pClosed {
		return
	}
	wait := rs.s.opts.ProgressInterval - time.Since(rs.pLast)
	if wait <= 0 {
		rs.flushProgress(p)
		return
	}
	rs.pPending = &p
	if rs.pTimer == nil {
		rs.pTimer = time.AfterFunc(wait, func() {
			rs.pmu.Lock()
			defer rs.pmu.Unlock()
			rs.pTimer = nil
			if !rs.pClosed && rs.pPending != nil {
				rs.flushProgress(*rs.pPending)
			}
		})
	}
}

// flushProgress persists p. Callers hold pmu. Progress bumps rev (long
// polls and the event stream see it) but writes no job_events row.
func (rs *runStage) flushProgress(p float64) {
	rs.pPending = nil
	rs.pLast = time.Now()
	if _, err := rs.s.apply(rs.rj.id, store.JobUpdate{Stages: []store.StagePatch{{Stage: rs.def.Name, Progress: &p}}}); err != nil {
		rs.s.log.Warn("scheduler: cannot record progress", "job_id", rs.rj.id, "stage", rs.def.Name, "error", err)
	}
}

// closeProgress ends all writes on behalf of the stage; it returns only
// after an in-flight one finished.
func (rs *runStage) closeProgress() {
	rs.pmu.Lock()
	defer rs.pmu.Unlock()
	rs.pClosed = true
	rs.pPending = nil
	if rs.pTimer != nil {
		rs.pTimer.Stop()
		rs.pTimer = nil
	}
}

// SetResult updates the job result (transcript_id / note_id) as soon as the
// stage has produced it, so readers see e.g. the note while playback is
// still running (§7.2). fn receives the current result and edits it in
// place. It fails when the scheduler has already given up on the stage
// (forced cancel or stop timeout).
func (sc *StageContext) SetResult(fn func(*app.JobResult)) error {
	rs := sc.rs
	rs.pmu.Lock()
	defer rs.pmu.Unlock()
	if rs.pClosed {
		return app.E(app.CodeConflict, "阶段已结束，不能再更新任务结果")
	}
	s := rs.s
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	ctx := context.Background()
	cur, err := s.st.GetJob(ctx, rs.rj.id)
	if err != nil {
		return err
	}
	res := cur.Result
	fn(&res)
	snap, err := s.st.UpdateJob(ctx, rs.rj.id, store.JobUpdate{
		Job:   store.JobPatch{Result: &res},
		Event: &store.EventInput{Type: "job.result", Data: res},
	})
	if err != nil {
		return err
	}
	s.emit(snap)
	return nil
}

// Cancel cancels a job (§7.3). A queued job becomes cancelled at once. A
// running job becomes cancelling, its stage contexts are cancelled, and it
// turns cancelled when every stage has returned; if that takes longer than
// Options.CancelTimeout the ForceKill hook is called and the job is marked
// cancelled with forced=true in its event. Cancel returns right after the
// first transition with the job as it is then.
//
// Errors: NOT_FOUND, CONFLICT for any other status, ENGINE_UNAVAILABLE
// after Stop.
func (s *Scheduler) Cancel(ctx context.Context, jobID string) (app.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return app.Job{}, errStopping()
	}
	j, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return app.Job{}, err
	}
	rj := s.active[jobID]
	now := app.Now()
	switch j.Status {
	case app.JobQueued:
		snap, err := s.apply(jobID, store.JobUpdate{
			IfStatus: []string{app.JobQueued},
			Job:      store.JobPatch{Status: ptr(app.JobCancelled), FinishedAt: &now},
			Event:    &store.EventInput{Type: "job.cancelled", Data: map[string]any{"forced": false}},
		})
		if err != nil {
			return app.Job{}, err
		}
		delete(s.active, jobID)
		return s.ToDTO(snap), nil
	case app.JobRunning:
		snap, err := s.apply(jobID, store.JobUpdate{
			IfStatus: []string{app.JobRunning},
			Job:      store.JobPatch{Status: ptr(app.JobCancelling)},
			Event:    &store.EventInput{Type: "job.cancelling"},
		})
		if err != nil {
			return app.Job{}, err
		}
		if rj == nil {
			rj = &runJob{id: jobID, running: map[string]*runStage{}}
			s.active[jobID] = rj
		}
		rj.cancelling = true
		if len(rj.running) == 0 {
			// Between stages (e.g. waiting for a resource): nothing to stop.
			s.settle(rj)
			return s.Get(ctx, jobID)
		}
		rj.ask()
		rj.timer = time.AfterFunc(s.opts.CancelTimeout, func() { s.forceCancel(rj) })
		return s.ToDTO(snap), nil
	default:
		return app.Job{}, app.E(app.CodeConflict, "任务当前状态为 %s，不能取消", j.Status).
			WithDetail("status", j.Status).WithHint("只有排队中或运行中的任务可以取消")
	}
}

// forceCancel ends a cancelling job whose stages did not stop in time.
func (s *Scheduler) forceCancel(rj *runJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[rj.id] != rj || len(rj.running) == 0 {
		return
	}
	s.log.Warn("scheduler: cancel timed out, killing child processes", "job_id", rj.id)
	s.abandon(rj, app.StageCancelled, app.JobCancelled, nil, map[string]any{"forced": true})
}

// abandon force-kills what is left of a job, detaches its stage goroutines
// and writes the final states itself. A detached goroutine keeps its
// resource slot until it really returns, because its work may still be
// using the resource. Callers hold mu.
func (s *Scheduler) abandon(rj *runJob, stageStatus, jobStatus string, jobErr *app.Error, data map[string]any) {
	if s.opts.ForceKill != nil {
		s.opts.ForceKill(rj.id)
	}
	now := app.Now()
	u := store.JobUpdate{Event: &store.EventInput{Type: "stage.abandoned", Data: data}}
	for name, rs := range rj.running {
		rs.asked, rs.detached = true, true
		rs.cancel()
		rs.closeProgress()
		p := store.StagePatch{Stage: name, Status: &stageStatus}
		if stageStatus == app.StagePending {
			p.Progress, p.StartedAt, p.FinishedAt = ptr(0.0), ptr(""), ptr("")
		} else {
			p.FinishedAt = &now
		}
		u.Stages = append(u.Stages, p)
		delete(rj.running, name)
	}
	if _, err := s.apply(rj.id, u); err != nil {
		s.log.Error("scheduler: cannot record abandoned stages", "job_id", rj.id, "error", err)
	}
	s.finish(rj, jobStatus, jobErr, data)
}

// Retry re-queues a failed, cancelled or interrupted job under the same job
// ID with attempt + 1 (§7.3). Stages that did not succeed go back to
// pending. A succeeded stage is kept, unless a stage that is going to run
// depends on it and its artifact is gone (purged or file missing) — then it
// runs again too, so a retry never trips over a deleted intermediate file.
//
// Errors: NOT_FOUND, CONFLICT for any other status, ENGINE_UNAVAILABLE
// after Stop.
func (s *Scheduler) Retry(ctx context.Context, jobID string) (app.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return app.Job{}, errStopping()
	}
	j, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return app.Job{}, err
	}
	allowed := []string{app.JobFailed, app.JobCancelled, app.JobInterrupted}
	switch j.Status {
	case app.JobFailed, app.JobCancelled, app.JobInterrupted:
	default:
		return app.Job{}, app.E(app.CodeConflict, "任务当前状态为 %s，不能重试", j.Status).
			WithDetail("status", j.Status).WithHint("只有失败、已取消或被中断的任务可以重试")
	}

	rerun := make(map[string]bool, len(j.Stages))
	for i := range j.Stages {
		rerun[j.Stages[i].Stage] = j.Stages[i].Status != app.StageSucceeded
	}
	if defs, err := s.defsFor(&j.Job); err == nil {
		// Reverse DAG order, so a dependency revived here is itself
		// checked against its own dependencies later in the loop.
		for i := len(defs) - 1; i >= 0; i-- {
			if !rerun[defs[i].Name] {
				continue
			}
			for _, dep := range defs[i].Deps {
				if row := j.Stage(dep); row != nil && !rerun[dep] && !s.artifactUsable(ctx, row.ArtifactID) {
					rerun[dep] = true
				}
			}
		}
	}
	attempt := j.Attempt + 1
	u := store.JobUpdate{
		IfStatus: allowed,
		Job:      store.JobPatch{Status: ptr(app.JobQueued), Attempt: &attempt, ClearError: true, FinishedAt: ptr("")},
		Event:    &store.EventInput{Type: "job.retried", Data: map[string]any{"attempt": attempt, "from": j.Status}},
	}
	for i := range j.Stages {
		if name := j.Stages[i].Stage; rerun[name] {
			u.Stages = append(u.Stages, store.StagePatch{Stage: name, Status: ptr(app.StagePending), Progress: ptr(0.0),
				ClearError: true, Reused: ptr(false), StartedAt: ptr(""), FinishedAt: ptr("")})
		}
	}
	snap, err := s.apply(jobID, u)
	if err != nil {
		return app.Job{}, err
	}
	delete(s.active, jobID)
	s.Wake()
	return s.ToDTO(snap), nil
}

// artifactUsable is the cheap presence check used by Retry; the stage's own
// fingerprint check (with hashing) still runs when it executes.
func (s *Scheduler) artifactUsable(ctx context.Context, artifactID string) bool {
	if artifactID == "" {
		return true // the stage registers no artifact
	}
	a, err := s.st.GetArtifact(ctx, artifactID)
	if err != nil {
		return app.CodeOf(err) != app.CodeNotFound // do not rerun on a transient read error
	}
	return s.st.ArtifactPresent(a)
}

// Wait is the long poll behind GET /jobs/{id}?wait_seconds=&after_rev=
// (§13.2): it returns the job as soon as its rev is greater than afterRev,
// or the current job when timeout elapses or the scheduler stops. A timeout
// <= 0 returns immediately. It returns ctx.Err() when ctx ends first and
// NOT_FOUND for an unknown job.
func (s *Scheduler) Wait(ctx context.Context, jobID string, afterRev int64, timeout time.Duration) (app.Job, error) {
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	for {
		// Take the channel before reading: a change after the read closes
		// this very channel, so it cannot be missed.
		changed := s.changedChan()
		j, err := s.st.GetJob(ctx, jobID)
		if err != nil {
			return app.Job{}, err
		}
		if j.Rev > afterRev || timeout <= 0 {
			return s.ToDTO(j), nil
		}
		select {
		case <-changed:
		case <-deadline:
			return s.ToDTO(j), nil
		case <-s.quit:
			return s.ToDTO(j), nil
		case <-ctx.Done():
			return app.Job{}, ctx.Err()
		}
	}
}

// Stop shuts the scheduler down (§5.4): no new work is accepted (NewJob,
// Cancel and Retry return ENGINE_UNAVAILABLE), the context of every running
// stage is cancelled, and Stop waits up to Options.StopTimeout for them to
// return. With force the ForceKill hook is called for every job with
// running stages first; otherwise only for those still running when the
// wait is over. Interrupted stages go back to pending and their jobs become
// interrupted, so store.Recover(autoResume) + a new scheduler resume them;
// queued jobs stay queued. Stop returns after the last notification was
// delivered. If ctx ends first the wait is cut short the same way as by the
// timeout. Stop is idempotent; a stopped scheduler cannot be restarted.
func (s *Scheduler) Stop(ctx context.Context, force bool) error {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		if s.loopDone != nil {
			<-s.notify.done
		}
		return nil
	}
	s.stopping = true
	var waits []chan struct{}
	for _, rj := range s.active {
		if rj.timer != nil {
			rj.timer.Stop()
		}
		if len(rj.running) == 0 {
			continue
		}
		if force && s.opts.ForceKill != nil {
			s.opts.ForceKill(rj.id)
		}
		for _, rs := range rj.running {
			waits = append(waits, rs.done)
		}
		rj.ask()
	}
	s.mu.Unlock()

	timer := time.NewTimer(s.opts.StopTimeout)
	defer timer.Stop()
wait:
	for _, done := range waits {
		select {
		case <-done:
		case <-timer.C:
			break wait
		case <-ctx.Done():
			break wait
		}
	}

	s.mu.Lock()
	for _, rj := range s.active {
		if len(rj.running) == 0 {
			continue
		}
		s.log.Warn("scheduler: stage did not stop in time, killing child processes", "job_id", rj.id)
		data := map[string]any{"forced": true, "reason": "engine_stopping"}
		switch {
		case rj.cancelling:
			s.abandon(rj, app.StageCancelled, app.JobCancelled, nil, data)
		case rj.failErr != nil:
			s.abandon(rj, app.StageCancelled, app.JobFailed, rj.failErr, data)
		default:
			s.abandon(rj, app.StagePending, app.JobInterrupted, nil, data)
		}
	}
	// Jobs that were between stages (e.g. waiting for a resource) are
	// running in the database without a running stage.
	if jobs, err := s.st.JobsByStatus(context.Background(), app.JobRunning, app.JobCancelling); err != nil {
		s.log.Error("scheduler: cannot list jobs while stopping", "error", err)
	} else {
		for _, j := range jobs {
			status := app.JobInterrupted
			if j.Status == app.JobCancelling {
				status = app.JobCancelled
			}
			s.finish(&runJob{id: j.ID}, status, nil, map[string]any{"reason": "engine_stopping"})
		}
	}
	started := s.loopDone != nil
	s.mu.Unlock()

	s.quitOnce.Do(func() { close(s.quit) })
	if started {
		<-s.loopDone
		s.notify.close()
		<-s.notify.done
	}
	return nil
}
