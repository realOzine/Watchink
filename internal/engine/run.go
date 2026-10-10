package engine

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"watchink/internal/api"
	"watchink/internal/app"
)

// DefaultIdleExit is the background-mode idle timeout.
const DefaultIdleExit = 10 * time.Minute

// Env is what Options.Build receives.
type Env struct {
	// DataDir is the absolute data directory the engine owns.
	DataDir string
	// Logger is the engine logger (JSON lines, redacted).
	Logger *slog.Logger
	// AddSecret registers a value (e.g. the LLM API key) that the log
	// redactor must mask.
	AddSecret func(secret string)
}

// Built is the service assembled by Options.Build.
type Built struct {
	// Service is served over HTTP.
	Service app.Local
	// Port is the configured engine.port; 0 means the default 47823.
	Port int
	// IdleExit is the configured engine.idle_exit; 0 means 10 minutes, a
	// negative value disables idle exit.
	IdleExit time.Duration
	// WebUI is the built SPA (may be nil).
	WebUI fs.FS
	// SetBaseURL, when set, is told the final http://127.0.0.1:<port> before
	// Start (the service needs it for web_url, playback and download URLs).
	SetBaseURL func(baseURL string)
	// Start, when set, starts the scheduler. It is called after engine.json
	// has been written (step 7); its ctx stays alive until Service.Stop
	// has returned.
	Start func(ctx context.Context) error
	// Close, when set, releases the store and other resources at exit.
	Close func() error
}

// Options configures Run.
type Options struct {
	// DataDir is the resolved data directory (required).
	DataDir string
	// Mode is ModeBackground (`engine run`, idles out) or ModeForeground
	// (`serve`, also logs to Stderr and never idles out). Empty = background.
	Mode string
	// Port overrides the listen port: 0 uses Built.Port (then 47823), a
	// negative value asks the OS for a free port. A busy port always falls
	// back to a free one (step 5).
	Port int
	// Version and BuildID identify the build (internal/version).
	Version string
	BuildID string
	// Stderr receives human-readable log lines in foreground mode.
	Stderr io.Writer
	// Build assembles the service after the lock is held: config, store,
	// migrations and restart recovery happen inside it (steps 2–4).
	Build func(ctx context.Context, env Env) (*Built, error)

	// OnReady, when set, is called once the engine accepts requests.
	OnReady func(d Discovery)
	// AllowedOrigins overrides the CORS origins (nil = api defaults).
	AllowedOrigins []string
	// LogLevel is the minimum log level; nil means Info.
	LogLevel slog.Leveler
	// NoSignals disables the SIGINT / SIGTERM handling (tests, embedding).
	NoSignals bool
	// IdleCheckInterval is how often the idle condition is evaluated; zero
	// picks min(15 s, idle timeout / 4).
	IdleCheckInterval time.Duration
	// HeartbeatInterval is the event-stream heartbeat; zero means 15 s.
	HeartbeatInterval time.Duration
	// ShutdownTimeout bounds the HTTP server shutdown; zero means 5 s.
	ShutdownTimeout time.Duration
}

// stopRequest is one request to stop the engine.
type stopRequest struct {
	reason string
	force  bool
}

// Run runs an engine for opts.DataDir until ctx is cancelled, a termination
// signal arrives, POST /api/v1/engine/shutdown is called, or (background
// mode) the engine has been idle for the configured time. It follows the
// start order and returns ErrAlreadyRunning when another engine owns
// the data directory. A clean stop returns nil.
func Run(ctx context.Context, opts Options) (err error) {
	if err := opts.normalize(); err != nil {
		return err
	}

	// 1. The lock decides who the engine is.
	lock, err := acquireLock(opts.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	lo := LoggerOptions{Level: opts.LogLevel}
	if opts.Mode == ModeForeground {
		lo.Stderr = opts.Stderr
	}
	lg, err := NewLogger(opts.DataDir, lo)
	if err != nil {
		return err
	}
	defer lg.Close()
	log := lg.Logger

	// 2–4. Config, store, migrations, recovery.
	built, err := opts.Build(ctx, Env{DataDir: opts.DataDir, Logger: log, AddSecret: lg.AddSecret})
	if err != nil {
		log.Error("engine build failed", "error", err.Error())
		return err
	}
	if built == nil || built.Service == nil {
		return errors.New("engine: Build returned no service")
	}
	var closeOnce sync.Once
	closeBuilt := func() {
		closeOnce.Do(func() {
			if built.Close == nil {
				return
			}
			if cerr := built.Close(); cerr != nil {
				log.Error("engine close failed", "error", cerr.Error())
			}
		})
	}
	defer closeBuilt()

	// 5. Listen on loopback only.
	ln, err := listen(opts.Port, built.Port, log)
	if err != nil {
		log.Error("engine listen failed", "error", err.Error())
		return err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)

	token, err := newToken()
	if err != nil {
		return err
	}
	lg.AddSecret(token)

	started := time.Now()
	disc := Discovery{
		PID:       os.Getpid(),
		BaseURL:   baseURL,
		Token:     token,
		Version:   opts.Version,
		BuildID:   opts.BuildID,
		API:       app.APIVersion,
		Mode:      opts.Mode,
		StartedAt: app.FormatTime(started),
	}

	stopCh := make(chan stopRequest, 8)
	requestStop := func(r stopRequest) {
		select {
		case stopCh <- r:
		default:
		}
	}

	handler := newHandler(opts, built, disc, port, log, func(force bool) {
		requestStop(stopRequest{reason: "shutdown requested", force: force})
	})
	defer handler.Close()
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		// No ReadTimeout / WriteTimeout: uploads, media and the event stream
		// are legitimately long-lived.
	}

	// 6. Publish the address and token.
	if err := writeDiscovery(opts.DataDir, disc); err != nil {
		log.Error("engine discovery write failed", "error", err.Error())
		return fmt.Errorf("engine: write engine.json: %w", err)
	}
	defer os.Remove(DiscoveryPath(opts.DataDir))

	// 7. Start the scheduler and serve. The scheduler context outlives ctx so
	// that a cancelled ctx still goes through Service.Stop first.
	if built.SetBaseURL != nil {
		built.SetBaseURL(baseURL)
	}
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	if built.Start != nil {
		if err := built.Start(runCtx); err != nil {
			log.Error("engine start failed", "error", err.Error())
			return err
		}
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	log.Info("engine ready", "base_url", baseURL, "pid", disc.PID, "mode", opts.Mode,
		"version", opts.Version, "build_id", opts.BuildID, "data_dir", opts.DataDir)
	if opts.OnReady != nil {
		opts.OnReady(disc)
	}

	var sigCh chan os.Signal
	if !opts.NoSignals {
		sigCh = make(chan os.Signal, 2)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)
	}
	idle := func() bool { return false }
	if timeout := idleTimeout(opts.Mode, built.IdleExit); timeout > 0 {
		idle = func() bool { return isIdle(runCtx, built.Service, handler, started, timeout) }
	}
	stop, runErr := waitForStop(ctx, stopCh, sigCh, serveErr, idle, idleCheckInterval(opts, built))
	log.Info("engine stopping", "reason", stop.reason, "force", stop.force)

	// Stop the service. A second signal or a later force request
	// upgrades a graceful stop that is still waiting for running stages.
	stopService(runCtx, built.Service, stop.force, stopCh, sigCh, log)

	handler.Close()
	shCtx, cancelSh := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
	if err := srv.Shutdown(shCtx); err != nil {
		_ = srv.Close()
	}
	cancelSh()
	cancelRun()
	closeBuilt()
	log.Info("engine stopped", "reason", stop.reason)
	// Deferred, in order: remove engine.json, close the listener and the
	// log, release the lock last.
	return runErr
}

// normalize validates the options and fills the defaults.
func (o *Options) normalize() error {
	if o.DataDir == "" {
		return errors.New("engine: DataDir is required")
	}
	if o.Build == nil {
		return errors.New("engine: Build is required")
	}
	switch o.Mode {
	case "":
		o.Mode = ModeBackground
	case ModeBackground, ModeForeground:
	default:
		return fmt.Errorf("engine: unknown mode %q", o.Mode)
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = 5 * time.Second
	}
	return nil
}

// newToken returns the per-run bearer token: 32 random bytes, base64url.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("engine: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// newHandler builds the HTTP API over the service. The engine owns the
// process identity, so it fills those fields of EngineInfo itself.
func newHandler(opts Options, built *Built, disc Discovery, port int, log *slog.Logger, onShutdown func(force bool)) *api.Handler {
	return api.NewHandler(api.Options{
		Service:           built.Service,
		Token:             disc.Token,
		Port:              port,
		WebUI:             built.WebUI,
		Logger:            log,
		AllowedOrigins:    opts.AllowedOrigins,
		HeartbeatInterval: opts.HeartbeatInterval,
		OnShutdown:        onShutdown,
		EngineInfo: func(info *app.EngineInfo) {
			info.PID = disc.PID
			info.BaseURL = disc.BaseURL
			info.Mode = disc.Mode
			info.StartedAt = disc.StartedAt
			info.Version = disc.Version
			info.BuildID = disc.BuildID
			info.API = disc.API
			if info.DataDir == "" {
				info.DataDir = opts.DataDir
			}
		},
	})
}

// waitForStop blocks until something asks the engine to stop and says why:
// the caller's context, a signal, a shutdown request, a dead HTTP server
// (the only case with an error) or the idle check.
func waitForStop(ctx context.Context, stopCh <-chan stopRequest, sigCh <-chan os.Signal, serveErr <-chan error, idle func() bool, idleEvery time.Duration) (stopRequest, error) {
	var idleTick <-chan time.Time
	if idleEvery > 0 {
		t := time.NewTicker(idleEvery)
		defer t.Stop()
		idleTick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return stopRequest{reason: "context cancelled"}, nil
		case sig := <-sigCh:
			return stopRequest{reason: "signal " + sig.String()}, nil
		case r := <-stopCh:
			return r, nil
		case err := <-serveErr:
			return stopRequest{reason: "http server error", force: true}, fmt.Errorf("engine: http server stopped: %w", err)
		case <-idleTick:
			if idle() {
				return stopRequest{reason: "idle"}, nil
			}
		}
	}
}

// idleCheckInterval is how often the idle condition is evaluated, or 0 when
// this engine never idles out.
func idleCheckInterval(opts Options, built *Built) time.Duration {
	idle := idleTimeout(opts.Mode, built.IdleExit)
	if idle <= 0 {
		return 0
	}
	if opts.IdleCheckInterval > 0 {
		return opts.IdleCheckInterval
	}
	return min(15*time.Second, max(idle/4, 10*time.Millisecond))
}

// listen binds 127.0.0.1 on the wanted port, falling back to a port chosen
// by the OS when it is taken (step 5).
func listen(optPort, cfgPort int, log *slog.Logger) (net.Listener, error) {
	port := optPort
	if port == 0 {
		port = cfgPort
	}
	if port == 0 {
		port = app.DefaultEnginePort
	}
	if port > 0 {
		ln, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			return ln, nil
		}
		log.Warn("engine port unavailable, using a random port", "port", port, "error", err.Error())
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("engine: listen on 127.0.0.1: %w", err)
	}
	return ln, nil
}

// idleTimeout returns the idle-exit duration, or 0 when idle exit is off:
// only background engines idle out.
func idleTimeout(mode string, configured time.Duration) time.Duration {
	if mode != ModeBackground || configured < 0 {
		return 0
	}
	if configured == 0 {
		return DefaultIdleExit
	}
	return configured
}

// isIdle evaluates the three conditions: no queued or running job,
// no event-stream subscriber, and no API request for the idle duration.
func isIdle(ctx context.Context, svc app.Local, h *api.Handler, started time.Time, idle time.Duration) bool {
	st := h.Stats()
	if st.Subscribers > 0 || st.InFlight > 0 {
		return false
	}
	last := started
	if st.LastRequest.After(last) {
		last = st.LastRequest
	}
	if time.Since(last) < idle {
		return false
	}
	n, err := svc.ActiveJobs(ctx)
	return err == nil && n == 0
}

func stopService(ctx context.Context, svc app.Local, force bool, stopCh <-chan stopRequest, sigCh <-chan os.Signal, log *slog.Logger) {
	// The service waits up to 10 s for running stages on a graceful stop.
	stopCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	call := func(force bool) <-chan error {
		done := make(chan error, 1)
		go func() { done <- svc.Stop(stopCtx, force) }()
		return done
	}
	done := call(force)
	for {
		select {
		case err := <-done:
			if err != nil {
				log.Error("service stop failed", "error", err.Error())
			}
			return
		case r := <-stopCh:
			if r.force && !force {
				force = true
				log.Info("engine stop upgraded to force")
				done = call(true)
			}
		case <-sigCh:
			if !force {
				force = true
				log.Info("engine stop upgraded to force", "reason", "second signal")
				done = call(true)
			}
		case <-stopCtx.Done():
			log.Error("service stop timed out")
			return
		}
	}
}
