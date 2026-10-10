package engine_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"watchink/internal/app"
	"watchink/internal/engine"
	"watchink/internal/transport/api/apitest"
)

// running is an in-process engine under test.
type running struct {
	t      *testing.T
	dir    string
	fake   *apitest.Fake
	disc   engine.Discovery
	cancel context.CancelFunc
	done   chan error

	mu    sync.Mutex
	steps []string
}

func (r *running) step(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, s)
}

func (r *running) order() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.steps, ",")
}

// start runs an engine and waits until it is ready.
func start(t *testing.T, dir string, mutate func(o *engine.Options, b *engine.Built)) *running {
	t.Helper()
	r := &running{t: t, dir: dir, fake: apitest.New(), done: make(chan error, 1)}
	ready := make(chan engine.Discovery, 1)
	built := &engine.Built{
		Service:    r.fake,
		IdleExit:   -1,
		SetBaseURL: func(u string) { r.step("base_url") },
		Start: func(context.Context) error {
			if _, err := os.Stat(engine.DiscoveryPath(dir)); err != nil {
				t.Errorf("Start called before engine.json exists: %v", err)
			}
			r.step("start")
			return nil
		},
		Close: func() error {
			if stopped, _ := r.fake.Stopped(); !stopped {
				t.Error("Close called before Service.Stop")
			}
			if _, err := os.Stat(engine.DiscoveryPath(dir)); err != nil {
				t.Errorf("engine.json removed before Close: %v", err)
			}
			r.step("close")
			return nil
		},
	}
	opts := engine.Options{
		DataDir:   dir,
		Port:      -1,
		Version:   "9.9.9",
		BuildID:   "build-A",
		NoSignals: true,
		OnReady:   func(d engine.Discovery) { ready <- d },
	}
	opts.Build = func(_ context.Context, env engine.Env) (*engine.Built, error) {
		if !engine.IsRunning(dir) {
			t.Error("Build called without the lock held")
		}
		if env.DataDir != dir || env.Logger == nil || env.AddSecret == nil {
			t.Errorf("env = %+v", env)
		}
		r.step("build")
		return built, nil
	}
	if mutate != nil {
		mutate(&opts, built)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.done <- engine.Run(ctx, opts) }()
	select {
	case r.disc = <-ready:
	case err := <-r.done:
		cancel()
		t.Fatalf("engine exited before ready: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("engine not ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Error("engine did not stop")
		}
	})
	return r
}

// wait waits for Run to return.
func (r *running) wait(d time.Duration) (error, bool) {
	select {
	case err := <-r.done:
		r.done <- err // keep it for Cleanup
		return err, true
	case <-time.After(d):
		return nil, false
	}
}

func (r *running) request(method, path, body string) (*http.Response, string) {
	r.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, r.disc.BaseURL+path, rd)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.disc.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestRunLifecycleAndDiscovery(t *testing.T) {
	dir := t.TempDir()
	before := time.Now().Add(-time.Second)
	r := start(t, dir, nil)

	// engine.json: exactly the fields, mode 0600 (Windows has no
	// permission bits; the file is protected by the profile directory's ACL).
	raw, err := os.ReadFile(engine.DiscoveryPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pid", "base_url", "token", "version", "build_id", "api", "mode", "started_at"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("engine.json misses %q", k)
		}
		delete(fields, k)
	}
	if len(fields) != 0 {
		t.Errorf("engine.json has extra fields: %v", fields)
	}
	st, _ := os.Stat(engine.DiscoveryPath(dir))
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("engine.json mode = %v", st.Mode().Perm())
	}
	d, err := engine.ReadDiscovery(dir)
	if err != nil {
		t.Fatal(err)
	}
	if *d != r.disc {
		t.Fatalf("ReadDiscovery = %+v, OnReady got %+v", d, r.disc)
	}
	tok, err := base64.RawURLEncoding.DecodeString(d.Token)
	if err != nil || len(tok) != 32 {
		t.Fatalf("token is not 32 random bytes in base64url: %v", err)
	}
	started, err := time.Parse(time.RFC3339Nano, d.StartedAt)
	if err != nil || started.Before(before) || started.After(time.Now()) {
		t.Fatalf("started_at = %q", d.StartedAt)
	}
	if d.PID != os.Getpid() || d.Version != "9.9.9" || d.BuildID != "build-A" || d.API != 1 ||
		d.Mode != engine.ModeBackground || !strings.HasPrefix(d.BaseURL, "http://127.0.0.1:") {
		t.Fatalf("discovery = %+v", d)
	}
	if leftovers, _ := os.ReadDir(engine.RunDir(dir)); len(leftovers) != 2 {
		t.Fatalf("run dir holds %d entries, want engine.lock + engine.json", len(leftovers))
	}
	if !engine.IsRunning(dir) {
		t.Fatal("IsRunning = false while the engine runs")
	}

	// Health is open; the engine endpoint overlays the process fields.
	resp, err := http.Get(d.BaseURL + "/api/v1/health")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %v %v", resp, err)
	}
	resp.Body.Close()
	resp, body := r.request("GET", "/api/v1/engine", "")
	var info app.EngineInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil || resp.StatusCode != 200 {
		t.Fatalf("engine: %d %s", resp.StatusCode, body)
	}
	if info.PID != d.PID || info.BaseURL != d.BaseURL || info.Mode != d.Mode || info.StartedAt != d.StartedAt ||
		info.Version != d.Version || info.BuildID != d.BuildID || info.API != 1 ||
		info.DataDir != "/fake/data" || !info.ToolsReady {
		t.Fatalf("engine info = %+v", info)
	}
	// A wrong token is refused, and the Host check is active on the real port.
	req, _ := http.NewRequest("GET", d.BaseURL+"/api/v1/engine", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", d.BaseURL+"/api/v1/health", nil)
	req.Host = "127.0.0.1:1"
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("wrong host port: %d", resp.StatusCode)
	}

	// Context cancellation is a graceful stop.
	r.cancel()
	if err, ok := r.wait(10 * time.Second); !ok || err != nil {
		t.Fatalf("Run after cancel: %v (returned %v)", err, ok)
	}
	if got := r.order(); got != "build,base_url,start,close" {
		t.Fatalf("order = %s", got)
	}
	if stopped, force := r.fake.Stopped(); !stopped || len(force) != 1 || force[0] {
		t.Fatalf("Stop calls = %v %v", stopped, force)
	}
	if _, err := os.Stat(engine.DiscoveryPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("engine.json still present: %v", err)
	}
	if _, err := engine.ReadDiscovery(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadDiscovery after stop: %v", err)
	}
	if engine.IsRunning(dir) {
		t.Fatal("lock still held after Run returned")
	}
	// The token never reaches the log.
	logData, err := os.ReadFile(engine.LogPath(dir))
	if err != nil || !bytes.Contains(logData, []byte(`"msg":"engine ready"`)) {
		t.Fatalf("log: %v\n%s", err, logData)
	}
	if bytes.Contains(logData, []byte(d.Token)) {
		t.Fatal("engine token leaked into the log")
	}
}

func TestLockExclusivity(t *testing.T) {
	dir := t.TempDir()
	if engine.IsRunning(dir) {
		t.Fatal("IsRunning on an empty dir")
	}
	r := start(t, dir, nil)

	built := false
	err := engine.Run(context.Background(), engine.Options{
		DataDir: dir, Port: -1, NoSignals: true,
		Build: func(context.Context, engine.Env) (*engine.Built, error) {
			built = true
			return &engine.Built{Service: apitest.New()}, nil
		},
	})
	if !errors.Is(err, engine.ErrAlreadyRunning) || built {
		t.Fatalf("second Run: err=%v built=%v", err, built)
	}
	// The loser must not disturb the winner's discovery file.
	if d, err := engine.ReadDiscovery(dir); err != nil || *d != r.disc {
		t.Fatalf("discovery after losing start: %+v %v", d, err)
	}
	if resp, _ := r.request("GET", "/api/v1/engine", ""); resp.StatusCode != 200 {
		t.Fatal("winner stopped serving")
	}

	r.cancel()
	r.wait(10 * time.Second)
	// The directory is free again.
	r2 := start(t, dir, nil)
	if r2.disc.Token == r.disc.Token {
		t.Fatal("token reused across engine runs")
	}
}

func TestStaleDiscoveryIsIgnoredAndReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(engine.RunDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := `{"pid":1,"base_url":"http://127.0.0.1:1","token":"stale","version":"0","build_id":"old","api":1,"mode":"background","started_at":"2020-01-01T00:00:00Z"}`
	if err := os.WriteFile(engine.DiscoveryPath(dir), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	// Liveness is the lock only: a discovery file alone means nothing.
	if engine.IsRunning(dir) {
		t.Fatal("IsRunning = true with only a stale engine.json")
	}
	r := start(t, dir, nil)
	d, err := engine.ReadDiscovery(dir)
	if err != nil || d.Token == "stale" || *d != r.disc {
		t.Fatalf("stale discovery not replaced: %+v %v", d, err)
	}
	if st, _ := os.Stat(engine.DiscoveryPath(dir)); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}

	// Unreadable files are errors, not panics.
	bad := t.TempDir()
	_ = os.MkdirAll(engine.RunDir(bad), 0o700)
	for _, content := range []string{"{", `{"pid":1}`} {
		_ = os.WriteFile(engine.DiscoveryPath(bad), []byte(content), 0o600)
		if _, err := engine.ReadDiscovery(bad); err == nil {
			t.Fatalf("ReadDiscovery accepted %q", content)
		}
	}
}

func TestShutdownEndpoint(t *testing.T) {
	for _, force := range []bool{false, true} {
		dir := t.TempDir()
		r := start(t, dir, nil)

		// A subscriber sees engine.stopping before its stream ends.
		req, _ := http.NewRequest("GET", r.disc.BaseURL+"/api/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+r.disc.Token)
		stream, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return r.fake.SubscriberCount() == 1 })

		body := `{"force":false}`
		if force {
			body = `{"force":true}`
		}
		resp, _ := r.request("POST", "/api/v1/engine/shutdown", body)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("shutdown: %d", resp.StatusCode)
		}
		all, _ := io.ReadAll(stream.Body)
		stream.Body.Close()
		if !strings.Contains(string(all), "event: engine.stopping") {
			t.Fatalf("stream ended without engine.stopping: %q", all)
		}
		if err, ok := r.wait(10 * time.Second); !ok || err != nil {
			t.Fatalf("Run after shutdown: %v (returned %v)", err, ok)
		}
		if stopped, kinds := r.fake.Stopped(); !stopped || len(kinds) != 1 || kinds[0] != force {
			t.Fatalf("Stop(force=%v) calls = %v", force, kinds)
		}
		if engine.IsRunning(dir) {
			t.Fatal("lock held after shutdown")
		}
		if _, err := os.Stat(engine.DiscoveryPath(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("engine.json left behind")
		}
		// The port is closed.
		if c, err := net.DialTimeout("tcp", strings.TrimPrefix(r.disc.BaseURL, "http://"), time.Second); err == nil {
			c.Close()
			t.Fatal("listener still open after shutdown")
		}
	}
}

func TestShutdownIsNotAvailableWithoutToken(t *testing.T) {
	r := start(t, t.TempDir(), nil)
	resp, err := http.Post(r.disc.BaseURL+"/api/v1/engine/shutdown", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if _, stopped := r.wait(200 * time.Millisecond); stopped {
		t.Fatal("engine stopped by an unauthenticated request")
	}
}

func idleOpts(idle time.Duration) func(o *engine.Options, b *engine.Built) {
	return func(o *engine.Options, b *engine.Built) {
		b.IdleExit = idle
		o.IdleCheckInterval = 10 * time.Millisecond
		o.HeartbeatInterval = 20 * time.Millisecond
	}
}

func TestIdleExitFires(t *testing.T) {
	dir := t.TempDir()
	began := time.Now()
	r := start(t, dir, idleOpts(150*time.Millisecond))
	err, ok := r.wait(5 * time.Second)
	if !ok || err != nil {
		t.Fatalf("engine did not idle out: %v", err)
	}
	if d := time.Since(began); d < 150*time.Millisecond {
		t.Fatalf("idled out after only %v", d)
	}
	if stopped, kinds := r.fake.Stopped(); !stopped || kinds[0] {
		t.Fatalf("idle exit must stop gracefully: %v", kinds)
	}
	if engine.IsRunning(dir) {
		t.Fatal("lock held after idle exit")
	}
	logData, _ := os.ReadFile(engine.LogPath(dir))
	if !bytes.Contains(logData, []byte(`"reason":"idle"`)) {
		t.Fatalf("log does not record the idle exit:\n%s", logData)
	}
}

func TestIdleExitBlockedByActiveJobs(t *testing.T) {
	r := start(t, t.TempDir(), func(o *engine.Options, b *engine.Built) {
		idleOpts(100*time.Millisecond)(o, b)
	})
	// start() returns within the idle window; a queued job now blocks exit.
	job := r.fake.AddJob(app.Job{Status: app.JobRunning})
	if _, stopped := r.wait(500 * time.Millisecond); stopped {
		t.Fatal("engine idled out with a running job")
	}
	r.fake.UpdateJob(job.JobID, func(j *app.Job) { j.Status = app.JobSucceeded })
	if err, ok := r.wait(5 * time.Second); !ok || err != nil {
		t.Fatalf("engine did not idle out after the job finished: %v", err)
	}
}

func TestIdleExitBlockedBySubscribers(t *testing.T) {
	r := start(t, t.TempDir(), idleOpts(100*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", r.disc.BaseURL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+r.disc.Token)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	// Keep reading so heartbeats flow, as a real subscriber would.
	go func() {
		sc := bufio.NewScanner(stream.Body)
		for sc.Scan() {
		}
	}()
	if _, stopped := r.wait(500 * time.Millisecond); stopped {
		t.Fatal("engine idled out with an event-stream subscriber")
	}
	cancel()
	if err, ok := r.wait(5 * time.Second); !ok || err != nil {
		t.Fatalf("engine did not idle out after the subscriber left: %v", err)
	}
}

func TestIdleExitDeferredByRequests(t *testing.T) {
	r := start(t, t.TempDir(), idleOpts(250*time.Millisecond))
	for i := 0; i < 8; i++ {
		time.Sleep(80 * time.Millisecond)
		if _, stopped := r.wait(0); stopped {
			t.Fatalf("engine idled out although requests kept arriving (iteration %d)", i)
		}
		r.request("GET", "/api/v1/tools", "")
	}
	if err, ok := r.wait(5 * time.Second); !ok || err != nil {
		t.Fatalf("engine did not idle out once requests stopped: %v", err)
	}
}

func TestForegroundNeverIdlesOutAndLogsToStderr(t *testing.T) {
	var stderr syncBuffer
	dir := t.TempDir()
	r := start(t, dir, func(o *engine.Options, b *engine.Built) {
		idleOpts(50*time.Millisecond)(o, b)
		o.Mode = engine.ModeForeground
		o.Stderr = &stderr
	})
	if _, stopped := r.wait(400 * time.Millisecond); stopped {
		t.Fatal("foreground engine idled out")
	}
	if r.disc.Mode != engine.ModeForeground {
		t.Fatalf("mode = %q", r.disc.Mode)
	}
	out := stderr.String()
	if !strings.Contains(out, `msg="engine ready"`) || strings.Contains(out, r.disc.Token) {
		t.Fatalf("stderr: %q", out)
	}
}

func TestPortFallbackWhenBusy(t *testing.T) {
	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	// Configured through Built.Port (engine.port) …
	r := start(t, t.TempDir(), func(o *engine.Options, b *engine.Built) { o.Port = 0; b.Port = port })
	if r.disc.BaseURL == "http://"+busy.Addr().String() {
		t.Fatal("engine claims the busy port")
	}
	if resp, _ := r.request("GET", "/api/v1/engine", ""); resp.StatusCode != 200 {
		t.Fatalf("engine on fallback port: %d", resp.StatusCode)
	}
	// … or through Options.Port.
	r2 := start(t, t.TempDir(), func(o *engine.Options, b *engine.Built) { o.Port = port })
	if r2.disc.BaseURL == "http://"+busy.Addr().String() || r2.disc.BaseURL == r.disc.BaseURL {
		t.Fatalf("fallback base URL %q", r2.disc.BaseURL)
	}

	// A free configured port is used as is.
	free, _ := net.Listen("tcp4", "127.0.0.1:0")
	freePort := free.Addr().(*net.TCPAddr).Port
	free.Close()
	r3 := start(t, t.TempDir(), func(o *engine.Options, b *engine.Built) { o.Port = 0; b.Port = freePort })
	if want := "http://" + free.Addr().String(); r3.disc.BaseURL != want {
		t.Fatalf("base URL %q, want %q", r3.disc.BaseURL, want)
	}
}

func TestBuildAndStartFailuresReleaseEverything(t *testing.T) {
	boom := errors.New("boom")
	dir := t.TempDir()
	err := engine.Run(context.Background(), engine.Options{
		DataDir: dir, Port: -1, NoSignals: true,
		Build: func(context.Context, engine.Env) (*engine.Built, error) { return nil, boom },
	})
	if !errors.Is(err, boom) || engine.IsRunning(dir) {
		t.Fatalf("build failure: err=%v running=%v", err, engine.IsRunning(dir))
	}
	closed := false
	err = engine.Run(context.Background(), engine.Options{
		DataDir: dir, Port: -1, NoSignals: true,
		Build: func(context.Context, engine.Env) (*engine.Built, error) {
			return &engine.Built{
				Service: apitest.New(),
				Start:   func(context.Context) error { return boom },
				Close:   func() error { closed = true; return nil },
			}, nil
		},
	})
	if !errors.Is(err, boom) || !closed || engine.IsRunning(dir) {
		t.Fatalf("start failure: err=%v closed=%v", err, closed)
	}
	if _, err := os.Stat(engine.DiscoveryPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("engine.json left behind after a failed start")
	}
	for _, o := range []engine.Options{{}, {DataDir: dir}, {DataDir: dir, Mode: "weird", Build: func(context.Context, engine.Env) (*engine.Built, error) { return nil, nil }}} {
		if err := engine.Run(context.Background(), o); err == nil {
			t.Fatalf("Run accepted invalid options %+v", o)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
