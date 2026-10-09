// Package jobs is the single scheduler of an engine (docs/v0.md D-18, §7):
// it runs each job as a fixed DAG of stages, bounded by per-resource
// semaphores, and owns the job and stage state machines of §7.3.
//
// # Usage
//
//	sched := jobs.New(jobs.Options{Store: st, Publisher: bus, ...})
//	sched.Register(app.JobKindProcessVideo, buildProcessVideo) // per job kind
//	st.Recover(ctx, autoResume)                                 // §7.6, before Start
//	sched.Start(ctx)
//	...
//	// inside the import transaction (§7.5):
//	st.Write(ctx, func(tx *store.Tx) error {
//	    ... check ActiveJobOfVideo ...
//	    job, err = sched.NewJob(ctx, tx.Queries, jobs.JobParams{...})
//	    return err
//	})
//	sched.Announce(ctx, job.ID) // after commit: publish + wake
//	...
//	sched.Stop(ctx, force)
//
// The database is the source of truth for every status; the scheduler keeps
// only the handles of the stages that are running right now, so a job can be
// resumed by a new scheduler after a restart.
//
// # Callbacks
//
// Publisher.Publish and OnJobChanged are called from one dedicated goroutine,
// in rev order per job, without any scheduler lock held: they may call back
// into the scheduler. The ForceKill hook and the Builder functions are
// called with the scheduling lock held and must not call the scheduler.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"watchink/internal/app"
	"watchink/internal/store"
)

// Resource classes of D-18. Any other name (including "") is unlimited.
const (
	ResourceNet   = "net"
	ResourceMedia = "media"
	ResourceASR   = "asr"
	ResourceLLM   = "llm"
	ResourceIO    = "io"
	ResourceCPU   = "cpu"
)

// Reprocess groups: the values of the reprocess_from job option (§7.5).
const (
	ReprocessAcquire    = "acquire"
	ReprocessTranscribe = "transcribe"
	ReprocessNote       = "note"

	// OptionReprocessFrom is the key read from the job options JSON.
	OptionReprocessFrom = "reprocess_from"
)

// Defaults for the timeouts of §7.3 / §5.4 and the progress throttle.
const (
	DefaultCancelTimeout    = 10 * time.Second
	DefaultStopTimeout      = 10 * time.Second
	DefaultProgressInterval = 500 * time.Millisecond
)

// DefaultLimits returns the resource limits of D-18.
func DefaultLimits() map[string]int {
	return map[string]int{ResourceNet: 2, ResourceMedia: 1, ResourceASR: 1, ResourceLLM: 4}
}

// Publisher receives job.updated events; the engine's event bus satisfies it.
type Publisher interface {
	Publish(app.Event) app.Event
}

// StageFunc runs one stage. ctx is cancelled when the job is cancelled, a
// sibling stage failed or the scheduler stops; the function must return
// promptly then (any error is fine). A returned error that was not caused by
// such a cancellation fails the stage and the job: *app.Error values are
// recorded as they are (with Stage set), anything else becomes INTERNAL.
// A nil *StageResult with a nil error is a success without an artifact.
type StageFunc func(ctx context.Context, sc *StageContext) (*StageResult, error)

// StageResult is what a successful stage hands back for job_stages.
type StageResult struct {
	ArtifactID  string
	Reused      bool // the fingerprint hit a registered artifact (§7.4)
	Fingerprint string
}

// StageDef describes one stage of a job kind.
type StageDef struct {
	Name string
	// Deps are stage names that must have succeeded first. They must appear
	// earlier in the list, which makes the list order a DAG order.
	Deps []string
	// Resource is the semaphore class the stage occupies while it runs.
	// ResourceLLM is special: llm stages are not gated as a whole — they
	// gate individual model requests with StageContext.Acquire.
	Resource string
	// Weight is the stage's share of the overall job progress (§7.2).
	Weight float64
	// ExcludeFromProgress leaves the stage out of the overall progress
	// (playback).
	ExcludeFromProgress bool
	// ReprocessGroups lists the reprocess_from values that make this stage
	// ignore cached artifacts. For process_video: every stage from acquire
	// on carries "acquire"; transcribe and the note stages carry
	// "transcribe"; the note stages carry "note".
	ReprocessGroups []string
	Run             StageFunc
}

// Builder returns the stage definitions of a job. It is called when the job
// is created (to write the job_stages rows) and again whenever the job is
// scheduled, retried or converted to a DTO, possibly by a later engine run,
// so it must be a pure function of the job row (kind, options, video ID):
// put what the definitions depend on (e.g. the source type that decides the
// acquire weight) into the job options. In particular it must not read the
// database — during NewJob the surrounding transaction is not committed yet.
type Builder func(job *store.Job) ([]StageDef, error)

// Options configures a Scheduler.
type Options struct {
	Store *store.Store // required
	// Publisher receives a job.updated event for every job change; may be nil.
	Publisher Publisher
	// OnJobChanged is called after each published change (e.g. to publish
	// video.updated); may be nil.
	OnJobChanged func(job app.Job)
	// ForceKill must kill the child processes of a job right away. It is
	// called when a cancelled job has not stopped within CancelTimeout and
	// when Stop gives up waiting (or immediately for a forced Stop).
	ForceKill func(jobID string)
	// Limits overrides DefaultLimits per resource; a value <= 0 makes the
	// resource unlimited.
	Limits map[string]int
	// CancelTimeout / StopTimeout / ProgressInterval default to the
	// Default* constants when zero.
	CancelTimeout    time.Duration
	StopTimeout      time.Duration
	ProgressInterval time.Duration
	Logger           *slog.Logger
}

// JobParams describes a job to create.
type JobParams struct {
	Kind        string
	VideoID     string // "" for jobs without a video (prepare_tools)
	RequestedBy string // desktop / web / cli / mcp
	// Options is marshalled to the jobs.options JSON object; nil means {}.
	// The scheduler itself reads only "reprocess_from".
	Options any
	// ID is optional; a new job ID is generated when empty.
	ID string
}

// validateDefs checks the invariants the scheduler relies on.
func validateDefs(kind string, defs []StageDef) error {
	if len(defs) == 0 {
		return fmt.Errorf("job kind %q has no stages", kind)
	}
	seen := make(map[string]bool, len(defs))
	for _, d := range defs {
		if d.Name == "" || d.Run == nil {
			return fmt.Errorf("job kind %q: stage %q needs a name and a Run function", kind, d.Name)
		}
		if seen[d.Name] {
			return fmt.Errorf("job kind %q: duplicate stage %q", kind, d.Name)
		}
		for _, dep := range d.Deps {
			if !seen[dep] {
				return fmt.Errorf("job kind %q: stage %q depends on %q, which is not defined before it", kind, d.Name, dep)
			}
		}
		seen[d.Name] = true
	}
	return nil
}

// Register installs the builder of a job kind. Register every kind before
// Start; registering a kind twice replaces the builder.
func (s *Scheduler) Register(kind string, b Builder) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	s.builders[kind] = b
}

// defsFor returns the validated stage definitions of a job.
func (s *Scheduler) defsFor(job *store.Job) ([]StageDef, error) {
	s.regMu.RLock()
	b := s.builders[job.Kind]
	s.regMu.RUnlock()
	if b == nil {
		return nil, fmt.Errorf("job kind %q is not registered", job.Kind)
	}
	defs, err := b(job)
	if err != nil {
		return nil, err
	}
	if err := validateDefs(job.Kind, defs); err != nil {
		return nil, err
	}
	return defs, nil
}

// NewJob creates a queued job and its stage rows through q. Pass tx.Queries
// to make the job part of a caller transaction (the import flow of §7.5
// checks "at most one active job per video" in that same transaction), then
// call Announce(jobID) after the transaction committed. Submit does both for
// the simple case.
//
// Errors: ENGINE_UNAVAILABLE once Stop was called, INVALID_ARGUMENT for an
// unregistered kind or unusable options.
func (s *Scheduler) NewJob(ctx context.Context, q *store.Queries, p JobParams) (*store.JobWithStages, error) {
	if s.isStopping() {
		return nil, errStopping()
	}
	options := json.RawMessage("{}")
	if p.Options != nil {
		b, err := json.Marshal(p.Options)
		if err != nil || len(b) == 0 || b[0] != '{' {
			return nil, app.E(app.CodeInvalidArgument, "任务参数必须是 JSON 对象")
		}
		options = b
	}
	id := p.ID
	if id == "" {
		id = app.NewID(app.PrefixJob)
	}
	job := &store.Job{ID: id, Kind: p.Kind, VideoID: p.VideoID, RequestedBy: p.RequestedBy,
		Options: options, Status: app.JobQueued}
	defs, err := s.defsFor(job)
	if err != nil {
		return nil, app.E(app.CodeInvalidArgument, "无法创建任务：%v", err).WithCause(err)
	}
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return q.CreateJob(ctx, job, names)
}

// Announce publishes the current state of a job created with NewJob and
// wakes the scheduler. Call it after the creating transaction committed.
//
// If the scheduler already picked the job up by itself (it may notice a
// committed job on any wake-up) the newer state has been published and
// Announce publishes nothing: events of a job never go back in rev.
func (s *Scheduler) Announce(ctx context.Context, jobID string) {
	// Read under applyMu so no change can slip in between the read and the
	// notification.
	s.applyMu.Lock()
	if j, err := s.st.GetJob(ctx, jobID); err == nil {
		s.emit(j)
	}
	s.applyMu.Unlock()
	s.Wake()
}

// Submit creates a job in its own transaction, publishes it and wakes the
// scheduler.
func (s *Scheduler) Submit(ctx context.Context, p JobParams) (app.Job, error) {
	j, err := s.NewJob(ctx, s.st.Queries, p)
	if err != nil {
		return app.Job{}, err
	}
	s.Announce(ctx, j.ID)
	return s.ToDTO(j), nil
}

// Get returns the job DTO, or NOT_FOUND.
func (s *Scheduler) Get(ctx context.Context, jobID string) (app.Job, error) {
	j, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return app.Job{}, err
	}
	return s.ToDTO(j), nil
}

// ActiveCount counts queued + running + cancelling jobs (§5.4).
func (s *Scheduler) ActiveCount(ctx context.Context) (int, error) {
	return s.st.CountActiveJobs(ctx)
}

// ToDTO assembles the wire form of a job (§13.2).
//
// Stages are listed in the DAG order of the registered definitions (stage
// rows unknown to the definitions follow in database order). Progress is the
// weighted sum of §7.2 over the stages not excluded from progress, a
// succeeded stage counting as 1. Stage is the first stage that has not
// succeeded yet, looking at the progress-relevant stages first so that a
// still-running playback does not hide the main line; it is empty once the
// job succeeded, failed or was cancelled.
//
// It also works for historical jobs and for kinds that are not registered
// (any more): without definitions every stage weighs the same.
func (s *Scheduler) ToDTO(j *store.JobWithStages) app.Job {
	defs, _ := s.defsFor(&j.Job)
	return toDTO(j, defs)
}

func toDTO(j *store.JobWithStages, defs []StageDef) app.Job {
	type entry struct {
		row      *store.JobStage
		weight   float64
		excluded bool
	}
	entries := make([]entry, 0, len(j.Stages))
	used := make(map[string]bool, len(j.Stages))
	for _, d := range defs {
		if row := j.Stage(d.Name); row != nil {
			entries = append(entries, entry{row, max(d.Weight, 0), d.ExcludeFromProgress})
			used[d.Name] = true
		}
	}
	for i := range j.Stages {
		if !used[j.Stages[i].Stage] {
			w := 0.0
			if len(defs) == 0 {
				w = 1
			}
			entries = append(entries, entry{&j.Stages[i], w, false})
		}
	}
	total := 0.0
	for _, e := range entries {
		if !e.excluded {
			total += e.weight
		}
	}
	if total == 0 { // definitions without weights: count every stage equally
		for i := range entries {
			if !entries[i].excluded {
				entries[i].weight = 1
				total++
			}
		}
	}

	out := app.Job{
		JobID: j.ID, Kind: j.Kind, VideoID: strPtr(j.VideoID), Status: j.Status, Rev: j.Rev, Attempt: j.Attempt,
		Stages: make([]app.JobStage, 0, len(entries)), Error: j.Error, Result: j.Result,
		RequestedBy: j.RequestedBy, CreatedAt: j.CreatedAt, StartedAt: strPtr(j.StartedAt),
		FinishedAt: strPtr(j.FinishedAt), UpdatedAt: j.UpdatedAt,
	}
	var sum float64
	current, currentExcluded := "", ""
	for _, e := range entries {
		r := e.row
		p := min(max(r.Progress, 0), 1)
		if r.Status == app.StageSucceeded {
			p = 1
		}
		out.Stages = append(out.Stages, app.JobStage{Stage: r.Stage, Status: r.Status, Progress: p, Reused: r.Reused,
			Attempt: r.Attempt, Error: r.Error, StartedAt: strPtr(r.StartedAt), FinishedAt: strPtr(r.FinishedAt)})
		if !e.excluded {
			sum += e.weight * p
		}
		if r.Status != app.StageSucceeded {
			if e.excluded && currentExcluded == "" {
				currentExcluded = r.Stage
			} else if !e.excluded && current == "" {
				current = r.Stage
			}
		}
	}
	if total > 0 {
		out.Progress = min(sum/total, 1)
	}
	switch j.Status {
	case app.JobSucceeded:
		out.Progress = 1
	case app.JobFailed, app.JobCancelled:
	default:
		out.Stage = current
		if out.Stage == "" {
			out.Stage = currentExcluded
		}
	}
	return out
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptr[T any](v T) *T { return &v }

func errStopping() *app.Error {
	return app.E(app.CodeEngineUnavailable, "Engine 正在停止，不再接收新的任务操作")
}

// reprocessFrom reads the reprocess_from option of a job.
func reprocessFrom(options json.RawMessage) string {
	var o struct {
		ReprocessFrom string `json:"reprocess_from"`
	}
	_ = json.Unmarshal(options, &o)
	return o.ReprocessFrom
}

func ignoreCache(def *StageDef, options json.RawMessage) bool {
	from := reprocessFrom(options)
	return from != "" && slices.Contains(def.ReprocessGroups, from)
}
