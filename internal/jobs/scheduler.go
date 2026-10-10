package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"watchink/internal/app"
	"watchink/internal/store"
)

// Scheduler runs jobs. Create it with New; all methods are safe for
// concurrent use.
type Scheduler struct {
	st   *store.Store
	opts Options
	log  *slog.Logger
	sems map[string]*semaphore

	regMu    sync.RWMutex
	builders map[string]Builder

	// mu guards the scheduling state below and serializes every state
	// machine transition (schedule, stage completion, cancel, retry, stop).
	// Lock order: mu → runStage.pmu → applyMu.
	mu       sync.Mutex
	active   map[string]*runJob // jobs the scheduler holds handles for
	started  bool
	stopping bool

	// applyMu makes "write the job, then queue its notification" atomic so
	// events leave in rev order. lastRev remembers the newest rev queued per
	// live job, which lets emit drop anything older or equal.
	applyMu sync.Mutex
	lastRev map[string]int64

	// changed is closed and replaced on every job change (Wait).
	bmu     sync.Mutex
	changed chan struct{}

	wake     chan struct{}
	quit     chan struct{}
	quitOnce sync.Once
	loopDone chan struct{}
	notify   *notifier
	baseCtx  context.Context
}

// runJob is the in-memory side of a job that is being scheduled.
type runJob struct {
	id         string
	defs       []StageDef
	running    map[string]*runStage
	cancelling bool       // Cancel was accepted; waiting for stages to stop
	failErr    *app.Error // a stage failed; waiting for siblings to stop
	timer      *time.Timer
}

// runStage is one running stage goroutine.
type runStage struct {
	s       *Scheduler
	rj      *runJob
	def     *StageDef
	cancel  context.CancelFunc
	release func()
	done    chan struct{}
	// asked: the scheduler cancelled the stage context (cancel, sibling
	// failure or stop), so an error return is not a failure. Guarded by mu.
	asked bool
	// detached: the scheduler gave up on the stage (forced cancel / stop
	// timeout) and already wrote its final state. Guarded by mu.
	detached bool

	// Progress throttle state.
	pmu      sync.Mutex
	pClosed  bool // no more writes on behalf of this stage
	pLast    time.Time
	pPending *float64
	pTimer   *time.Timer
}

// New returns a stopped scheduler; Register the job kinds, then Start it.
func New(opts Options) *Scheduler {
	if opts.Store == nil {
		panic("jobs.New: Options.Store is required")
	}
	if opts.CancelTimeout <= 0 {
		opts.CancelTimeout = DefaultCancelTimeout
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = DefaultStopTimeout
	}
	if opts.ProgressInterval <= 0 {
		opts.ProgressInterval = DefaultProgressInterval
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	limits := DefaultLimits()
	for k, v := range opts.Limits {
		limits[k] = v
	}
	s := &Scheduler{
		st: opts.Store, opts: opts, log: log,
		sems:     map[string]*semaphore{},
		builders: map[string]Builder{},
		active:   map[string]*runJob{},
		lastRev:  map[string]int64{},
		changed:  make(chan struct{}),
		wake:     make(chan struct{}, 1),
		quit:     make(chan struct{}),
		notify:   newNotifier(),
		baseCtx:  context.Background(),
	}
	for name, n := range limits {
		if n > 0 {
			s.sems[name] = &semaphore{ch: make(chan struct{}, n)}
		}
	}
	return s
}

// Start begins scheduling. Run store.Recover first: Start does
// not repair states left by a crashed engine. Values of ctx are passed on to
// the stages; its cancellation is ignored — use Stop.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopping {
		return app.E(app.CodeInternal, "调度器已经启动或已停止")
	}
	s.started = true
	s.baseCtx = context.WithoutCancel(ctx)
	s.loopDone = make(chan struct{})
	go s.loop()
	go s.notify.run(s.deliver)
	s.Wake()
	return nil
}

// Wake asks the scheduler to look for runnable stages. It never blocks and
// may be called at any time.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) loop() {
	defer close(s.loopDone)
	for {
		select {
		case <-s.quit:
			return
		case <-s.wake:
			s.schedule()
		}
	}
}

func (s *Scheduler) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// Acquire blocks until a slot of the resource is free (or ctx is done) and
// returns the function that gives it back; release is idempotent. Unlimited
// resources return at once. Stages of resource "llm" use it to gate each
// model request on the global limit.
func (s *Scheduler) Acquire(ctx context.Context, resource string) (release func(), err error) {
	sem := s.sems[resource]
	if sem == nil {
		return func() {}, nil
	}
	select {
	case sem.ch <- struct{}{}:
	default:
		select {
		case sem.ch <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.releaser(resource, sem), nil
}

func (s *Scheduler) releaser(resource string, sem *semaphore) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			<-sem.ch
			// A pending stage may be waiting for this slot. llm slots never
			// gate stages, so their (frequent) releases need no wake-up.
			if resource != ResourceLLM {
				s.Wake()
			}
		})
	}
}

// tryAcquire takes the slot a stage needs without blocking.
func (s *Scheduler) tryAcquire(resource string) (release func(), ok bool) {
	sem := s.sems[resource]
	if sem == nil || resource == ResourceLLM {
		return func() {}, true
	}
	select {
	case sem.ch <- struct{}{}:
		return s.releaser(resource, sem), true
	default:
		return nil, false
	}
}

type semaphore struct{ ch chan struct{} }

// apply writes one job change and queues its notification. It uses a
// background context: a state transition must not be lost because the
// request or stage that triggered it was cancelled.
func (s *Scheduler) apply(jobID string, u store.JobUpdate) (*store.JobWithStages, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	snap, err := s.st.UpdateJob(context.Background(), jobID, u)
	if err != nil {
		return nil, err
	}
	s.emit(snap)
	return snap, nil
}

// emit wakes long-pollers and queues the job.updated event. Callers hold
// applyMu.
func (s *Scheduler) emit(snap *store.JobWithStages) {
	s.bmu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.bmu.Unlock()
	if snap.Rev <= s.lastRev[snap.ID] {
		return // already published (Announce racing with the scheduler)
	}
	switch snap.Status {
	case app.JobQueued, app.JobRunning, app.JobCancelling:
		s.lastRev[snap.ID] = snap.Rev
	default:
		// Finished jobs change again only through Retry, with a higher
		// rev; forgetting them keeps the map bounded by the live jobs.
		delete(s.lastRev, snap.ID)
	}
	s.notify.push(s.ToDTO(snap))
}

func (s *Scheduler) changedChan() <-chan struct{} {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	return s.changed
}

func (s *Scheduler) deliver(job app.Job) {
	if s.opts.Publisher != nil {
		s.opts.Publisher.Publish(app.Event{Type: app.EventJobUpdated, Job: &job})
	}
	if s.opts.OnJobChanged != nil {
		s.opts.OnJobChanged(job)
	}
}

// schedule starts every stage that can run now: jobs in creation order,
// stages in definition order, bounded by the resource semaphores.
func (s *Scheduler) schedule() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.stopping {
		return
	}
	jobs, err := s.st.JobsByStatus(s.baseCtx, app.JobQueued, app.JobRunning)
	if err != nil {
		s.log.Error("scheduler: cannot list jobs", "error", err)
		time.AfterFunc(time.Second, s.Wake)
		return
	}
	for _, j := range jobs {
		s.advance(j)
	}
}

// advance starts the runnable stages of one job. Callers hold mu.
func (s *Scheduler) advance(j *store.JobWithStages) {
	rj := s.active[j.ID]
	if rj == nil {
		defs, err := s.defsFor(&j.Job)
		if err != nil {
			s.failJob(j.ID, app.E(app.CodeInternal, "无法确定任务的阶段：%v", err))
			return
		}
		rj = &runJob{id: j.ID, defs: defs, running: map[string]*runStage{}}
		s.active[j.ID] = rj
	}
	if rj.cancelling || rj.failErr != nil {
		return // winding down; settle finishes the job
	}
	done := true
	for i := range rj.defs {
		row := j.Stage(rj.defs[i].Name)
		if row == nil {
			s.failJob(j.ID, app.E(app.CodeInternal, "任务缺少阶段 %s 的记录（程序版本已变化）", rj.defs[i].Name).
				WithHint("请重新导入该视频"))
			return
		}
		if row.Status != app.StageSucceeded {
			done = false
		}
	}
	if done {
		if len(rj.running) == 0 {
			s.finish(rj, app.JobSucceeded, nil, nil)
		}
		return
	}
	status := j.Status
	for i := range rj.defs {
		def := &rj.defs[i]
		row := j.Stage(def.Name)
		// Anything not succeeded and not running in this process is
		// runnable. That includes rows a crash left as failed / cancelled
		// under a resumed job.
		if row.Status == app.StageSucceeded || rj.running[def.Name] != nil {
			continue
		}
		ready := true
		for _, dep := range def.Deps {
			if d := j.Stage(dep); d == nil || d.Status != app.StageSucceeded {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		release, ok := s.tryAcquire(def.Resource)
		if !ok {
			continue
		}
		if s.startStage(rj, j, def, row, status, release) {
			status = app.JobRunning
		}
	}
}

// startStage marks the stage (and, for the first one, the job) running and
// launches its goroutine. Callers hold mu.
func (s *Scheduler) startStage(rj *runJob, j *store.JobWithStages, def *StageDef, row *store.JobStage, status string, release func()) bool {
	now := app.Now()
	attempt := row.Attempt + 1
	u := store.JobUpdate{
		IfStatus: []string{app.JobQueued, app.JobRunning},
		Stages: []store.StagePatch{{Stage: def.Name, Status: ptr(app.StageRunning), Progress: ptr(0.0),
			Attempt: &attempt, ClearError: true, Reused: ptr(false), StartedAt: &now, FinishedAt: ptr("")}},
		Event: &store.EventInput{Type: "stage.started", Data: map[string]any{"stage": def.Name, "attempt": attempt}},
	}
	if status == app.JobQueued {
		u.Job.Status = ptr(app.JobRunning)
		if j.StartedAt == "" {
			u.Job.StartedAt = &now
		}
	}
	snap, err := s.apply(j.ID, u)
	if err != nil {
		release()
		s.log.Error("scheduler: cannot start stage", "job_id", j.ID, "stage", def.Name, "error", err)
		return false
	}
	ctx, cancel := context.WithCancel(s.baseCtx)
	rs := &runStage{s: s, rj: rj, def: def, cancel: cancel, release: release, done: make(chan struct{})}
	rj.running[def.Name] = rs
	sc := &StageContext{
		Job:          snap.Job,
		Stage:        def.Name,
		Attempt:      snap.Attempt,
		StageAttempt: attempt,
		IgnoreCache:  ignoreCache(def, snap.Options),
		Logger:       s.log.With("job_id", j.ID, "stage", def.Name),
		rs:           rs,
		snap:         snap,
	}
	go rs.run(ctx, sc)
	return true
}

func (rs *runStage) run(ctx context.Context, sc *StageContext) {
	defer close(rs.done)
	res, err := safeRun(ctx, rs.def, sc)
	rs.s.stageDone(rs, res, err)
}

// safeRun turns a panic in a stage into an INTERNAL error.
func safeRun(ctx context.Context, def *StageDef, sc *StageContext) (res *StageResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			sc.Logger.Error("stage panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			res, err = nil, app.E(app.CodeInternal, "阶段 %s 发生内部错误：%v", def.Name, r)
		}
	}()
	return def.Run(ctx, sc)
}

// stageError tags err with the stage; non-app errors become INTERNAL. The
// error is copied because stages may return shared values.
func stageError(err error, stage string) *app.Error {
	e := *app.AsError(err)
	e.Stage = stage
	return &e
}

// stageDone records the outcome of a stage goroutine and moves the job on.
func (s *Scheduler) stageDone(rs *runStage, res *StageResult, err error) {
	rs.closeProgress()
	s.mu.Lock()
	defer s.mu.Unlock()
	rs.cancel()
	rs.release()
	if rs.detached {
		return // its final state was written when the scheduler gave up on it
	}
	rj, name := rs.rj, rs.def.Name
	delete(rj.running, name)
	now := app.Now()
	var u store.JobUpdate
	switch {
	case err == nil:
		if res == nil {
			res = &StageResult{}
		}
		u.Stages = []store.StagePatch{{Stage: name, Status: ptr(app.StageSucceeded), Progress: ptr(1.0),
			ArtifactID: &res.ArtifactID, Fingerprint: &res.Fingerprint, Reused: &res.Reused, FinishedAt: &now}}
		u.Event = &store.EventInput{Type: "stage.succeeded", Data: map[string]any{"stage": name, "reused": res.Reused}}
	case rs.asked && s.stopping && !rj.cancelling && rj.failErr == nil:
		// Interrupted by shutdown: back to pending so the next engine run
		// resumes it, exactly like the crash recovery.
		u.Stages = []store.StagePatch{{Stage: name, Status: ptr(app.StagePending), Progress: ptr(0.0),
			StartedAt: ptr(""), FinishedAt: ptr("")}}
		u.Event = &store.EventInput{Type: "stage.interrupted", Data: map[string]any{"stage": name}}
	case rs.asked:
		u.Stages = []store.StagePatch{{Stage: name, Status: ptr(app.StageCancelled), FinishedAt: &now}}
		u.Event = &store.EventInput{Type: "stage.cancelled", Data: map[string]any{"stage": name}}
	default:
		ae := stageError(err, name)
		u.Stages = []store.StagePatch{{Stage: name, Status: ptr(app.StageFailed), Error: ae, FinishedAt: &now}}
		u.Job.Error = ae
		u.Event = &store.EventInput{Type: "stage.failed", Data: map[string]any{"stage": name, "code": ae.Code}}
		// no further stages start, and the siblings are asked to stop;
		// the job turns failed once they have (settle).
		rj.failErr = ae
		rj.ask()
	}
	if _, aerr := s.apply(rj.id, u); aerr != nil {
		s.log.Error("scheduler: cannot record stage result", "job_id", rj.id, "stage", name, "error", aerr)
	}
	s.settle(rj)
	s.Wake()
}

// ask cancels the context of every running stage of the job.
func (rj *runJob) ask() {
	for _, rs := range rj.running {
		rs.asked = true
		rs.cancel()
	}
}

// settle finishes the job when nothing of it is running any more. Callers
// hold mu.
func (s *Scheduler) settle(rj *runJob) {
	if len(rj.running) > 0 || s.active[rj.id] != rj {
		return
	}
	switch {
	case rj.cancelling:
		s.finish(rj, app.JobCancelled, nil, map[string]any{"forced": false})
	case rj.failErr != nil:
		s.finish(rj, app.JobFailed, rj.failErr, nil)
	default:
		j, err := s.st.GetJob(context.Background(), rj.id)
		if err != nil {
			s.log.Error("scheduler: cannot read job", "job_id", rj.id, "error", err)
			return
		}
		all := true
		for i := range j.Stages {
			all = all && j.Stages[i].Status == app.StageSucceeded
		}
		switch {
		case all:
			s.finish(rj, app.JobSucceeded, nil, nil)
		case s.stopping && j.Status == app.JobRunning:
			s.finish(rj, app.JobInterrupted, nil, map[string]any{"reason": "engine_stopping"})
		}
	}
}

// finish writes a final job status and forgets the job. Callers hold mu.
func (s *Scheduler) finish(rj *runJob, status string, jobErr *app.Error, data map[string]any) {
	if rj.timer != nil {
		rj.timer.Stop()
	}
	delete(s.active, rj.id)
	now := app.Now()
	u := store.JobUpdate{
		Job:   store.JobPatch{Status: &status, FinishedAt: &now, Error: jobErr},
		Event: &store.EventInput{Type: "job." + status, Data: data},
	}
	if _, err := s.apply(rj.id, u); err != nil {
		s.log.Error("scheduler: cannot finish job", "job_id", rj.id, "status", status, "error", err)
	}
}

// failJob fails a job the scheduler cannot run at all. Callers hold mu.
func (s *Scheduler) failJob(jobID string, e *app.Error) {
	rj := s.active[jobID]
	if rj == nil {
		rj = &runJob{id: jobID, running: map[string]*runStage{}}
	}
	if len(rj.running) > 0 {
		rj.failErr = e
		rj.ask()
		return
	}
	s.finish(rj, app.JobFailed, e, nil)
}

// notifier delivers job changes in order from one goroutine, so callbacks
// never run under a scheduler lock.
type notifier struct {
	mu     sync.Mutex
	queue  []app.Job
	closed bool
	sig    chan struct{}
	done   chan struct{}
}

func newNotifier() *notifier {
	return &notifier{sig: make(chan struct{}, 1), done: make(chan struct{})}
}

func (n *notifier) push(j app.Job) {
	n.mu.Lock()
	if !n.closed {
		n.queue = append(n.queue, j)
	}
	n.mu.Unlock()
	select {
	case n.sig <- struct{}{}:
	default:
	}
}

// close stops accepting changes; run exits after delivering what is queued.
func (n *notifier) close() {
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()
	select {
	case n.sig <- struct{}{}:
	default:
	}
}

func (n *notifier) run(deliver func(app.Job)) {
	defer close(n.done)
	for {
		n.mu.Lock()
		batch, closed := n.queue, n.closed
		n.queue = nil
		n.mu.Unlock()
		for _, j := range batch {
			deliver(j)
		}
		if len(batch) > 0 {
			continue
		}
		if closed {
			return
		}
		<-n.sig
	}
}
