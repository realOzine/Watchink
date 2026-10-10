package jobs

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"watchink/internal/app"
	"watchink/internal/store"
)

const testKind = "test_kind"

// recorder is a Publisher that keeps every job.updated event.
type recorder struct {
	mu     sync.Mutex
	nextID int64
	events []app.Job
}

func (r *recorder) Publish(ev app.Event) app.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	ev.ID = r.nextID
	if ev.Type == app.EventJobUpdated && ev.Job != nil {
		r.events = append(r.events, *ev.Job)
	}
	return ev
}

func (r *recorder) revs(jobID string) []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []int64
	for _, j := range r.events {
		if j.JobID == jobID {
			out = append(out, j.Rev)
		}
	}
	return out
}

type harness struct {
	t   *testing.T
	st  *store.Store
	s   *Scheduler
	rec *recorder
	vid string
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newHarness opens a store and a scheduler; the scheduler is not started.
func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	st := openStore(t, t.TempDir())
	return attach(t, st, opts)
}

func attach(t *testing.T, st *store.Store, opts Options) *harness {
	t.Helper()
	h := &harness{t: t, st: st, rec: &recorder{}}
	opts.Store = st
	if opts.Publisher == nil {
		opts.Publisher = h.rec
	}
	if opts.ProgressInterval == 0 {
		opts.ProgressInterval = 5 * time.Millisecond
	}
	h.s = New(opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.s.Stop(ctx, true)
	})
	ctx := context.Background()
	if v, _ := st.FindVideoBySourceKey(ctx, "file:1:test"); v != nil {
		h.vid = v.ID
	} else {
		v := &store.Video{SourceType: app.SourceFile, SourceKey: "file:1:test", Title: "t"}
		if err := st.CreateVideo(ctx, v); err != nil {
			t.Fatal(err)
		}
		h.vid = v.ID
	}
	return h
}

func (h *harness) start() {
	h.t.Helper()
	if err := h.s.Start(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) submit(kind string, options any) app.Job {
	h.t.Helper()
	j, err := h.s.Submit(context.Background(), JobParams{Kind: kind, VideoID: h.vid, Options: options, RequestedBy: "cli"})
	if err != nil {
		h.t.Fatal(err)
	}
	return j
}

// waitFor polls the job until cond holds.
func (h *harness) waitFor(jobID string, what string, cond func(app.Job) bool) app.Job {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var rev int64
	for {
		j, err := h.s.Wait(context.Background(), jobID, rev, 200*time.Millisecond)
		if err != nil {
			h.t.Fatalf("Wait: %v", err)
		}
		if cond(j) {
			return j
		}
		rev = j.Rev
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; job = %s", what, describe(j))
		}
	}
}

func (h *harness) waitStatus(jobID, status string) app.Job {
	h.t.Helper()
	return h.waitFor(jobID, "status "+status, func(j app.Job) bool { return j.Status == status })
}

func describe(j app.Job) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s rev=%d stage=%q err=%v [", j.Status, j.Rev, j.Stage, j.Error)
	for _, st := range j.Stages {
		fmt.Fprintf(&sb, " %s:%s", st.Stage, st.Status)
	}
	return sb.String() + " ]"
}

func stageOf(t *testing.T, j app.Job, name string) app.JobStage {
	t.Helper()
	for _, st := range j.Stages {
		if st.Stage == name {
			return st
		}
	}
	t.Fatalf("job has no stage %s: %s", name, describe(j))
	return app.JobStage{}
}

func ok(context.Context, *StageContext) (*StageResult, error) { return nil, nil }

// block returns a stage that signals started and then waits for release or
// its context.
func block(started chan<- string, release <-chan struct{}) StageFunc {
	return func(ctx context.Context, sc *StageContext) (*StageResult, error) {
		if started != nil {
			started <- sc.Stage
		}
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func fixed(defs ...StageDef) Builder {
	return func(*store.Job) ([]StageDef, error) { return defs, nil }
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func TestHappyPathWithParallelBranch(t *testing.T) {
	h := newHarness(t, Options{})
	var changed atomic.Int64
	h2 := attach(t, h.st, Options{OnJobChanged: func(app.Job) { changed.Add(1) }})
	_ = h
	h = h2

	// extract and playback both depend on probe and must overlap: each
	// waits until the other one has started.
	extractUp, playbackUp := make(chan struct{}), make(chan struct{})
	meet := func(mine, other chan struct{}) StageFunc {
		return func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			close(mine)
			select {
			case <-other:
				return &StageResult{ArtifactID: "art_" + sc.Stage, Fingerprint: "fp_" + sc.Stage}, nil
			case <-time.After(5 * time.Second):
				return nil, errors.New("sibling stage never started: stages do not run in parallel")
			}
		}
	}
	var noteDep DepResult
	var noteDepOK, missingDepOK bool
	h.s.Register(testKind, fixed(
		StageDef{Name: "probe", Weight: 1, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			return &StageResult{ArtifactID: "art_probe", Fingerprint: "fp_probe", Reused: true}, nil
		}},
		StageDef{Name: "extract", Deps: []string{"probe"}, Resource: ResourceMedia, Weight: 4, Run: meet(extractUp, playbackUp)},
		StageDef{Name: "playback", Deps: []string{"probe"}, Resource: ResourceIO, ExcludeFromProgress: true, Run: meet(playbackUp, extractUp)},
		StageDef{Name: "note", Deps: []string{"extract"}, Resource: ResourceLLM, Weight: 5, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			noteDep, noteDepOK = sc.Dep("probe")
			_, missingDepOK = sc.Dep("nope")
			var o struct {
				Language string `json:"language"`
			}
			if err := sc.DecodeOptions(&o); err != nil || o.Language != "zh" {
				return nil, fmt.Errorf("options = %+v, %v", o, err)
			}
			if sc.JobID() == "" || sc.VideoID() == "" || sc.Attempt != 1 || sc.StageAttempt != 1 || sc.IgnoreCache {
				return nil, fmt.Errorf("stage context = %+v", sc)
			}
			err := sc.SetResult(func(r *app.JobResult) { r.NoteID = ptr("note_1") })
			return nil, err
		}},
	))
	h.start()
	created := h.submit(testKind, map[string]any{"language": "zh"})
	if created.Status != app.JobQueued || created.Rev != 1 || created.Stage != "probe" || created.Progress != 0 || len(created.Stages) != 4 {
		t.Fatalf("created job = %s", describe(created))
	}
	j := h.waitStatus(created.JobID, app.JobSucceeded)
	if j.Progress != 1 || j.Stage != "" || j.Error != nil || j.FinishedAt == nil || j.StartedAt == nil ||
		j.Result.NoteID == nil || *j.Result.NoteID != "note_1" || j.Result.TranscriptID != nil {
		t.Fatalf("finished job = %+v", j)
	}
	for _, st := range j.Stages {
		if st.Status != app.StageSucceeded || st.Progress != 1 || st.Attempt != 1 || st.StartedAt == nil || st.FinishedAt == nil {
			t.Fatalf("stage %+v", st)
		}
	}
	if !stageOf(t, j, "probe").Reused || stageOf(t, j, "extract").Reused {
		t.Fatal("reused flags")
	}
	if !noteDepOK || noteDep.ArtifactID != "art_probe" || noteDep.Fingerprint != "fp_probe" || !noteDep.Reused || missingDepOK {
		t.Fatalf("Dep(probe) = %+v, %v; Dep(nope) ok = %v", noteDep, noteDepOK, missingDepOK)
	}
	row, _ := h.st.GetJob(context.Background(), j.JobID)
	if row.Stage("extract").ArtifactID != "art_extract" || row.Stage("extract").Fingerprint != "fp_extract" {
		t.Fatalf("stage row = %+v", row.Stage("extract"))
	}

	// Stop delivers every pending notification before it returns.
	if err := h.s.Stop(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	revs := h.rec.revs(j.JobID)
	// The first event is rev 1 unless the scheduler picked the job up before
	// Announce ran, in which case it starts at the stage start (rev 2).
	if len(revs) == 0 || revs[0] > 2 || revs[len(revs)-1] != j.Rev {
		t.Fatalf("published revs = %v, final rev %d", revs, j.Rev)
	}
	for i := 1; i < len(revs); i++ {
		if revs[i] != revs[i-1]+1 {
			t.Fatalf("published revs are not strictly increasing by one: %v", revs)
		}
	}
	if int(changed.Load()) != len(revs) {
		t.Fatalf("OnJobChanged called %d times for %d events", changed.Load(), len(revs))
	}
	// Every state change left an event row.
	evs, _ := h.st.ListJobEvents(context.Background(), j.JobID)
	var types []string
	for _, e := range evs {
		types = append(types, e.Type)
	}
	for _, want := range []string{"job.created", "stage.started", "stage.succeeded", "job.result", "job.succeeded"} {
		if !slices.Contains(types, want) {
			t.Errorf("no %s event in %v", want, types)
		}
	}
	if n, _ := h.s.ActiveCount(context.Background()); n != 0 {
		t.Fatalf("ActiveCount = %d", n)
	}
}

func TestResourceLimits(t *testing.T) {
	h := newHarness(t, Options{Limits: map[string]int{"custom": 3}})
	type gauge struct{ cur, max atomic.Int64 }
	gauges := map[string]*gauge{}
	for _, r := range []string{ResourceNet, ResourceMedia, ResourceASR, "custom", ResourceIO, "llm-stage", "llm-request"} {
		gauges[r] = &gauge{}
	}
	track := func(g *gauge) func() {
		n := g.cur.Add(1)
		for {
			m := g.max.Load()
			if n <= m || g.max.CompareAndSwap(m, n) {
				break
			}
		}
		return func() { g.cur.Add(-1) }
	}
	// Unlimited stages wait for each other, proving they are not serialized.
	const jobs = 8
	var ioUp sync.WaitGroup
	ioUp.Add(jobs)
	ioAll := make(chan struct{})
	go func() { ioUp.Wait(); close(ioAll) }()
	var llmUp sync.WaitGroup
	llmUp.Add(jobs)
	llmAll := make(chan struct{})
	go func() { llmUp.Wait(); close(llmAll) }()

	busy := func(resource string) StageFunc {
		return func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			defer track(gauges[resource])()
			time.Sleep(3 * time.Millisecond)
			return nil, nil
		}
	}
	h.s.Register(testKind, fixed(
		StageDef{Name: "io", Resource: ResourceIO, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			defer track(gauges[ResourceIO])()
			ioUp.Done()
			select {
			case <-ioAll:
				return nil, nil
			case <-time.After(5 * time.Second):
				return nil, errors.New("unlimited stages were not all running together")
			}
		}},
		StageDef{Name: "net", Resource: ResourceNet, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			// Wait until the limit has been reached once, so the test shows
			// that two net stages really do run together.
			defer track(gauges[ResourceNet])()
			for deadline := time.Now().Add(5 * time.Second); gauges[ResourceNet].max.Load() < 2; {
				if time.Now().After(deadline) {
					return nil, errors.New("net never reached its limit of 2")
				}
				time.Sleep(time.Millisecond)
			}
			return nil, nil
		}},
		StageDef{Name: "media", Resource: ResourceMedia, Run: busy(ResourceMedia)},
		StageDef{Name: "asr", Resource: ResourceASR, Run: busy(ResourceASR)},
		StageDef{Name: "custom", Resource: "custom", Run: busy("custom")},
		StageDef{Name: "llm", Resource: ResourceLLM, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			// The stage itself is not gated by llm: all of them run at once...
			defer track(gauges["llm-stage"])()
			llmUp.Done()
			select {
			case <-llmAll:
			case <-time.After(5 * time.Second):
				return nil, errors.New("llm stages were gated as a whole")
			}
			// ...while individual requests share the global limit of 4.
			var wg sync.WaitGroup
			var failed atomic.Bool
			for range 3 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					release, err := sc.Acquire(ctx, ResourceLLM)
					if err != nil {
						failed.Store(true)
						return
					}
					done := track(gauges["llm-request"])
					time.Sleep(2 * time.Millisecond)
					done()
					release()
					release() // idempotent
				}()
			}
			wg.Wait()
			if failed.Load() {
				return nil, errors.New("Acquire failed")
			}
			return nil, nil
		}},
	))
	h.start()
	var ids []string
	for range jobs {
		ids = append(ids, h.submit(testKind, nil).JobID)
	}
	for _, id := range ids {
		if j := h.waitFor(id, "terminal", func(j app.Job) bool { return j.Terminal() }); j.Status != app.JobSucceeded {
			t.Fatalf("job = %s", describe(j))
		}
	}
	for res, want := range map[string]int64{ResourceNet: 2, ResourceMedia: 1, ResourceASR: 1, "custom": 3, "llm-request": 4} {
		if got := gauges[res].max.Load(); got > want || got < 1 {
			t.Errorf("max concurrency of %s = %d, limit %d", res, got, want)
		}
	}
	if got := gauges[ResourceIO].max.Load(); got != jobs {
		t.Errorf("unlimited resource max = %d, want %d", got, jobs)
	}
	if got := gauges["llm-stage"].max.Load(); got != jobs {
		t.Errorf("llm stages max = %d, want %d", got, jobs)
	}

	// Acquire honours its context when the resource is exhausted.
	rel, err := h.s.Acquire(context.Background(), ResourceMedia)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.s.Acquire(ctx, ResourceMedia); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire on a full resource = %v", err)
	}
	rel()
	if rel2, err := h.s.Acquire(context.Background(), ResourceMedia); err != nil {
		t.Fatal(err)
	} else {
		rel2()
	}
}

func TestLimitOverrideUnlimited(t *testing.T) {
	h := newHarness(t, Options{Limits: map[string]int{ResourceASR: 0}})
	var up sync.WaitGroup
	up.Add(3)
	all := make(chan struct{})
	go func() { up.Wait(); close(all) }()
	h.s.Register(testKind, fixed(StageDef{Name: "asr", Resource: ResourceASR, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
		up.Done()
		select {
		case <-all:
			return nil, nil
		case <-time.After(5 * time.Second):
			return nil, errors.New("limit 0 did not make the resource unlimited")
		}
	}}))
	h.start()
	for _, id := range []string{h.submit(testKind, nil).JobID, h.submit(testKind, nil).JobID, h.submit(testKind, nil).JobID} {
		h.waitStatus(id, app.JobSucceeded)
	}
}

func TestCreationOrder(t *testing.T) {
	h := newHarness(t, Options{})
	var mu sync.Mutex
	var order []string
	h.s.Register(testKind, fixed(
		StageDef{Name: "asr", Resource: ResourceASR, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			mu.Lock()
			order = append(order, sc.JobID())
			mu.Unlock()
			time.Sleep(time.Millisecond)
			return nil, nil
		}},
		StageDef{Name: "after", Deps: []string{"asr"}, Run: ok},
	))
	// Queue everything first, so the order is decided by the scheduler and
	// not by submission timing.
	var ids []string
	for range 6 {
		ids = append(ids, h.submit(testKind, nil).JobID)
	}
	h.start()
	for _, id := range ids {
		h.waitStatus(id, app.JobSucceeded)
	}
	if !slices.Equal(order, ids) {
		t.Fatalf("run order %v, creation order %v", order, ids)
	}
}

func TestStageFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fail     func() error
		wantCode app.Code
		wantMsg  string
	}{
		{"app error", func() error {
			return app.E(app.CodeASRFailed, "转写失败").WithHint("重试").WithDetail("exit_code", 3)
		}, app.CodeASRFailed, "转写失败"},
		{"plain error", func() error { return errors.New("disk on fire") }, app.CodeInternal, "disk on fire"},
		{"panic", func() error { panic("boom") }, app.CodeInternal, "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Options{})
			started := make(chan string, 2)
			var siblingErr atomic.Value
			var laterRan atomic.Bool
			goFail := make(chan struct{})
			h.s.Register(testKind, fixed(
				StageDef{Name: "first", Run: ok},
				StageDef{Name: "bad", Deps: []string{"first"}, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
					started <- sc.Stage
					<-goFail
					return nil, tc.fail()
				}},
				StageDef{Name: "sibling", Deps: []string{"first"}, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
					started <- sc.Stage
					<-ctx.Done() // asked to stop because "bad" failed
					siblingErr.Store(ctx.Err())
					return nil, ctx.Err()
				}},
				StageDef{Name: "later", Deps: []string{"bad", "sibling"}, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
					laterRan.Store(true)
					return nil, nil
				}},
			))
			h.start()
			id := h.submit(testKind, nil).JobID
			recv(t, started, "bad or sibling")
			recv(t, started, "bad or sibling")
			close(goFail)
			j := h.waitStatus(id, app.JobFailed)
			if j.Error == nil || j.Error.Code != tc.wantCode || j.Error.Stage != "bad" || !strings.Contains(j.Error.Message, tc.wantMsg) {
				t.Fatalf("job error = %+v", j.Error)
			}
			if tc.wantCode == app.CodeASRFailed && (j.Error.Hint != "重试" || j.Error.Details["exit_code"] != float64(3) || !j.Error.Retryable) {
				t.Fatalf("error fields lost: %+v", j.Error)
			}
			bad := stageOf(t, j, "bad")
			if bad.Status != app.StageFailed || bad.Error == nil || bad.Error.Code != tc.wantCode || bad.Error.Stage != "bad" {
				t.Fatalf("failed stage = %+v", bad)
			}
			if st := stageOf(t, j, "sibling"); st.Status != app.StageCancelled || st.Error != nil {
				t.Fatalf("sibling = %+v", st)
			}
			if st := stageOf(t, j, "later"); st.Status != app.StagePending || laterRan.Load() {
				t.Fatalf("later stage ran: %+v", st)
			}
			if stageOf(t, j, "first").Status != app.StageSucceeded || j.FinishedAt == nil || j.Stage != "" {
				t.Fatalf("job = %s", describe(j))
			}
			if siblingErr.Load() == nil {
				t.Fatal("sibling context was not cancelled")
			}
		})
	}
}

func TestSharedErrorValueIsNotMutated(t *testing.T) {
	h := newHarness(t, Options{})
	shared := app.E(app.CodeToolFailed, "x")
	h.s.Register(testKind, fixed(StageDef{Name: "s", Run: func(context.Context, *StageContext) (*StageResult, error) { return nil, shared }}))
	h.start()
	j := h.waitStatus(h.submit(testKind, nil).JobID, app.JobFailed)
	if j.Error.Stage != "s" || shared.Stage != "" {
		t.Fatalf("job error stage %q, shared value stage %q", j.Error.Stage, shared.Stage)
	}
}

func TestRetry(t *testing.T) {
	h := newHarness(t, Options{})
	var runs sync.Map
	count := func(name string) int {
		v, _ := runs.LoadOrStore(name, new(atomic.Int64))
		return int(v.(*atomic.Int64).Load())
	}
	bump := func(name string) {
		v, _ := runs.LoadOrStore(name, new(atomic.Int64))
		v.(*atomic.Int64).Add(1)
	}
	var healthy atomic.Bool
	var attempts sync.Map
	h.s.Register(testKind, fixed(
		StageDef{Name: "a", Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			bump("a")
			return &StageResult{Fingerprint: "fp_a"}, nil
		}},
		StageDef{Name: "b", Deps: []string{"a"}, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			bump("b")
			attempts.Store(fmt.Sprintf("b%d", count("b")), [2]int{sc.Attempt, sc.StageAttempt})
			if !healthy.Load() {
				sc.Progress(0.7)
				return nil, app.E(app.CodeModelNotConfigured, "没有配置在线模型")
			}
			if d, ok := sc.Dep("a"); !ok || d.Fingerprint != "fp_a" {
				return nil, fmt.Errorf("Dep(a) after retry = %+v, %v", d, ok)
			}
			return nil, nil
		}},
		StageDef{Name: "c", Deps: []string{"b"}, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			bump("c")
			return nil, nil
		}},
	))
	h.start()
	ctx := context.Background()
	id := h.submit(testKind, nil).JobID

	failed := h.waitStatus(id, app.JobFailed)
	if failed.Error.Code != app.CodeModelNotConfigured || count("a") != 1 || count("b") != 1 || count("c") != 0 {
		t.Fatalf("first attempt: %s a=%d b=%d c=%d", describe(failed), count("a"), count("b"), count("c"))
	}

	healthy.Store(true)
	retried, err := h.s.Retry(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if retried.JobID != id || retried.Attempt != 2 || retried.Error != nil || retried.FinishedAt != nil ||
		retried.Rev <= failed.Rev || (retried.Status != app.JobQueued && retried.Status != app.JobRunning) {
		t.Fatalf("retried job = %+v", retried)
	}
	done := h.waitStatus(id, app.JobSucceeded)
	if count("a") != 1 || count("b") != 2 || count("c") != 1 {
		t.Fatalf("after retry: a=%d b=%d c=%d (a must be skipped)", count("a"), count("b"), count("c"))
	}
	if done.Attempt != 2 || done.Error != nil || stageOf(t, done, "a").Attempt != 1 || stageOf(t, done, "b").Attempt != 2 ||
		stageOf(t, done, "b").Error != nil || done.Progress != 1 {
		t.Fatalf("done = %+v", done)
	}
	if v, _ := attempts.Load("b2"); v != [2]int{2, 2} {
		t.Fatalf("attempts seen by the stage on retry = %v", v)
	}

	for _, tc := range []struct {
		name string
		id   string
		code app.Code
	}{{"succeeded job", id, app.CodeConflict}, {"unknown job", "job_missing", app.CodeNotFound}} {
		if _, err := h.s.Retry(ctx, tc.id); !app.IsCode(err, tc.code) {
			t.Errorf("Retry of %s = %v, want %s", tc.name, err, tc.code)
		}
	}
}

// A succeeded stage whose artifact is gone is rerun on retry, but only when
// a stage that is going to run needs it (retry skips only stages whose artifact is still valid).
func TestRetryRevivesStagesWithMissingArtifacts(t *testing.T) {
	h := newHarness(t, Options{})
	ctx := context.Background()
	var runs [4]atomic.Int64
	var healthy atomic.Bool
	artifact := func(i int, name string) StageFunc {
		return func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			runs[i].Add(1)
			rel := h.st.VideoDir(sc.VideoID()) + "/" + name
			fi, err := h.st.WriteFileAtomic(rel, []byte(name))
			if err != nil {
				return nil, err
			}
			a, err := h.st.PutArtifact(ctx, &store.Artifact{VideoID: sc.VideoID(), Kind: name, Fingerprint: "fp", Path: rel, SHA256: fi.SHA256, SizeBytes: fi.Size})
			if err != nil {
				return nil, err
			}
			return &StageResult{ArtifactID: a.ID, Fingerprint: "fp"}, nil
		}
	}
	h.s.Register(testKind, fixed(
		StageDef{Name: "audio", Run: artifact(0, "audio")},                      // purged later, needed only by asr
		StageDef{Name: "asr", Deps: []string{"audio"}, Run: artifact(1, "asr")}, // purged later, needed by note
		StageDef{Name: "probe", Run: artifact(2, "probe")},                      // stays valid
		StageDef{Name: "note", Deps: []string{"asr", "probe"}, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			runs[3].Add(1)
			if !healthy.Load() {
				return nil, errors.New("not yet")
			}
			return nil, nil
		}},
	))
	h.start()
	id := h.submit(testKind, nil).JobID
	h.waitStatus(id, app.JobFailed)
	row, _ := h.st.GetJob(ctx, id)

	// Case 1: only audio is purged. Its sole dependent (asr) succeeded and
	// is still valid, so nothing but note reruns.
	if err := h.st.PurgeArtifact(ctx, row.Stage("audio").ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(id, app.JobFailed)
	if got := [4]int64{runs[0].Load(), runs[1].Load(), runs[2].Load(), runs[3].Load()}; got != [4]int64{1, 1, 1, 2} {
		t.Fatalf("runs after retry with a purged but unneeded artifact = %v", got)
	}

	// Case 2: asr's artifact is gone too. note needs asr, and asr in turn
	// needs the purged audio: both are revived; probe is not.
	if err := h.st.PurgeArtifact(ctx, row.Stage("asr").ArtifactID); err != nil {
		t.Fatal(err)
	}
	healthy.Store(true)
	if _, err := h.s.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	j := h.waitStatus(id, app.JobSucceeded)
	if got := [4]int64{runs[0].Load(), runs[1].Load(), runs[2].Load(), runs[3].Load()}; got != [4]int64{2, 2, 1, 3} {
		t.Fatalf("runs after retry with missing needed artifacts = %v", got)
	}
	if j.Attempt != 3 || stageOf(t, j, "probe").Attempt != 1 || stageOf(t, j, "audio").Attempt != 2 {
		t.Fatalf("job = %+v", j)
	}
}

func TestCancelQueued(t *testing.T) {
	h := newHarness(t, Options{})
	started := make(chan string, 4)
	release := make(chan struct{})
	var secondRan atomic.Bool
	h.s.Register(testKind, fixed(StageDef{Name: "asr", Resource: ResourceASR, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
		if sc.Job.Options != nil && strings.Contains(string(sc.Job.Options), "second") {
			secondRan.Store(true)
		}
		return block(started, release)(ctx, sc)
	}}))
	h.start()
	ctx := context.Background()
	first := h.submit(testKind, map[string]any{"n": "first"}).JobID
	recv(t, started, "first job")
	second := h.submit(testKind, map[string]any{"n": "second"}).JobID // queued behind the asr slot

	j, err := h.s.Cancel(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != app.JobCancelled || j.FinishedAt == nil || j.Stage != "" || stageOf(t, j, "asr").Status != app.StagePending {
		t.Fatalf("cancelled queued job = %s", describe(j))
	}
	if _, err := h.s.Cancel(ctx, second); !app.IsCode(err, app.CodeConflict) {
		t.Fatalf("second cancel = %v", err)
	}
	if _, err := h.s.Cancel(ctx, "job_missing"); !app.IsCode(err, app.CodeNotFound) {
		t.Fatalf("cancel of an unknown job = %v", err)
	}
	close(release)
	h.waitStatus(first, app.JobSucceeded)
	if _, err := h.s.Cancel(ctx, first); !app.IsCode(err, app.CodeConflict) {
		t.Fatalf("cancel of a succeeded job = %v", err)
	}
	// The cancelled job never runs, and can be retried later.
	time.Sleep(20 * time.Millisecond)
	if secondRan.Load() {
		t.Fatal("cancelled queued job ran")
	}
	if _, err := h.s.Retry(ctx, second); err != nil {
		t.Fatal(err)
	}
	if j := h.waitStatus(second, app.JobSucceeded); j.Attempt != 2 || !secondRan.Load() {
		t.Fatalf("retried cancelled job = %+v", j)
	}
}

func TestCancelRunning(t *testing.T) {
	var killed atomic.Int64
	h := newHarness(t, Options{ForceKill: func(string) { killed.Add(1) }})
	started := make(chan string, 2)
	never := make(chan struct{})
	var nextRan atomic.Bool
	h.s.Register(testKind, fixed(
		StageDef{Name: "a", Run: block(started, never)},
		StageDef{Name: "b", Run: block(started, never)},
		StageDef{Name: "next", Deps: []string{"a", "b"}, Run: func(context.Context, *StageContext) (*StageResult, error) {
			nextRan.Store(true)
			return nil, nil
		}},
	))
	h.start()
	ctx := context.Background()
	id := h.submit(testKind, nil).JobID
	recv(t, started, "a/b")
	recv(t, started, "a/b")
	j, err := h.s.Cancel(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != app.JobCancelling && j.Status != app.JobCancelled {
		t.Fatalf("Cancel returned %s", describe(j))
	}
	j = h.waitStatus(id, app.JobCancelled)
	if stageOf(t, j, "a").Status != app.StageCancelled || stageOf(t, j, "b").Status != app.StageCancelled ||
		stageOf(t, j, "next").Status != app.StagePending || j.Error != nil || j.FinishedAt == nil || nextRan.Load() {
		t.Fatalf("cancelled job = %s", describe(j))
	}
	if killed.Load() != 0 {
		t.Fatal("ForceKill called for a cooperative cancel")
	}
	evs, _ := h.st.ListJobEvents(ctx, id)
	var sawCancelling bool
	for _, e := range evs {
		sawCancelling = sawCancelling || e.Type == "job.cancelling"
		if e.Type == "job.cancelled" && !strings.Contains(string(e.Data), `"forced":false`) {
			t.Fatalf("cancelled event = %s", e.Data)
		}
	}
	if !sawCancelling {
		t.Fatal("job never went through cancelling")
	}
}

// Cancelling a running job that is between stages (waiting for a resource)
// has nothing to stop and finishes at once.
func TestCancelRunningBetweenStages(t *testing.T) {
	h := newHarness(t, Options{})
	started := make(chan string, 2)
	release := make(chan struct{})
	h.s.Register("holder", fixed(StageDef{Name: "asr", Resource: ResourceASR, Run: block(started, release)}))
	h.s.Register(testKind, fixed(
		StageDef{Name: "first", Run: ok},
		StageDef{Name: "asr", Deps: []string{"first"}, Resource: ResourceASR, Run: ok},
	))
	h.start()
	holder := h.submit("holder", nil).JobID
	recv(t, started, "holder")
	id := h.submit(testKind, nil).JobID
	h.waitFor(id, "first stage done", func(j app.Job) bool { return stageOf(t, j, "first").Status == app.StageSucceeded })
	j, err := h.s.Cancel(context.Background(), id)
	if err != nil || j.Status != app.JobCancelled {
		t.Fatalf("Cancel = %s, %v", describe(j), err)
	}
	close(release)
	h.waitStatus(holder, app.JobSucceeded)
}

func TestForcedCancelAfterTimeout(t *testing.T) {
	var killed sync.Map
	h := newHarness(t, Options{CancelTimeout: 30 * time.Millisecond, ForceKill: func(id string) { killed.Store(id, true) }})
	started := make(chan string, 2)
	letGo := make(chan struct{})
	var lateResult atomic.Value // SetResult outcome of the abandoned stage
	h.s.Register(testKind, fixed(StageDef{Name: "stubborn", Resource: ResourceASR, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
		started <- sc.Stage
		<-letGo // ignores ctx, like a child process that does not die on SIGTERM
		sc.Progress(0.9)
		err := sc.SetResult(func(r *app.JobResult) { r.NoteID = ptr("late") })
		lateResult.CompareAndSwap(nil, fmt.Sprint(err)) // the first job's (abandoned) stage gets here first
		return &StageResult{ArtifactID: "late"}, nil
	}}))
	h.start()
	ctx := context.Background()
	id := h.submit(testKind, nil).JobID
	recv(t, started, "stubborn stage")
	begin := time.Now()
	if j, err := h.s.Cancel(ctx, id); err != nil || j.Status != app.JobCancelling {
		t.Fatalf("Cancel = %s, %v", describe(j), err)
	}
	if _, err := h.s.Cancel(ctx, id); !app.IsCode(err, app.CodeConflict) {
		t.Fatalf("cancel of a cancelling job = %v", err)
	}
	j := h.waitStatus(id, app.JobCancelled)
	if time.Since(begin) < 30*time.Millisecond {
		t.Fatal("forced cancel happened before the timeout")
	}
	if _, ok := killed.Load(id); !ok {
		t.Fatal("ForceKill was not called")
	}
	if stageOf(t, j, "stubborn").Status != app.StageCancelled {
		t.Fatalf("job = %s", describe(j))
	}
	evs, _ := h.st.ListJobEvents(ctx, id)
	last := evs[len(evs)-1]
	if last.Type != "job.cancelled" || !strings.Contains(string(last.Data), `"forced":true`) {
		t.Fatalf("last event = %s %s", last.Type, last.Data)
	}

	// The abandoned stage still holds its resource slot until it returns...
	second := h.submit(testKind, nil).JobID
	time.Sleep(30 * time.Millisecond)
	if j, _ := h.s.Get(ctx, second); j.Status != app.JobQueued {
		t.Fatalf("second job started while the abandoned stage still runs: %s", describe(j))
	}
	// ...and whatever it reports afterwards is ignored.
	close(letGo)
	recv(t, started, "second job's stage")
	after, _ := h.s.Get(ctx, id)
	if after.Status != app.JobCancelled || after.Rev != j.Rev || after.Result.NoteID != nil ||
		stageOf(t, after, "stubborn").Status != app.StageCancelled {
		t.Fatalf("abandoned stage changed the job: %+v", after)
	}
	if s, _ := lateResult.Load().(string); !strings.Contains(s, string(app.CodeConflict)) {
		t.Fatalf("SetResult from an abandoned stage = %q", s)
	}
	h.waitStatus(second, app.JobSucceeded)
}

func TestStopInterruptsAndNewSchedulerResumes(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	var runs [3]atomic.Int64
	started := make(chan string, 4)
	var healthy atomic.Bool
	build := fixed(
		StageDef{Name: "first", Run: func(context.Context, *StageContext) (*StageResult, error) {
			runs[0].Add(1)
			return &StageResult{ArtifactID: "art_first"}, nil
		}},
		StageDef{Name: "long", Deps: []string{"first"}, Resource: ResourceASR, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			runs[1].Add(1)
			if healthy.Load() {
				if d, ok := sc.Dep("first"); !ok || d.ArtifactID != "art_first" {
					return nil, fmt.Errorf("Dep(first) after resume = %+v, %v", d, ok)
				}
				return nil, nil
			}
			sc.Progress(0.4)
			started <- sc.Stage
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		StageDef{Name: "last", Deps: []string{"long"}, Run: func(context.Context, *StageContext) (*StageResult, error) {
			runs[2].Add(1)
			return nil, nil
		}},
	)
	var killed atomic.Int64
	h := attach(t, st, Options{ForceKill: func(string) { killed.Add(1) }})
	h.s.Register(testKind, build)
	h.start()
	ctx := context.Background()
	running := h.submit(testKind, nil).JobID
	recv(t, started, "long stage")
	queued := h.submit(testKind, nil).JobID // waits for the asr slot
	h.waitFor(queued, "first stage of the second job", func(j app.Job) bool { return stageOf(t, j, "first").Status == app.StageSucceeded })

	if err := h.s.Stop(ctx, false); err != nil {
		t.Fatal(err)
	}
	if killed.Load() != 0 {
		t.Fatal("ForceKill called although the stage stopped in time")
	}
	for _, id := range []string{running, queued} {
		row, err := st.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != app.JobInterrupted || row.Stage("first").Status != app.StageSucceeded ||
			row.Stage("long").Status != app.StagePending || row.Stage("long").Progress != 0 || row.Error != nil {
			t.Fatalf("after Stop: %s %+v", row.Status, row.Stages)
		}
	}
	// The interrupted transition was published before Stop returned.
	if revs := h.rec.revs(running); len(revs) == 0 {
		t.Fatal("nothing published")
	} else if row, _ := st.GetJob(ctx, running); revs[len(revs)-1] != row.Rev {
		t.Fatalf("last published rev %d, stored rev %d", revs[len(revs)-1], row.Rev)
	}
	// A stopped scheduler refuses work.
	if _, err := h.s.Submit(ctx, JobParams{Kind: testKind}); !app.IsCode(err, app.CodeEngineUnavailable) {
		t.Fatalf("Submit after Stop = %v", err)
	}
	if _, err := h.s.Retry(ctx, running); !app.IsCode(err, app.CodeEngineUnavailable) {
		t.Fatalf("Retry after Stop = %v", err)
	}
	if _, err := h.s.Cancel(ctx, running); !app.IsCode(err, app.CodeEngineUnavailable) {
		t.Fatalf("Cancel after Stop = %v", err)
	}
	if err := h.s.Stop(ctx, false); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	// Next engine run: recovery + a new scheduler finish both jobs without
	// repeating the stage that had succeeded.
	res, err := st.Recover(ctx, true)
	if err != nil || len(res.Requeued) != 2 {
		t.Fatalf("Recover = %+v, %v", res, err)
	}
	healthy.Store(true)
	h2 := attach(t, st, Options{})
	h2.s.Register(testKind, build)
	h2.start()
	for _, id := range []string{running, queued} {
		j := h2.waitStatus(id, app.JobSucceeded)
		if stageOf(t, j, "first").Attempt != 1 || j.Attempt != 1 || j.Progress != 1 {
			t.Fatalf("resumed job = %+v", j)
		}
	}
	if runs[0].Load() != 2 || runs[1].Load() != 3 || runs[2].Load() != 2 {
		t.Fatalf("runs = %d %d %d, want 2 3 2", runs[0].Load(), runs[1].Load(), runs[2].Load())
	}
}

func TestStopTimeoutAndForce(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			var killedAt atomic.Int64
			h := newHarness(t, Options{StopTimeout: 40 * time.Millisecond, ForceKill: func(string) {
				killedAt.CompareAndSwap(0, time.Now().UnixNano())
			}})
			started := make(chan string, 1)
			letGo := make(chan struct{})
			defer close(letGo)
			h.s.Register(testKind, fixed(StageDef{Name: "stubborn", Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
				started <- sc.Stage
				<-letGo
				return nil, nil
			}}))
			h.start()
			id := h.submit(testKind, nil).JobID
			recv(t, started, "stage")
			begin := time.Now()
			if err := h.s.Stop(context.Background(), force); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(begin); d < 40*time.Millisecond || d > 5*time.Second {
				t.Fatalf("Stop took %v", d)
			}
			k := killedAt.Load()
			if k == 0 {
				t.Fatal("ForceKill not called")
			}
			// force kills before waiting; otherwise only after the wait.
			if early := time.Unix(0, k).Sub(begin) < 30*time.Millisecond; early != force {
				t.Fatalf("ForceKill after %v with force=%v", time.Unix(0, k).Sub(begin), force)
			}
			row, _ := h.st.GetJob(context.Background(), id)
			if row.Status != app.JobInterrupted || row.Stage("stubborn").Status != app.StagePending {
				t.Fatalf("after Stop: %s %+v", row.Status, row.Stages)
			}
		})
	}
}

func TestWait(t *testing.T) {
	h := newHarness(t, Options{})
	started := make(chan string, 1)
	release := make(chan struct{})
	h.s.Register(testKind, fixed(StageDef{Name: "s", Run: block(started, release)}))
	h.start()
	ctx := context.Background()
	id := h.submit(testKind, nil).JobID
	recv(t, started, "stage")
	cur := h.waitStatus(id, app.JobRunning)

	// rev already beyond after_rev: immediate.
	begin := time.Now()
	if j, err := h.s.Wait(ctx, id, cur.Rev-1, 5*time.Second); err != nil || j.Rev != cur.Rev || time.Since(begin) > time.Second {
		t.Fatalf("Wait(after older rev) = rev %d, %v after %v", j.Rev, err, time.Since(begin))
	}
	// No change: returns the current job when the timeout elapses.
	begin = time.Now()
	if j, err := h.s.Wait(ctx, id, cur.Rev, 60*time.Millisecond); err != nil || j.Rev != cur.Rev || time.Since(begin) < 60*time.Millisecond {
		t.Fatalf("Wait(timeout) = rev %d, %v after %v", j.Rev, err, time.Since(begin))
	}
	// timeout 0 never blocks.
	if j, err := h.s.Wait(ctx, id, cur.Rev+100, 0); err != nil || j.Rev != cur.Rev {
		t.Fatalf("Wait(0) = rev %d, %v", j.Rev, err)
	}
	// A change wakes the waiter long before the timeout.
	type result struct {
		j   app.Job
		err error
		d   time.Duration
	}
	got := make(chan result, 1)
	go func() {
		begin := time.Now()
		j, err := h.s.Wait(ctx, id, cur.Rev, 10*time.Second)
		got <- result{j, err, time.Since(begin)}
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	r := recv(t, got, "Wait to return")
	if r.err != nil || r.j.Rev <= cur.Rev || r.d > 5*time.Second {
		t.Fatalf("Wait(change) = rev %d, %v after %v", r.j.Rev, r.err, r.d)
	}
	// Context cancellation and unknown jobs.
	h.waitStatus(id, app.JobSucceeded)
	final, _ := h.s.Get(ctx, id)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := h.s.Wait(cctx, id, final.Rev, 10*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait(ctx done) = %v", err)
	}
	if _, err := h.s.Wait(ctx, "job_missing", 0, time.Second); !app.IsCode(err, app.CodeNotFound) {
		t.Fatalf("Wait(unknown) = %v", err)
	}
	if _, err := h.s.Get(ctx, "job_missing"); !app.IsCode(err, app.CodeNotFound) {
		t.Fatalf("Get(unknown) = %v", err)
	}
	// Stop releases waiters.
	go func() {
		begin := time.Now()
		j, err := h.s.Wait(ctx, id, final.Rev, 10*time.Second)
		got <- result{j, err, time.Since(begin)}
	}()
	time.Sleep(20 * time.Millisecond)
	h.s.Stop(ctx, false)
	if r := recv(t, got, "Wait to return on Stop"); r.err != nil || r.d > 5*time.Second {
		t.Fatalf("Wait across Stop = %v after %v", r.err, r.d)
	}
}

func TestReprocessFromSetsIgnoreCache(t *testing.T) {
	all := []string{ReprocessAcquire}
	fromTranscribe := []string{ReprocessAcquire, ReprocessTranscribe}
	fromNote := []string{ReprocessAcquire, ReprocessTranscribe, ReprocessNote}
	stages := []struct {
		name   string
		deps   []string
		groups []string
	}{
		{app.StageEnsureTools, nil, nil},
		{app.StageAcquire, []string{app.StageEnsureTools}, all},
		{app.StageProbe, []string{app.StageAcquire}, all},
		{app.StageExtractAudio, []string{app.StageProbe}, all},
		{app.StageTranscribe, []string{app.StageExtractAudio}, fromTranscribe},
		{app.StagePlayback, []string{app.StageProbe}, all},
		{app.StageNoteMap, []string{app.StageTranscribe}, fromNote},
		{app.StageNoteReduce, []string{app.StageNoteMap}, fromNote},
		{app.StageNoteRender, []string{app.StageNoteReduce}, fromNote},
	}
	for _, tc := range []struct {
		from string
		want []string // stages with IgnoreCache
	}{
		{"", nil},
		{"bogus", nil},
		{ReprocessNote, []string{app.StageNoteMap, app.StageNoteReduce, app.StageNoteRender}},
		{ReprocessTranscribe, []string{app.StageTranscribe, app.StageNoteMap, app.StageNoteReduce, app.StageNoteRender}},
		{ReprocessAcquire, []string{app.StageAcquire, app.StageProbe, app.StageExtractAudio, app.StageTranscribe,
			app.StagePlayback, app.StageNoteMap, app.StageNoteReduce, app.StageNoteRender}},
	} {
		t.Run("from="+tc.from, func(t *testing.T) {
			h := newHarness(t, Options{})
			var mu sync.Mutex
			var ignored []string
			var defs []StageDef
			for _, s := range stages {
				defs = append(defs, StageDef{Name: s.name, Deps: s.deps, ReprocessGroups: s.groups,
					Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
						if sc.IgnoreCache {
							mu.Lock()
							ignored = append(ignored, sc.Stage)
							mu.Unlock()
						}
						return nil, nil
					}})
			}
			h.s.Register(app.JobKindProcessVideo, fixed(defs...))
			h.start()
			var opts any
			if tc.from != "" {
				opts = app.ImportRequest{ReprocessFrom: tc.from, Language: "zh"}
			}
			h.waitStatus(h.submit(app.JobKindProcessVideo, opts).JobID, app.JobSucceeded)
			slices.Sort(ignored)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(ignored, want) {
				t.Fatalf("IgnoreCache on %v, want %v", ignored, want)
			}
		})
	}
}

func TestProgressIsThrottled(t *testing.T) {
	h := newHarness(t, Options{ProgressInterval: 40 * time.Millisecond})
	reported := make(chan struct{})
	release := make(chan struct{})
	h.s.Register(testKind, fixed(
		StageDef{Name: "s", Weight: 3, Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
			for i := range 2000 {
				sc.Progress(float64(i) / 4000)
			}
			sc.Progress(7)     // clamped to 1...
			sc.Progress(0.5)   // ...and the last value wins
			sc.Progress(nan()) // ignored
			close(reported)
			<-release
			return nil, nil
		}},
		StageDef{Name: "t", Deps: []string{"s"}, Weight: 1, Run: ok},
	))
	h.start()
	id := h.submit(testKind, nil).JobID
	recv(t, reported, "progress calls")
	j := h.waitFor(id, "trailing progress flush", func(j app.Job) bool { return stageOf(t, j, "s").Progress == 0.5 })
	// created(1) + started(2) + first progress(3) + trailing flush(4), and
	// nothing else for 2003 calls.
	if j.Rev > 5 {
		t.Fatalf("2003 Progress calls caused %d revisions", j.Rev)
	}
	if j.Progress != 0.375 || j.Stage != "s" { // 3 × 0.5 / (3 + 1)
		t.Fatalf("job progress = %v, stage %q", j.Progress, j.Stage)
	}
	evs, _ := h.st.ListJobEvents(context.Background(), id)
	for _, e := range evs {
		if strings.Contains(e.Type, "progress") {
			t.Fatalf("progress wrote an event row: %s", e.Type)
		}
	}
	close(release)
	if j := h.waitStatus(id, app.JobSucceeded); stageOf(t, j, "s").Progress != 1 {
		t.Fatalf("final progress = %v", stageOf(t, j, "s").Progress)
	}
}

func nan() float64 {
	var zero float64
	return zero / zero
}

func TestNewJobInsideCallerTransaction(t *testing.T) {
	h := newHarness(t, Options{})
	var ran atomic.Int64
	h.s.Register(testKind, fixed(StageDef{Name: "s", Run: func(context.Context, *StageContext) (*StageResult, error) {
		ran.Add(1)
		return nil, nil
	}}))
	h.start()
	ctx := context.Background()

	// A rolled-back import leaves no job behind.
	boom := errors.New("boom")
	err := h.st.Write(ctx, func(tx *store.Tx) error {
		if _, err := h.s.NewJob(ctx, tx.Queries, JobParams{Kind: testKind, VideoID: h.vid}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if n, _ := h.s.ActiveCount(ctx); n != 0 {
		t.Fatalf("rolled-back job exists: %d active", n)
	}

	// The flow from many goroutines: one video, one active job.
	const n = 16
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var id string
			var created bool
			err := h.st.Write(ctx, func(tx *store.Tx) error {
				if active, err := tx.ActiveJobOfVideo(ctx, h.vid); err != nil || active != nil {
					if active != nil {
						id = active.ID
					}
					return err
				}
				if latest, err := tx.LatestJobOfVideo(ctx, h.vid); err != nil || latest != nil {
					if latest != nil {
						id = latest.ID
					}
					return err
				}
				j, err := h.s.NewJob(ctx, tx.Queries, JobParams{Kind: testKind, VideoID: h.vid, RequestedBy: "mcp"})
				if err != nil {
					return err
				}
				id, created = j.ID, true
				return nil
			})
			if err != nil {
				t.Errorf("import: %v", err)
			}
			if created {
				h.s.Announce(ctx, id)
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("two jobs for one video: %s and %s", first, id)
		}
	}
	j := h.waitStatus(first, app.JobSucceeded)
	if ran.Load() != 1 || j.RequestedBy != "mcp" || j.VideoID == nil || *j.VideoID != h.vid {
		t.Fatalf("job = %+v, ran %d", j, ran.Load())
	}
	h.s.Stop(ctx, false) // flushes the notifications
	revs := h.rec.revs(first)
	if len(revs) == 0 || revs[0] > 2 || !slices.IsSorted(revs) || len(slices.Compact(slices.Clone(revs))) != len(revs) {
		t.Fatalf("published revs are not strictly increasing from the creation: %v", revs)
	}
	h = newHarness(t, Options{})
	h.s.Register(testKind, fixed(StageDef{Name: "s", Run: ok}))
	h.start()

	for _, tc := range []struct {
		name string
		p    JobParams
	}{
		{"unregistered kind", JobParams{Kind: "nope"}},
		{"options not an object", JobParams{Kind: testKind, Options: []int{1}}},
	} {
		if _, err := h.s.Submit(ctx, tc.p); !app.IsCode(err, app.CodeInvalidArgument) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// A job without a video (prepare_tools) is fine.
	if j := h.waitStatus(func() string {
		j, err := h.s.Submit(ctx, JobParams{Kind: testKind})
		if err != nil {
			t.Fatal(err)
		}
		return j.JobID
	}(), app.JobSucceeded); j.VideoID != nil {
		t.Fatalf("video id = %v", *j.VideoID)
	}
}

func TestInvalidDefinitions(t *testing.T) {
	for name, defs := range map[string][]StageDef{
		"empty":       nil,
		"no run":      {{Name: "a"}},
		"no name":     {{Run: ok}},
		"duplicate":   {{Name: "a", Run: ok}, {Name: "a", Run: ok}},
		"unknown dep": {{Name: "a", Deps: []string{"x"}, Run: ok}},
		"forward dep": {{Name: "a", Deps: []string{"b"}, Run: ok}, {Name: "b", Run: ok}},
		"self dep":    {{Name: "a", Deps: []string{"a"}, Run: ok}},
	} {
		if err := validateDefs("k", defs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validateDefs("k", []StageDef{{Name: "a", Run: ok}, {Name: "b", Deps: []string{"a"}, Run: ok}}); err != nil {
		t.Errorf("valid definitions rejected: %v", err)
	}
}

// A job whose kind lost its builder (or whose stage list changed) fails
// cleanly instead of sitting in the queue forever.
func TestUnrunnableJobFails(t *testing.T) {
	h := newHarness(t, Options{})
	ctx := context.Background()
	orphan, err := h.st.CreateJob(ctx, &store.Job{Kind: "gone", VideoID: h.vid}, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := h.st.CreateJob(ctx, &store.Job{Kind: testKind, VideoID: h.vid}, []string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	h.s.Register(testKind, fixed(StageDef{Name: "new", Run: ok}))
	h.start()
	for _, id := range []string{orphan.ID, changed.ID} {
		if j := h.waitStatus(id, app.JobFailed); j.Error == nil || j.Error.Code != app.CodeInternal {
			t.Fatalf("job = %+v", j)
		}
	}
}

func TestStopLeavesNoGoroutines(t *testing.T) {
	st := openStore(t, t.TempDir())
	// Warm up the database pools so their goroutines are in the baseline.
	if err := st.IntegrityCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	h0 := attach(t, st, Options{})
	h0.s.Stop(context.Background(), false)
	settle := func() int {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	base := settle()

	for round := range 3 {
		h := attach(t, st, Options{CancelTimeout: time.Hour})
		started := make(chan string, 16)
		never := make(chan struct{})
		h.s.Register(testKind, fixed(
			StageDef{Name: "quick", Run: func(ctx context.Context, sc *StageContext) (*StageResult, error) {
				sc.Progress(0.1)
				sc.Progress(0.2) // leaves a throttle timer pending
				return nil, nil
			}},
			StageDef{Name: "slow", Deps: []string{"quick"}, Run: block(started, never)},
		))
		h.start()
		ctx := context.Background()
		var ids []string
		for range 4 {
			ids = append(ids, h.submit(testKind, nil).JobID)
		}
		for range ids {
			recv(t, started, "slow stage")
		}
		// One cancel in flight (its 1 h timer must not outlive Stop) and a
		// long poll.
		go h.s.Wait(ctx, ids[1], 1<<40, time.Hour)
		if _, err := h.s.Cancel(ctx, ids[0]); err != nil {
			t.Fatal(err)
		}
		if err := h.s.Stop(ctx, round%2 == 1); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids[1:] {
			if row, _ := st.GetJob(ctx, id); row.Status != app.JobInterrupted {
				t.Fatalf("job after Stop: %s", row.Status)
			}
		}
		if row, _ := st.GetJob(ctx, ids[0]); row.Status != app.JobCancelled {
			t.Fatalf("cancelled job after Stop: %s", row.Status)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		n := settle()
		if n <= base {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines: %d before, %d after Stop\n%s", base, n, buf[:runtime.Stack(buf, true)])
		}
	}
}

func TestToDTO(t *testing.T) {
	// The process_video weights (local file: acquire weighs 2).
	defs := []StageDef{
		{Name: app.StageEnsureTools, Weight: 2},
		{Name: app.StageAcquire, Weight: 2},
		{Name: app.StageProbe, Weight: 1},
		{Name: app.StageExtractAudio, Weight: 4},
		{Name: app.StageTranscribe, Weight: 45},
		{Name: app.StagePlayback, ExcludeFromProgress: true},
		{Name: app.StageNoteMap, Weight: 25},
		{Name: app.StageNoteReduce, Weight: 7},
		{Name: app.StageNoteRender, Weight: 1},
	}
	const total = 2 + 2 + 1 + 4 + 45 + 25 + 7 + 1
	mk := func(status string, stages ...store.JobStage) *store.JobWithStages {
		j := &store.JobWithStages{Job: store.Job{ID: "job_1", Kind: app.JobKindProcessVideo, VideoID: "vid_1",
			Status: status, Rev: 9, Attempt: 2, CreatedAt: "c", UpdatedAt: "u", RequestedBy: "web"}}
		// Database order differs from DAG order on purpose.
		for i := len(defs) - 1; i >= 0; i-- {
			st := store.JobStage{JobID: "job_1", Stage: defs[i].Name, Status: app.StagePending}
			for _, s := range stages {
				if s.Stage == st.Stage {
					st = s
				}
			}
			j.Stages = append(j.Stages, st)
		}
		return j
	}
	ok := func(name string) store.JobStage {
		return store.JobStage{Stage: name, Status: app.StageSucceeded, Progress: 1, Attempt: 1, StartedAt: "s", FinishedAt: "f"}
	}
	approx := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

	t.Run("running", func(t *testing.T) {
		j := mk(app.JobRunning, ok(app.StageEnsureTools), ok(app.StageAcquire), ok(app.StageProbe), ok(app.StageExtractAudio),
			store.JobStage{Stage: app.StageTranscribe, Status: app.StageRunning, Progress: 0.5, Attempt: 1, StartedAt: "s"},
			store.JobStage{Stage: app.StagePlayback, Status: app.StageRunning, Progress: 0.9, Attempt: 1})
		d := toDTO(j, defs)
		if want := (2 + 2 + 1 + 4 + 45*0.5) / total; !approx(d.Progress, want) {
			t.Errorf("progress = %v, want %v (playback must not count)", d.Progress, want)
		}
		if d.Stage != app.StageTranscribe {
			t.Errorf("stage = %q", d.Stage)
		}
		var order []string
		for _, s := range d.Stages {
			order = append(order, s.Stage)
		}
		var want []string
		for _, def := range defs {
			want = append(want, def.Name)
		}
		if !slices.Equal(order, want) {
			t.Errorf("stage order = %v", order)
		}
		if d.JobID != "job_1" || *d.VideoID != "vid_1" || d.Rev != 9 || d.Attempt != 2 || d.RequestedBy != "web" ||
			d.StartedAt != nil || d.FinishedAt != nil || d.Stages[0].StartedAt == nil || d.Stages[8].StartedAt != nil {
			t.Errorf("fields = %+v", d)
		}
	})
	t.Run("only playback left", func(t *testing.T) {
		var done []store.JobStage
		for _, def := range defs {
			if def.Name != app.StagePlayback {
				done = append(done, ok(def.Name))
			}
		}
		d := toDTO(mk(app.JobRunning, done...), defs)
		if !approx(d.Progress, 1) || d.Stage != app.StagePlayback {
			t.Errorf("progress %v stage %q", d.Progress, d.Stage)
		}
	})
	t.Run("terminal states", func(t *testing.T) {
		failed := mk(app.JobFailed, ok(app.StageEnsureTools),
			store.JobStage{Stage: app.StageAcquire, Status: app.StageFailed, Progress: 0.5, Error: app.E(app.CodeNetworkError, "x").WithStage(app.StageAcquire)})
		failed.Error = failed.Stages[7].Error
		d := toDTO(failed, defs)
		if d.Stage != "" || !approx(d.Progress, (2+2*0.5)/total) || d.Error == nil || d.Stages[1].Error == nil {
			t.Errorf("failed: stage %q progress %v", d.Stage, d.Progress)
		}
		if d := toDTO(mk(app.JobCancelled), defs); d.Stage != "" || d.Progress != 0 {
			t.Errorf("cancelled: stage %q progress %v", d.Stage, d.Progress)
		}
		if d := toDTO(mk(app.JobSucceeded), defs); d.Stage != "" || d.Progress != 1 {
			t.Errorf("succeeded: stage %q progress %v", d.Stage, d.Progress)
		}
		// interrupted and queued jobs still point at where they will go on.
		if d := toDTO(mk(app.JobInterrupted, ok(app.StageEnsureTools)), defs); d.Stage != app.StageAcquire {
			t.Errorf("interrupted: stage %q", d.Stage)
		}
		if d := toDTO(mk(app.JobQueued), defs); d.Stage != app.StageEnsureTools || d.VideoID == nil {
			t.Errorf("queued: stage %q", d.Stage)
		}
	})
	t.Run("without definitions", func(t *testing.T) {
		// Historical job of an unknown kind: database order, equal weights.
		j := &store.JobWithStages{Job: store.Job{ID: "job_2", Kind: "legacy", Status: app.JobRunning}, Stages: []store.JobStage{
			{Stage: "one", Status: app.StageSucceeded}, {Stage: "two", Status: app.StageRunning, Progress: 0.5}, {Stage: "three", Status: app.StagePending},
			{Stage: "four", Status: app.StagePending}}}
		d := toDTO(j, nil)
		if !approx(d.Progress, 1.5/4) || d.Stage != "two" || d.VideoID != nil || len(d.Stages) != 4 || d.Stages[0].Progress != 1 {
			t.Errorf("legacy: %+v", d)
		}
		// Definitions without weights fall back to equal weights too.
		d = toDTO(j, []StageDef{{Name: "one"}, {Name: "two"}, {Name: "three"}, {Name: "four", ExcludeFromProgress: true}})
		if !approx(d.Progress, 1.5/3) {
			t.Errorf("unweighted: %v", d.Progress)
		}
	})
	t.Run("through the scheduler", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.s.Register(app.JobKindProcessVideo, func(job *store.Job) ([]StageDef, error) {
			out := slices.Clone(defs)
			for i := range out {
				out[i].Run = ok2
			}
			// Definitions may depend on the job options.
			if strings.Contains(string(job.Options), `"source_type":"youtube"`) {
				out[1].Weight = 15
			}
			return out, nil
		})
		j := mk(app.JobRunning, ok(app.StageEnsureTools), ok(app.StageAcquire))
		if d := h.s.ToDTO(j); !approx(d.Progress, 4.0/total) {
			t.Errorf("file: %v", d.Progress)
		}
		j.Options = []byte(`{"source_type":"youtube"}`)
		if d := h.s.ToDTO(j); !approx(d.Progress, 17.0/(total+13)) {
			t.Errorf("youtube: %v", d.Progress)
		}
	})
}

func ok2(context.Context, *StageContext) (*StageResult, error) { return nil, nil }
