package client

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"watchink/internal/app"
	"watchink/internal/transport/api/apitest"
	"watchink/internal/transport/discovery"
	"watchink/internal/transport/engine"
)

// farm replaces the Spawner with one that runs engines in this process, the
// way a detached `watchink engine run` would start them.
type farm struct {
	t       *testing.T
	buildID string // build of the engines this farm starts

	mu       sync.Mutex
	spawns   int
	built    []*apitest.Fake // engines that won the lock
	lost     int             // engines that exited with ErrAlreadyRunning
	lastArgs []string
	lastExe  string
	lastEnv  int
	cancels  []context.CancelFunc
	wg       sync.WaitGroup
	prepare  func(f *apitest.Fake)
	delay    time.Duration
}

func newFarm(t *testing.T, clientBuild, engineBuild string) *farm {
	t.Helper()
	f := &farm{t: t, buildID: engineBuild}
	// Cleanups run last-in-first-out and the removal of the test's temp
	// directories is registered by the first t.TempDir call. Making that call
	// here, before the engines' cleanup below, means the engines stop first:
	// Windows cannot delete a data directory whose engine.log is still open.
	t.TempDir()
	oldSpawner, oldBuild := Spawner, buildID
	Spawner = f.spawn
	buildID = func() string { return clientBuild }
	t.Cleanup(func() {
		f.stopAll()
		Spawner, buildID = oldSpawner, oldBuild
	})
	return f
}

func (f *farm) spawn(exe string, args []string, env []string) error {
	f.mu.Lock()
	f.spawns++
	f.lastExe, f.lastArgs, f.lastEnv = exe, args, len(env)
	f.mu.Unlock()
	if len(args) != 4 || args[0] != "engine" || args[1] != "run" || args[2] != "--data-dir" {
		f.t.Errorf("spawn args = %v", args)
		return errors.New("bad args")
	}
	f.run(args[3], f.buildID, nil)
	return nil
}

// run starts one engine process stand-in and returns immediately.
func (f *farm) run(dataDir, build string, ready chan<- discovery.Info) {
	ctx, cancel := context.WithCancel(context.Background())
	f.mu.Lock()
	f.cancels = append(f.cancels, cancel)
	delay := f.delay
	f.mu.Unlock()
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		err := engine.Run(ctx, engine.Options{
			DataDir: dataDir, Port: -1, Version: "t", BuildID: build, NoSignals: true,
			OnReady: func(d discovery.Info) {
				if ready != nil {
					ready <- d
				}
			},
			Build: func(context.Context, engine.Env) (*engine.Built, error) {
				time.Sleep(delay) // migrations, recovery …
				fake := apitest.New()
				f.mu.Lock()
				if f.prepare != nil {
					f.prepare(fake)
				}
				f.built = append(f.built, fake)
				f.mu.Unlock()
				return &engine.Built{Service: fake, IdleExit: -1}, nil
			},
		})
		switch {
		case errors.Is(err, discovery.ErrAlreadyRunning):
			f.mu.Lock()
			f.lost++
			f.mu.Unlock()
		case err != nil:
			f.t.Errorf("engine.Run: %v", err)
		}
	}()
}

// startExisting runs an engine of the given build and waits until it serves.
func (f *farm) startExisting(dataDir, build string) (discovery.Info, *apitest.Fake) {
	f.t.Helper()
	ready := make(chan discovery.Info, 1)
	f.run(dataDir, build, ready)
	select {
	case d := <-ready:
		f.mu.Lock()
		defer f.mu.Unlock()
		return d, f.built[len(f.built)-1]
	case <-time.After(10 * time.Second):
		f.t.Fatal("existing engine not ready")
	}
	return discovery.Info{}, nil
}

func (f *farm) stopAll() {
	f.mu.Lock()
	cancels := f.cancels
	f.cancels = nil
	f.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	f.wg.Wait()
}

func (f *farm) counts() (spawns, built, lost int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns, len(f.built), f.lost
}

func TestConnectColdStartSpawnsEngine(t *testing.T) {
	f := newFarm(t, "b1", "b1")
	dir := t.TempDir()
	remote, err := Connect(ctxT(t), app.ConnectOptions{DataDir: dir, Requester: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if spawns, built, _ := f.counts(); spawns != 1 || built != 1 {
		t.Fatalf("spawns=%d built=%d", spawns, built)
	}
	exe, _ := os.Executable()
	if f.lastExe != exe || f.lastArgs[3] != dir || f.lastEnv != len(os.Environ()) {
		t.Fatalf("spawned %q %v with %d env vars", f.lastExe, f.lastArgs, f.lastEnv)
	}
	d, err := discovery.Read(dir)
	if err != nil || remote.BaseURL() != d.BaseURL {
		t.Fatalf("BaseURL %q vs discovery %+v (%v)", remote.BaseURL(), d, err)
	}
	info, err := remote.EngineInfo(ctxT(t))
	if err != nil || info.BuildID != "b1" || info.PID != os.Getpid() || info.Mode != discovery.ModeBackground {
		t.Fatalf("EngineInfo = %+v %v", info, err)
	}
	// The requester is stamped on imports.
	if _, err := remote.Import(ctxT(t), app.ImportRequest{Source: app.ImportSource{Type: app.SourceFile, Path: "/a.mp4"}}); err != nil {
		t.Fatal(err)
	}
	call, _ := f.built[0].LastCall("Import")
	if got := call.Args[0].(app.ImportRequest).RequestedBy; got != "mcp" {
		t.Fatalf("requested_by = %q", got)
	}

	// A second Connect reuses the running engine: no spawn.
	again, err := Connect(ctxT(t), app.ConnectOptions{DataDir: dir, NoSpawn: true})
	if err != nil || again.BaseURL() != remote.BaseURL() {
		t.Fatalf("reuse: %v", err)
	}
	again.Close()
	if spawns, built, _ := f.counts(); spawns != 1 || built != 1 {
		t.Fatalf("after reuse: spawns=%d built=%d", spawns, built)
	}

	// Shutdown through the Remote stops it; the lock frees.
	if err := remote.Shutdown(ctxT(t), false); err != nil {
		t.Fatal(err)
	}
	if err := waitStopped(ctxT(t), dir); err != nil {
		t.Fatal(err)
	}
	_, err = Connect(ctxT(t), app.ConnectOptions{DataDir: dir, NoSpawn: true})
	wantCode(t, err, app.CodeEngineUnavailable)
}

// TestConcurrentColdStartsConvergeOnOneEngine: three clients cold
// start at once and exactly one engine results.
func TestConcurrentColdStartsConvergeOnOneEngine(t *testing.T) {
	for round := 0; round < 5; round++ {
		f := newFarm(t, "b1", "b1")
		f.delay = 50 * time.Millisecond // widen the race window
		dir := t.TempDir()

		const clients = 3
		var wg sync.WaitGroup
		urls := make([]string, clients)
		errs := make([]error, clients)
		startGate := make(chan struct{})
		for i := 0; i < clients; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-startGate
				remote, err := Connect(ctxT(t), app.ConnectOptions{DataDir: dir})
				if err != nil {
					errs[i] = err
					return
				}
				defer remote.Close()
				info, err := remote.EngineInfo(ctxT(t))
				if err != nil {
					errs[i] = err
					return
				}
				urls[i] = info.BaseURL
			}()
		}
		close(startGate)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d client %d: %v", round, i, err)
			}
		}
		if urls[0] == "" || urls[0] != urls[1] || urls[1] != urls[2] {
			t.Fatalf("round %d: clients ended on different engines: %v", round, urls)
		}
		spawns, built, lost := f.counts()
		if built != 1 {
			t.Fatalf("round %d: %d engines came up, want exactly 1 (spawns=%d)", round, built, spawns)
		}
		if spawns < 1 || spawns > clients {
			t.Fatalf("round %d: spawns=%d", round, spawns)
		}
		// Every other spawned process exits on the lock while the winner
		// is still running (a loser retries the lock for ~100 ms first).
		deadline := time.Now().Add(5 * time.Second)
		for lost != spawns-1 {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: spawns=%d lost=%d", round, spawns, lost)
			}
			time.Sleep(10 * time.Millisecond)
			_, _, lost = f.counts()
		}
		if _, built, _ = f.counts(); built != 1 || !discovery.IsRunning(dir) {
			t.Fatalf("round %d: built=%d after the losers exited", round, built)
		}
		f.stopAll()
	}
}

func TestConnectIgnoresStaleDiscovery(t *testing.T) {
	f := newFarm(t, "b1", "b1")
	// (a) points at a closed port.
	closed, _ := net.Listen("tcp4", "127.0.0.1:0")
	addr := closed.Addr().String()
	closed.Close()
	// (b) points at a live engine of another data dir (token mismatch → 401).
	otherDir := t.TempDir()
	other, _ := f.startExisting(otherDir, "b1")

	for name, baseURL := range map[string]string{"closed port": "http://" + addr, "foreign engine": other.BaseURL} {
		dir := t.TempDir()
		if err := os.MkdirAll(discovery.RunDir(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		stale := `{"pid":999999,"base_url":"` + baseURL + `","token":"stale-token","version":"0","build_id":"b1","api":1,"mode":"background","started_at":"2020-01-01T00:00:00Z"}`
		if err := os.WriteFile(discovery.Path(dir), []byte(stale), 0o600); err != nil {
			t.Fatal(err)
		}
		// Without spawning, the stale file must not be mistaken for an engine.
		_, err := Connect(ctxT(t), app.ConnectOptions{DataDir: dir, NoSpawn: true})
		wantCode(t, err, app.CodeEngineUnavailable)

		before, _, _ := f.counts()
		remote, err := Connect(ctxT(t), app.ConnectOptions{DataDir: dir})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if remote.BaseURL() == baseURL {
			t.Fatalf("%s: connected to the stale address", name)
		}
		info, err := remote.EngineInfo(ctxT(t))
		if err != nil || info.BaseURL != remote.BaseURL() {
			t.Fatalf("%s: %+v %v", name, info, err)
		}
		remote.Close()
		if after, _, _ := f.counts(); after != before+1 {
			t.Fatalf("%s: spawns %d → %d", name, before, after)
		}
	}
}

func TestConnectNoSpawnAndMissingSpawner(t *testing.T) {
	f := newFarm(t, "b1", "b1")
	dir := t.TempDir()
	_, err := Connect(ctxT(t), app.ConnectOptions{DataDir: dir, NoSpawn: true})
	e := wantCode(t, err, app.CodeEngineUnavailable)
	if !strings.Contains(e.Message, "--no-spawn") {
		t.Fatalf("message = %q", e.Message)
	}
	if spawns, _, _ := f.counts(); spawns != 0 {
		t.Fatal("NoSpawn spawned an engine")
	}

	Spawner = nil
	_, err = Connect(ctxT(t), app.ConnectOptions{DataDir: dir})
	if e := wantCode(t, err, app.CodeEngineUnavailable); e.Hint == "" {
		t.Fatalf("no hint: %+v", e)
	}

	Spawner = func(string, []string, []string) error { return errors.New("exec format error") }
	_, err = Connect(ctxT(t), app.ConnectOptions{DataDir: dir})
	if e := wantCode(t, err, app.CodeEngineUnavailable); !strings.Contains(e.Message, "exec format error") {
		t.Fatalf("message = %q", e.Message)
	}
}

func TestConnectTimesOutWhenEngineNeverComesUp(t *testing.T) {
	newFarm(t, "b1", "b1")
	var polls atomic.Int32
	Spawner = func(string, []string, []string) error { polls.Add(1); return nil }
	dir := t.TempDir()
	began := time.Now()
	_, err := Connect(ctxT(t), app.ConnectOptions{DataDir: dir, Timeout: 400 * time.Millisecond})
	e := wantCode(t, err, app.CodeEngineUnavailable)
	if d := time.Since(began); d < 350*time.Millisecond || d > 3*time.Second {
		t.Fatalf("returned after %v", d)
	}
	if polls.Load() != 1 {
		t.Fatalf("spawned %d times, want once", polls.Load())
	}
	if !strings.Contains(e.Hint, "engine.log") {
		t.Fatalf("hint = %q", e.Hint)
	}

	// The caller's cancellation is not turned into an engine error.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	_, err = Connect(ctx, app.ConnectOptions{DataDir: dir})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Connect: %v", err)
	}
}

func TestBuildMismatchIdleEngineIsReplaced(t *testing.T) {
	f := newFarm(t, "new-build", "new-build")
	dir := t.TempDir()
	oldDisc, oldFake := f.startExisting(dir, "old-build")

	remote, err := Connect(ctxT(t), app.ConnectOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	info, err := remote.EngineInfo(ctxT(t))
	if err != nil || info.BuildID != "new-build" {
		t.Fatalf("connected engine: %+v %v", info, err)
	}
	d, _ := discovery.Read(dir)
	if d.Token == oldDisc.Token || d.BuildID != "new-build" {
		t.Fatalf("discovery still describes the old engine: %+v", d)
	}
	// The old engine was asked to stop gracefully, and stopped.
	if stopped, kinds := oldFake.Stopped(); !stopped || len(kinds) != 1 || kinds[0] {
		t.Fatalf("old engine Stop calls: %v %v", stopped, kinds)
	}
	if spawns, built, _ := f.counts(); spawns != 1 || built != 2 {
		t.Fatalf("spawns=%d built=%d", spawns, built)
	}
}

func TestBuildMismatchBusyEngineIsKept(t *testing.T) {
	f := newFarm(t, "new-build", "new-build")
	f.prepare = func(fake *apitest.Fake) { fake.AddJob(app.Job{Status: app.JobRunning}) }
	dir := t.TempDir()
	oldDisc, oldFake := f.startExisting(dir, "old-build")

	for _, noSpawn := range []bool{false, true} {
		_, err := Connect(ctxT(t), app.ConnectOptions{DataDir: dir, NoSpawn: noSpawn})
		e := wantCode(t, err, app.CodeEngineVersionMismatch)
		if !strings.Contains(e.Hint, "watchink engine stop --force") || !strings.Contains(e.Hint, "等待任务完成") {
			t.Fatalf("hint = %q", e.Hint)
		}
		if e.Details["engine_build_id"] != "old-build" || e.Details["client_build_id"] != "new-build" || e.Details["active_jobs"] != 1 {
			t.Fatalf("details = %v", e.Details)
		}
	}
	// The busy engine is untouched.
	if stopped, _ := oldFake.Stopped(); stopped {
		t.Fatal("busy engine was stopped")
	}
	if spawns, built, _ := f.counts(); spawns != 0 || built != 1 {
		t.Fatalf("spawns=%d built=%d", spawns, built)
	}
	if d, err := discovery.Read(dir); err != nil || *d != oldDisc || !discovery.IsRunning(dir) {
		t.Fatalf("old engine gone: %+v %v", d, err)
	}

	// Once its job is done, the same Connect replaces it.
	f.prepare = nil
	oldFake.SetActive(0)
	remote, err := Connect(ctxT(t), app.ConnectOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if info, _ := remote.EngineInfo(ctxT(t)); info == nil || info.BuildID != "new-build" {
		t.Fatalf("after the job finished: %+v", info)
	}
}

func TestBuildMismatchIdleButNoSpawnAndPersistentMismatch(t *testing.T) {
	// The spawned replacement is itself of another build (two installed
	// versions): Connect must not loop, it reports the mismatch.
	f := newFarm(t, "client-build", "third-build")
	dir := t.TempDir()
	_, oldFake := f.startExisting(dir, "old-build")

	_, err := Connect(ctxT(t), app.ConnectOptions{DataDir: dir, NoSpawn: true})
	wantCode(t, err, app.CodeEngineVersionMismatch)
	if stopped, _ := oldFake.Stopped(); stopped {
		t.Fatal("NoSpawn must not stop an engine it cannot replace")
	}

	_, err = Connect(ctxT(t), app.ConnectOptions{DataDir: dir})
	e := wantCode(t, err, app.CodeEngineVersionMismatch)
	if e.Details["engine_build_id"] != "third-build" {
		t.Fatalf("details = %v", e.Details)
	}
	if spawns, _, _ := f.counts(); spawns != 1 {
		t.Fatalf("spawns = %d, want exactly one replacement attempt", spawns)
	}
}
