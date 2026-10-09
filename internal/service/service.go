// Package service implements app.Local, the in-process use cases of the
// engine (docs/v0.md §4.1 "app"): import and dedup, jobs, videos, notes,
// transcripts, playback, export, settings and tools. It owns the store, the
// scheduler, the event bus and the tool manager of one data directory.
package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"os"
	"sync"
	"time"

	"watchink/internal/app"
	"watchink/internal/config"
	"watchink/internal/events"
	"watchink/internal/jobs"
	"watchink/internal/pipeline"
	"watchink/internal/procx"
	"watchink/internal/store"
	"watchink/internal/tools"
)

// Options configures Open.
type Options struct {
	DataDir string
	Logger  *slog.Logger
	// AddSecret registers a value the log redactor must mask; may be nil.
	AddSecret func(string)
	// GuardExe is the watchink executable used to wrap child processes
	// (macOS orphan protection, §5.5); empty runs tools directly.
	GuardExe string
}

// Service is the engine-side implementation of app.Local.
type Service struct {
	dataDir   string
	log       *slog.Logger
	addSecret func(string)

	st     *store.Store
	bus    *events.Bus
	runner *procx.Runner
	tools  *tools.Manager
	sched  *jobs.Scheduler

	mediaKey []byte

	// Immutable rows kept in memory; see cache.go. Slices handed out by
	// these caches are shared and must not be modified.
	citations *lru[[]app.Citation] // by note ID
	segments  *lru[[]app.Segment]  // by transcript ID, in idx order

	mu       sync.RWMutex
	settings app.Settings
	baseURL  string

	stopOnce sync.Once
}

var _ app.Local = (*Service)(nil)

// Open loads the configuration, opens the database (running migrations) and
// performs restart recovery (§5.3 steps 2–4). The scheduler is not started:
// call Start once the engine is listening.
func Open(ctx context.Context, opts Options) (*Service, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	cfg, err := config.Load(opts.DataDir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, opts.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Service{dataDir: opts.DataDir, log: log, addSecret: opts.AddSecret, st: st, bus: events.New(0), settings: cfg, mediaKey: make([]byte, 32),
		citations: newLRU[[]app.Citation](64), segments: newLRU[[]app.Segment](16)}
	if _, err := rand.Read(s.mediaKey); err != nil {
		st.Close()
		return nil, err
	}
	s.registerSecret(cfg)

	s.runner = procx.New(procx.Options{
		GuardExe: opts.GuardExe,
		// child_processes lets the next engine run find children that
		// outlived a crash (§7.6 step 3).
		OnStart: func(pid int, exe, jobID, stage string) {
			_ = st.AddChildProcess(context.Background(), store.ChildProcess{PID: pid, JobID: jobID, Stage: stage, Exe: exe, StartedAt: app.Now()})
		},
		OnExit: func(pid int) { _ = st.RemoveChildProcess(context.Background(), pid) },
	})
	s.tools, err = tools.New(tools.Options{
		DataDir: opts.DataDir, Exec: s.exec, Mirrors: cfg.Download.Mirrors, Proxy: cfg.Network.Proxy, Logger: log,
		OnChange: func() { s.publishTools() },
	})
	if err != nil {
		st.Close()
		return nil, err
	}
	s.sched = jobs.New(jobs.Options{
		Store: st, Publisher: s.bus, Logger: log,
		ForceKill:    s.runner.KillJob,
		OnJobChanged: func(j app.Job) { s.jobChanged(j) },
	})
	pipeline.New(pipeline.Deps{
		Store: st, Runner: s.runner, Tools: s.tools, Settings: s.Settings, Logger: log,
		VideoChanged: s.publishVideo,
	}).Register(s.sched)

	if err := s.recover(ctx, cfg.Engine.AutoResume); err != nil {
		st.Close()
		return nil, err
	}
	return s, nil
}

// recover applies §7.6: fix states left by a dead engine, sweep orphan
// files and kill children that survived it.
func (s *Service) recover(ctx context.Context, autoResume bool) error {
	res, err := s.st.Recover(ctx, autoResume)
	if err != nil {
		return err
	}
	if ids := res.JobIDs(); len(ids) > 0 {
		s.log.Info("recovered jobs", "cancelled", len(res.Cancelled), "interrupted", len(res.Interrupted), "requeued", len(res.Requeued))
	}
	if _, err := s.st.SweepOrphans(ctx); err != nil {
		s.log.Warn("sweep orphans failed", "error", err)
	}
	children, err := s.st.ListChildProcesses(ctx)
	if err != nil {
		return err
	}
	for _, c := range children {
		// Only a process whose pid AND executable both match is ours.
		if killed, err := procx.KillIfMatches(c.PID, c.Exe); err != nil {
			s.log.Warn("kill leftover child failed", "pid", c.PID, "error", err)
		} else if killed {
			s.log.Info("killed leftover child", "pid", c.PID, "exe", c.Exe)
		}
	}
	return s.st.ClearChildProcesses(ctx)
}

// exec runs a tool for the tool manager's smoke tests through procx, the
// only package allowed to create processes.
func (s *Service) exec(ctx context.Context, path string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	var mu sync.Mutex
	w := lockedWriter{mu: &mu, buf: &out}
	_, err := s.runner.Run(ctx, procx.Cmd{Path: path, Args: args, Stdout: w, Stderr: w})
	return out.Bytes(), err
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// Settings returns the current settings snapshot.
func (s *Service) Settings() app.Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

// WebEnabled implements app.Local.
func (s *Service) WebEnabled() bool { return s.Settings().Web.Enabled }

// Port returns the configured engine port.
func (s *Service) Port() int { return s.Settings().Engine.Port }

// IdleExit returns the configured idle-exit duration (§5.4).
func (s *Service) IdleExit() time.Duration {
	d, err := config.ParseDuration(s.Settings().Engine.IdleExit)
	if err != nil {
		return 0
	}
	return d
}

// SetBaseURL records the address the engine listens on; it is needed for
// web_url, playback and download URLs.
func (s *Service) SetBaseURL(u string) {
	s.mu.Lock()
	s.baseURL = u
	s.mu.Unlock()
}

func (s *Service) base() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.baseURL
}

// Start starts the scheduler (§5.3 step 7).
func (s *Service) Start(ctx context.Context) error { return s.sched.Start(ctx) }

// Close releases the database; call it after Stop.
func (s *Service) Close() error {
	s.bus.Close()
	return s.st.Close()
}

// Stop implements app.Local. It may be called more than once, also
// concurrently (a forced stop while a graceful one is waiting).
func (s *Service) Stop(ctx context.Context, force bool) error {
	s.stopOnce.Do(func() {
		reason := "shutdown"
		s.bus.Publish(app.Event{Type: app.EventEngineStopping, Reason: reason})
	})
	if force {
		s.runner.KillAll()
	}
	return s.sched.Stop(ctx, force)
}

// ActiveJobs implements app.Local.
func (s *Service) ActiveJobs(ctx context.Context) (int, error) { return s.sched.ActiveCount(ctx) }

// EngineInfo implements app.Service; the engine overlays pid, mode, etc.
func (s *Service) EngineInfo(ctx context.Context) (*app.EngineInfo, error) {
	cfg := s.Settings()
	active, err := s.sched.ActiveCount(ctx)
	if err != nil {
		return nil, err
	}
	return &app.EngineInfo{
		API: app.APIVersion, PID: os.Getpid(), DataDir: s.dataDir, BaseURL: s.base(), ActiveJobs: active,
		ToolsReady:    s.tools.Status(pipeline.ASRModel(cfg, s.tools), cfg.ASR.VAD).Ready,
		LLMConfigured: pipeline.LLMConfigured(cfg),
	}, nil
}

// Subscribe implements app.Service.
func (s *Service) Subscribe(ctx context.Context, afterID int64) (<-chan app.Event, bool, error) {
	ch, reset := s.bus.Subscribe(ctx, afterID)
	return ch, reset, nil
}

func (s *Service) jobChanged(j app.Job) {
	if j.Status == app.JobFailed && j.Error != nil {
		// The reason a job failed belongs in the engine log, not only in
		// the database (the logger redacts credentials).
		s.log.Warn("job failed", "job_id", j.JobID, "stage", j.Error.Stage, "code", j.Error.Code,
			"message", j.Error.Message, "details", j.Error.Details)
	}
	if j.VideoID != nil {
		s.publishVideo(*j.VideoID)
	}
	if j.Kind == app.JobKindPrepareTools {
		s.publishTools()
	}
}

func (s *Service) publishVideo(videoID string) {
	v, err := s.GetVideo(context.Background(), videoID)
	if err != nil {
		return
	}
	s.bus.Publish(app.Event{Type: app.EventVideoUpdated, Video: v})
}

func (s *Service) publishTools() {
	st, err := s.GetTools(context.Background())
	if err != nil {
		return
	}
	s.bus.Publish(app.Event{Type: app.EventToolsUpdated, Tools: st})
}

// registerSecret hands the current API key to the log redactor.
func (s *Service) registerSecret(cfg app.Settings) {
	if s.addSecret == nil {
		return
	}
	if key, _ := config.ResolveLLMAPIKey(cfg); key != "" {
		s.addSecret(key)
	}
}
