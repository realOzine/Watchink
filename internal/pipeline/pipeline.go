// Package pipeline implements the stages of the process_video and
// prepare_tools jobs (docs/v0.md §7.2). Each stage computes an input
// fingerprint, reuses a registered artifact when one matches (§7.4) and
// otherwise does the work through the media, asr, llm and notes packages.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"watchink/internal/app"
	"watchink/internal/config"
	"watchink/internal/jobs"
	"watchink/internal/llm"
	"watchink/internal/media"
	"watchink/internal/procx"
	"watchink/internal/store"
	"watchink/internal/tools"
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
	// they start so a config change applies to the next retry (§7.4).
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

// Reprocess groups per stage (§7.5): a job with reprocess_from=X ignores the
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
	// Weights of §7.2; a local file is copied, not downloaded.
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

func options(sc *jobs.StageContext) (JobOptions, error) {
	var opt JobOptions
	err := sc.DecodeOptions(&opt)
	return opt, err
}

// cached returns the registered artifact for (kind, fp) when the stage may
// reuse it. hash re-verifies the sha256; large media files only get the
// cheap existence + size check.
func (p *Pipeline) cached(ctx context.Context, sc *jobs.StageContext, kind, fp string, hash bool) *store.Artifact {
	if sc.IgnoreCache {
		return nil
	}
	a, err := p.st.FindArtifact(ctx, kind, fp)
	if err != nil || a == nil || a.Purged {
		return nil
	}
	if hash {
		if !p.st.VerifyArtifact(a) {
			return nil
		}
	} else if !p.st.ArtifactPresent(a) {
		return nil
	}
	return a
}

func reused(a *store.Artifact) *jobs.StageResult {
	return &jobs.StageResult{ArtifactID: a.ID, Reused: true, Fingerprint: a.Fingerprint}
}

func done(a *store.Artifact) *jobs.StageResult {
	return &jobs.StageResult{ArtifactID: a.ID, Fingerprint: a.Fingerprint}
}

// depArtifact loads the artifact a dependency stage produced.
func (p *Pipeline) depArtifact(ctx context.Context, sc *jobs.StageContext, stage string) (*store.Artifact, error) {
	d, ok := sc.Dep(stage)
	if !ok || d.ArtifactID == "" {
		return nil, app.E(app.CodeInternal, "阶段 %s 没有登记产物", stage)
	}
	return p.st.GetArtifact(ctx, d.ArtifactID)
}

func (p *Pipeline) readJSON(a *store.Artifact, v any) error {
	b, err := readFile(p.st.Abs(a.Path))
	if err != nil {
		return app.E(app.CodeInternal, "读取产物 %s 失败：%v", a.Path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return app.E(app.CodeInternal, "解析产物 %s 失败：%v", a.Path, err)
	}
	return nil
}

// putJSON writes v under rel and registers it as an artifact.
func (p *Pipeline) putJSON(ctx context.Context, videoID, kind, fp, rel string, v any) (*store.Artifact, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	fi, err := p.st.WriteFileAtomic(rel, buf.Bytes())
	if err != nil {
		return nil, err
	}
	return p.st.PutArtifact(ctx, &store.Artifact{VideoID: videoID, Kind: kind, Fingerprint: fp, Path: rel, SHA256: fi.SHA256, SizeBytes: fi.Size})
}

// ASRModel returns the configured ASR model ID or the platform default.
func ASRModel(cfg app.Settings, t *tools.Manager) string {
	if cfg.ASR.Model != "" {
		return cfg.ASR.Model
	}
	return t.DefaultASRModel()
}

// HTTPClient returns a client honouring network.proxy (§11.4); an empty
// proxy follows the environment.
func HTTPClient(proxy string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr}
}

// LLMConfig builds the online model configuration from the settings and the
// resolved credential (D-14).
func LLMConfig(cfg app.Settings, acquire func(context.Context) (func(), error)) llm.Config {
	key, _ := config.ResolveLLMAPIKey(cfg)
	timeout, err := config.ParseDuration(cfg.LLM.Timeout)
	if err != nil || timeout <= 0 {
		timeout = 120 * time.Second
	}
	return llm.Config{
		BaseURL: cfg.LLM.BaseURL, Model: cfg.LLM.Model, APIKey: key,
		StructuredOutput: cfg.LLM.StructuredOutput, Temperature: cfg.LLM.Temperature,
		Timeout: timeout, HTTPClient: HTTPClient(cfg.Network.Proxy), Acquire: acquire,
	}
}

// LLMConfigured reports whether notes can be generated with cfg.
func LLMConfigured(cfg app.Settings) bool {
	return llm.Validate(LLMConfig(cfg, nil)) == nil
}

// --- ensure_tools / prepare_tools ---

func (p *Pipeline) ensureTools(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	opt, err := options(sc)
	if err != nil {
		return nil, err
	}
	cfg := p.cfg()
	model := ASRModel(cfg, p.tools)
	missing := p.missing(p.tools.Needed(opt.SourceType, model, cfg.ASR.VAD), model, cfg.ASR.VAD)
	if len(missing) == 0 {
		return &jobs.StageResult{}, nil
	}
	if !cfg.Tools.AutoPrepare {
		return nil, app.E(app.CodeToolMissing, "缺少工具或模型：%v", missing).
			WithHint("运行 watchink tools prepare，或在设置中开启自动准备").WithDetail("missing", missing)
	}
	return &jobs.StageResult{}, p.ensure(ctx, sc, missing)
}

func (p *Pipeline) prepareTools(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	opt, err := options(sc)
	if err != nil {
		return nil, err
	}
	return &jobs.StageResult{}, p.ensure(ctx, sc, opt.Items)
}

// missing filters names down to the items that are not ready.
func (p *Pipeline) missing(names []string, model string, vad bool) []string {
	ready := map[string]bool{}
	for _, it := range p.tools.Status(model, vad).Items {
		ready[it.Name] = it.Status == app.ToolReady
	}
	var out []string
	for _, n := range names {
		if !ready[n] {
			out = append(out, n)
		}
	}
	return out
}

// ensure installs names, reporting progress as the mean of the items.
func (p *Pipeline) ensure(ctx context.Context, sc *jobs.StageContext, names []string) error {
	if len(names) == 0 {
		return nil
	}
	frac := map[string]float64{}
	return p.tools.Ensure(ctx, names, func(name string, got, total int64) {
		if total > 0 {
			frac[name] = float64(got) / float64(total)
		}
		sum := 0.0
		for _, f := range frac {
			sum += f
		}
		sc.Progress(sum / float64(len(names)))
	})
}
