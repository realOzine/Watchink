// Package pipeline implements the stages of the process_video and
// prepare_tools jobs.
//
// The scheduler (internal/core/jobs) knows nothing about video: this package
// gives it a list of stages per job kind (the DAG in processVideoStages) and
// one function per stage. Every stage function has the same shape:
//
//  1. load its inputs: the job options and the artifacts of the stages it
//     depends on;
//  2. compute a fingerprint of those inputs;
//  3. return the registered artifact when one matches (reused);
//  4. otherwise do the work through media / asr / llm / notes, write the
//     result under the video's directory and register it as an artifact.
//
// Files: pipeline.go (wiring and the DAG), artifacts.go (the cache and
// artifact helpers every stage uses), settings.go (settings → package
// configs), then one stage_*.go per group of stages.
package pipeline

import (
	"context"
	"encoding/json"
	"log/slog"

	"watchink/internal/app"
	"watchink/internal/capability/media"
	"watchink/internal/capability/tools"
	"watchink/internal/core/jobs"
	"watchink/internal/infra/procx"
	"watchink/internal/infra/store"
)

// JobOptions is the jobs.options JSON of both job kinds. The stage builders
// are pure functions of it (the scheduler requires that).
type JobOptions struct {
	// SourceType is file / upload / youtube / bilibili; it decides the
	// acquire weight and which tools are needed.
	SourceType string `json:"source_type,omitempty"`
	// Path is the validated local path of a file import.
	Path     string `json:"path,omitempty"`
	URL      string `json:"url,omitempty"`
	UploadID string `json:"upload_id,omitempty"`
	Language string `json:"language,omitempty"`
	// ReprocessFrom is read by the scheduler too (jobs.OptionReprocessFrom).
	ReprocessFrom string `json:"reprocess_from,omitempty"`
	// Items lists the tools and models of a prepare_tools job.
	Items []string `json:"items,omitempty"`
}

// Deps is what the stages need from the engine.
type Deps struct {
	Store  *store.Store
	Runner *procx.Runner
	Tools  *tools.Manager
	// Settings returns the current settings snapshot; stages read it when
	// they start so a config change applies to the next retry.
	Settings func() app.Settings
	// VideoChanged is called after a stage changed a videos row.
	VideoChanged func(videoID string)
	Logger       *slog.Logger
}

// Pipeline holds the stage implementations.
type Pipeline struct {
	st      *store.Store
	runner  *procx.Runner
	tools   *tools.Manager
	cfg     func() app.Settings
	changed func(string)
	log     *slog.Logger
}

// New returns the pipeline; register its builders with Register.
func New(d Deps) *Pipeline {
	p := &Pipeline{st: d.Store, runner: d.Runner, tools: d.Tools, cfg: d.Settings, changed: d.VideoChanged, log: d.Logger}
	if p.changed == nil {
		p.changed = func(string) {}
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p
}

// Register adds both job kinds to the scheduler.
func (p *Pipeline) Register(s *jobs.Scheduler) {
	s.Register(app.JobKindProcessVideo, p.processVideoStages)
	s.Register(app.JobKindPrepareTools, p.prepareToolsStages)
}

// Reprocess groups per stage: a job with reprocess_from=X ignores the
// cache of every stage carrying group X.
var (
	groupsAcquire    = []string{jobs.ReprocessAcquire}
	groupsTranscribe = []string{jobs.ReprocessAcquire, jobs.ReprocessTranscribe}
	groupsNote       = []string{jobs.ReprocessAcquire, jobs.ReprocessTranscribe, jobs.ReprocessNote}
)

func (p *Pipeline) processVideoStages(job *store.Job) ([]jobs.StageDef, error) {
	var opt JobOptions
	if len(job.Options) > 0 {
		if err := json.Unmarshal(job.Options, &opt); err != nil {
			return nil, app.E(app.CodeInternal, "任务参数无法解析：%v", err)
		}
	}
	// Weights; a local file is copied, not downloaded.
	acquireWeight := 15.0
	if opt.SourceType == app.SourceFile || opt.SourceType == app.SourceUpload {
		acquireWeight = 2
	}
	return []jobs.StageDef{
		{Name: app.StageEnsureTools, Resource: jobs.ResourceNet, Weight: 2, Run: p.wrap(p.ensureTools)},
		{Name: app.StageAcquire, Deps: []string{app.StageEnsureTools}, Resource: acquireResource(opt.SourceType), Weight: acquireWeight, ReprocessGroups: groupsAcquire, Run: p.wrap(p.acquire)},
		{Name: app.StageProbe, Deps: []string{app.StageAcquire}, Resource: jobs.ResourceIO, Weight: 1, ReprocessGroups: groupsAcquire, Run: p.wrap(p.probe)},
		{Name: app.StageExtractAudio, Deps: []string{app.StageProbe}, Resource: jobs.ResourceMedia, Weight: 4, ReprocessGroups: groupsAcquire, Run: p.wrap(p.extractAudio)},
		{Name: app.StageTranscribe, Deps: []string{app.StageExtractAudio}, Resource: jobs.ResourceASR, Weight: 45, ReprocessGroups: groupsTranscribe, Run: p.wrap(p.transcribe)},
		{Name: app.StagePlayback, Deps: []string{app.StageProbe}, Resource: jobs.ResourceMedia, ExcludeFromProgress: true, ReprocessGroups: groupsAcquire, Run: p.wrap(p.playback)},
		{Name: app.StageNoteMap, Deps: []string{app.StageTranscribe}, Resource: jobs.ResourceLLM, Weight: 25, ReprocessGroups: groupsNote, Run: p.wrap(p.noteMap)},
		{Name: app.StageNoteReduce, Deps: []string{app.StageNoteMap}, Resource: jobs.ResourceLLM, Weight: 7, ReprocessGroups: groupsNote, Run: p.wrap(p.noteReduce)},
		{Name: app.StageNoteRender, Deps: []string{app.StageNoteReduce}, Resource: jobs.ResourceCPU, Weight: 1, ReprocessGroups: groupsNote, Run: p.wrap(p.noteRender)},
	}, nil
}

func acquireResource(sourceType string) string {
	if sourceType == app.SourceYouTube || sourceType == app.SourceBilibili {
		return jobs.ResourceNet
	}
	return jobs.ResourceIO
}

func (p *Pipeline) prepareToolsStages(*store.Job) ([]jobs.StageDef, error) {
	return []jobs.StageDef{{Name: app.StagePrepare, Resource: jobs.ResourceNet, Weight: 1, Run: p.wrap(p.prepareTools)}}, nil
}

// wrap labels child processes with the job and stage (so a forced cancel can
// kill them) and maps a full disk to DISK_FULL whatever tool reported it.
func (p *Pipeline) wrap(fn jobs.StageFunc) jobs.StageFunc {
	return func(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
		res, err := fn(procx.WithLabels(ctx, sc.JobID(), sc.Stage), sc)
		if err != nil && ctx.Err() == nil {
			tail := ""
			if e := app.AsError(err); e != nil {
				tail, _ = e.Details["stderr_tail"].(string)
			}
			err = media.WrapDiskFull(err, tail)
		}
		return res, err
	}
}

// Stage versions take part in the fingerprints: bump one when that stage's
// logic changes so artifacts made by the old logic are not reused.
const (
	acquireVersion    = 1
	probeVersion      = 1
	audioVersion      = 1
	asrVersion        = 1
	playbackVersion   = 1
	noteMapVersion    = 1
	noteReduceVersion = 1
	noteRenderVersion = 1
)
