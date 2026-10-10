package tools

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"watchink/internal/app"
)

const testPlatform = "test-os"

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// fakeExec stands in for a process runner: a "program" works unless its
// content contains BROKEN. It records every path it was asked to run.
type fakeExec struct {
	mu    sync.Mutex
	calls []string
	args  [][]string
}

func (f *fakeExec) run(ctx context.Context, path string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, path)
	f.args = append(f.args, args)
	f.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Windows has no executable bit; there the file name decides.
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("not executable")
	}
	if bytes.Contains(data, []byte("BROKEN")) {
		return []byte("segfault"), errors.New("exit status 139")
	}
	return []byte("version 1"), nil
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// origin is a test download server that counts requests per path.
type origin struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
	rng   map[string][]string
}

func newOrigin(t *testing.T) *origin {
	o := &origin{files: map[string][]byte{}, hits: map[string]int{}, rng: map[string][]string{}}
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.hits[r.URL.Path]++
		o.rng[r.URL.Path] = append(o.rng[r.URL.Path], r.Header.Get("Range"))
		data, ok := o.files[r.URL.Path]
		o.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(o.Close)
	return o
}

func (o *origin) put(path string, data []byte) string {
	o.mu.Lock()
	o.files[path] = data
	o.mu.Unlock()
	return o.URL + path
}

func (o *origin) hitCount(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hits[path]
}

type fixture struct {
	t    *testing.T
	dir  string
	srv  *origin
	exec *fakeExec
	mf   *Manifest
}

// newFixture serves a tiny manifest: "alpha" (zip), "beta" (raw file),
// "gamma" (tar.gz, lazy), "emb" (embedded) and the model "m1".
func newFixture(t *testing.T) *fixture {
	fx := &fixture{t: t, dir: t.TempDir(), srv: newOrigin(t), exec: &fakeExec{}}
	alpha := zipOf(t, map[string]string{"alpha": "alpha-v1", "README": "x"})
	beta := []byte("beta-v1")
	gamma := tarGzOf(t, map[string]string{"pkg/bin/gamma": "gamma-v1"})
	model := bytes.Repeat([]byte("model-data-"), 4000)
	fx.mf = &Manifest{
		ManifestVersion: 1,
		Tools: map[string]Tool{
			"alpha": {Version: "1.0", License: "MIT", SmokeArgs: []string{"-version"}, Platforms: map[string]Platform{testPlatform: {
				URL: fx.srv.put("/alpha.zip", alpha), SHA256: sum(alpha), Size: int64(len(alpha)), Archive: ArchiveZip, Files: map[string]string{"alpha": "alpha"}}}},
			"beta": {Version: "1.0", Platforms: map[string]Platform{testPlatform: {
				URL: fx.srv.put("/dl/1.0/beta_bin", beta), SHA256: sum(beta), Size: int64(len(beta)), Archive: ArchiveNone, Files: map[string]string{"beta_bin": "beta"}}},
				Update: &UpdateSource{LatestURL: fx.srv.URL + "/latest", DownloadURL: fx.srv.URL + "/dl/{version}/{asset}", Checksums: "SUMS"}},
			"gamma": {Version: "2.0", Lazy: true, Platforms: map[string]Platform{testPlatform: {
				URL: fx.srv.put("/gamma.tgz", gamma), SHA256: sum(gamma), Archive: ArchiveTarGz, Files: map[string]string{"pkg/bin/gamma": "gamma"}}}},
			"emb": {Version: "v9", Embedded: true, SmokeArgs: []string{"--help"}},
		},
		Models: map[string]Model{
			"m1": {URL: fx.srv.put("/models/m1.bin", model), SHA256: sum(model), Size: int64(len(model)), File: "m1.bin"},
		},
	}
	return fx
}

func (fx *fixture) manager(mut ...func(*Options)) *Manager {
	fx.t.Helper()
	emb := []byte("embedded-binary")
	opts := Options{
		DataDir: fx.dir, Exec: fx.exec.run, Manifest: fx.mf, Platform: testPlatform,
		Embedded: fstest.MapFS{
			testPlatform + "/emb":        {Data: emb, Mode: 0o644},
			testPlatform + "/emb.sha256": {Data: []byte(sum(emb) + "\n")},
		},
	}
	for _, f := range mut {
		f(&opts)
	}
	m, err := New(opts)
	if err != nil {
		fx.t.Fatal(err)
	}
	m.retryDelay = time.Millisecond
	return m
}

func wantCode(t *testing.T, err error, code app.Code) {
	t.Helper()
	if !app.IsCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func readInstalled(t *testing.T, dir string) *installedFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "tools", "installed.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := newInstalledFile()
	if err := json.Unmarshal(data, f); err != nil {
		t.Fatalf("installed.json is not valid JSON: %v\n%s", err, data)
	}
	return f
}

func stagingLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(filepath.Join(dir, "tools", ".staging"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestEnsureHappyPath(t *testing.T) {
	fx := newFixture(t)
	changes := atomic.Int32{}
	m := fx.manager(func(o *Options) { o.OnChange = func() { changes.Add(1) } })

	st := m.Status("m1", false)
	if st.Ready || len(st.Items) != 5 {
		t.Fatalf("fresh status: ready=%v items=%d", st.Ready, len(st.Items))
	}
	if _, err := m.Path("alpha"); !app.IsCode(err, app.CodeToolMissing) {
		t.Fatalf("Path before install: %v", err)
	}

	var mu sync.Mutex
	seen := map[string][2]int64{}
	names := []string{"alpha", "beta", "gamma", "emb", "m1"}
	err := m.Ensure(context.Background(), names, func(name string, done, total int64) {
		mu.Lock()
		seen[name] = [2]int64{done, total}
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"alpha", "m1"} {
		if p := seen[n]; p[0] == 0 || p[0] != p[1] {
			t.Errorf("%s: final progress %v", n, p)
		}
	}
	for name, want := range map[string]string{"alpha": "alpha-v1", "beta": "beta-v1", "gamma": "gamma-v1", "emb": "embedded-binary"} {
		p, err := m.Path(name)
		if err != nil {
			t.Fatalf("Path(%s): %v", name, err)
		}
		wantPath := filepath.Join(fx.dir, "tools", name, m.Version(name), name)
		if p != wantPath {
			t.Errorf("Path(%s) = %s, want %s", name, p, wantPath)
		}
		data, _ := os.ReadFile(p)
		if string(data) != want {
			t.Errorf("%s content = %q", name, data)
		}
		if info, _ := os.Stat(p); runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
			t.Errorf("%s mode = %v", name, info.Mode())
		}
		if data, _ := os.ReadFile(filepath.Join(m.BinDir(), name)); string(data) != want {
			t.Errorf("BinDir link for %s is wrong", name)
		}
	}
	mp, msum, err := m.ModelPath("m1")
	if err != nil || mp != filepath.Join(fx.dir, "models", "m1", "m1.bin") || msum != fx.mf.Models["m1"].SHA256 {
		t.Fatalf("ModelPath = %q %q %v", mp, msum, err)
	}
	// Smoke tests ran from staging with the manifest's arguments.
	if fx.exec.count() != 4 {
		t.Fatalf("exec calls = %d, want 4", fx.exec.count())
	}
	for i, c := range fx.exec.calls {
		if !strings.Contains(c, ".staging") {
			t.Errorf("smoke test ran outside staging: %s", c)
		}
		want := "--version"
		switch filepath.Base(c) {
		case "alpha":
			want = "-version"
		case "emb":
			want = "--help"
		}
		if len(fx.exec.args[i]) != 1 || fx.exec.args[i][0] != want {
			t.Errorf("%s smoke args = %v", c, fx.exec.args[i])
		}
	}
	st = m.Status("m1", false)
	// (Ready stays false here: the fixture manifest has no ffmpeg etc.)
	if st.MissingDownloadBytes != 0 {
		t.Fatalf("status after install: %+v", st)
	}
	for _, it := range st.Items {
		if it.Status != app.ToolReady || it.Path == "" || it.SizeBytes == 0 {
			t.Errorf("item %+v not ready", it)
		}
	}
	if left := stagingLeftovers(t, fx.dir); len(left) != 0 {
		t.Errorf("staging not clean: %v", left)
	}
	if changes.Load() == 0 {
		t.Error("OnChange was never called")
	}

	// A second Ensure, and a new Manager on the same directory, do nothing.
	before := fx.srv.hitCount("/alpha.zip")
	if err := fx.manager().Ensure(context.Background(), names, nil); err != nil {
		t.Fatal(err)
	}
	if fx.srv.hitCount("/alpha.zip") != before || fx.exec.count() != 4 {
		t.Error("ready items were installed again")
	}
}

func TestChecksumMismatchNeverInstallsOrExecutes(t *testing.T) {
	fx := newFixture(t)
	fx.srv.put("/alpha.zip", zipOf(t, map[string]string{"alpha": "evil"}))
	fx.srv.put("/dl/1.0/beta_bin", []byte("evil!!!")) // same size as the original
	fx.srv.put("/models/m1.bin", []byte("short"))
	m := fx.manager()
	for _, name := range []string{"alpha", "beta", "m1"} {
		err := m.Ensure(context.Background(), []string{name}, nil)
		wantCode(t, err, app.CodeChecksumMismatch)
	}
	if fx.exec.count() != 0 {
		t.Fatalf("unverified file was executed: %v", fx.exec.calls)
	}
	for _, p := range []string{"tools/alpha", "tools/beta", "models/m1/m1.bin", "tools/installed.json"} {
		if _, err := os.Stat(filepath.Join(fx.dir, p)); err == nil {
			t.Errorf("%s exists after a checksum mismatch", p)
		}
	}
	if left := stagingLeftovers(t, fx.dir); len(left) != 0 {
		t.Errorf("bad downloads kept: %v", left)
	}
	st := m.Status("m1", false)
	for _, it := range st.Items {
		if it.Name == "alpha" && (it.Status != app.ToolFailed || it.Error == "") {
			t.Errorf("alpha status = %+v", it)
		}
	}
}

func TestMirrorRewrite(t *testing.T) {
	fx := newFixture(t)
	p := fx.mf.Tools["alpha"].Platforms[testPlatform]
	p.URL = "https://upstream.invalid/alpha.zip?download=true"
	fx.mf.Tools["alpha"].Platforms[testPlatform] = p
	m := fx.manager()
	m.attempts = 1

	// Without the mirror the host does not resolve.
	wantCode(t, m.Ensure(context.Background(), []string{"alpha"}, nil), app.CodeNetworkError)

	m.SetNetwork(map[string]string{"Upstream.invalid": fx.srv.URL + "/"}, "")
	if got, want := m.mirrorURL(p.URL), fx.srv.URL+"/alpha.zip?download=true"; got != want {
		t.Fatalf("mirrorURL = %s, want %s", got, want)
	}
	if err := m.Ensure(context.Background(), []string{"alpha"}, nil); err != nil {
		t.Fatal(err)
	}

	// A mirror serving different bytes is rejected by the same sha256 check.
	mp := fx.mf.Models["m1"]
	mp.URL = "https://models.invalid/models/m1.bin"
	fx.mf.Models["m1"] = mp
	fx.srv.put("/models/m1.bin", []byte("tampered"))
	m.SetNetwork(map[string]string{"models.invalid": fx.srv.URL}, "")
	wantCode(t, m.Ensure(context.Background(), []string{"m1"}, nil), app.CodeChecksumMismatch)
}

func TestProxySetting(t *testing.T) {
	fx := newFixture(t)
	body := []byte("beta-v1")
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "upstream.invalid" { // a proxy receives the absolute URL
			http.Error(w, "not a proxy request", http.StatusBadRequest)
			return
		}
		proxied.Add(1)
		w.Write(body)
	}))
	defer proxy.Close()
	p := fx.mf.Tools["beta"].Platforms[testPlatform]
	p.URL = "http://upstream.invalid/beta_bin"
	fx.mf.Tools["beta"].Platforms[testPlatform] = p

	m := fx.manager()
	m.attempts = 1
	m.SetNetwork(nil, proxy.URL) // changed at runtime
	if err := m.Ensure(context.Background(), []string{"beta"}, nil); err != nil {
		t.Fatal(err)
	}
	if proxied.Load() != 1 {
		t.Fatalf("proxy saw %d requests", proxied.Load())
	}
}

func TestFailedSmokeTestKeepsOldVersion(t *testing.T) {
	fx := newFixture(t)
	m := fx.manager()
	if err := m.Ensure(context.Background(), []string{"alpha"}, nil); err != nil {
		t.Fatal(err)
	}
	oldPath, _ := m.Path("alpha")

	// Same version, replaced by a broken build via Import: the installed
	// copy must survive.
	broken := zipOf(t, map[string]string{"alpha": "BROKEN"})
	p := fx.mf.Tools["alpha"].Platforms[testPlatform]
	p.SHA256 = sum(broken)
	fx.mf.Tools["alpha"].Platforms[testPlatform] = p
	file := filepath.Join(t.TempDir(), "alpha.zip")
	os.WriteFile(file, broken, 0o644)
	err := m.Import(context.Background(), "alpha", file)
	wantCode(t, err, app.CodeToolFailed)
	if out, _ := app.AsError(err).Details["output"].(string); out != "segfault" {
		t.Errorf("smoke output not reported: %v", app.AsError(err).Details)
	}
	if data, _ := os.ReadFile(oldPath); string(data) != "alpha-v1" {
		t.Fatalf("installed version was damaged: %q", data)
	}
	if got, err := m.Path("alpha"); err != nil || got != oldPath {
		t.Fatalf("Path after failed import = %q, %v", got, err)
	}

	// A new manifest version that fails its smoke test: 1.0 stays active.
	fx.srv.put("/alpha2.zip", broken)
	fx.mf.Tools["alpha"] = Tool{Version: "2.0", Platforms: map[string]Platform{testPlatform: {
		URL: fx.srv.URL + "/alpha2.zip", SHA256: sum(broken), Archive: ArchiveZip, Files: map[string]string{"alpha": "alpha"}}}}
	m2 := fx.manager()
	wantCode(t, m2.Ensure(context.Background(), []string{"alpha"}, nil), app.CodeToolFailed)
	inst := readInstalled(t, fx.dir)
	if rec := inst.Tools["alpha"]; rec.Active != "1.0" || rec.Versions["2.0"] != nil {
		t.Fatalf("installed.json changed: %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(fx.dir, "tools", "alpha", "2.0")); err == nil {
		t.Error("broken version was moved into place")
	}
	if data, _ := os.ReadFile(oldPath); string(data) != "alpha-v1" {
		t.Fatal("old version removed")
	}
}

func TestConcurrentEnsureSameItem(t *testing.T) {
	fx := newFixture(t)
	m := fx.manager()
	var wg sync.WaitGroup
	errs := make([]error, 12)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = m.Ensure(context.Background(), []string{"alpha", "m1"}, func(string, int64, int64) {})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := fx.srv.hitCount("/alpha.zip"); n != 1 {
		t.Errorf("alpha downloaded %d times", n)
	}
	if n := fx.srv.hitCount("/models/m1.bin"); n != 1 {
		t.Errorf("model downloaded %d times", n)
	}
	if fx.exec.count() != 1 {
		t.Errorf("smoke test ran %d times", fx.exec.count())
	}
}

func TestWaiterTakesOverAfterCancel(t *testing.T) {
	fx := newFixture(t)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	data := fx.srv.files["/alpha.zip"]
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
			w.Write(data)
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	p := fx.mf.Tools["alpha"].Platforms[testPlatform]
	p.URL = slow.URL + "/alpha.zip"
	fx.mf.Tools["alpha"].Platforms[testPlatform] = p
	m := fx.manager()

	ctx1, cancel1 := context.WithCancel(context.Background())
	res1, res2 := make(chan error, 1), make(chan error, 1)
	go func() { res1 <- m.Ensure(ctx1, []string{"alpha"}, nil) }()
	<-started
	if st := m.Status("m1", false); st.Items[0].Status != app.ToolDownloading {
		t.Errorf("status during download = %s", st.Items[0].Status)
	}
	go func() { res2 <- m.Ensure(context.Background(), []string{"alpha"}, nil) }()
	time.Sleep(20 * time.Millisecond)
	cancel1()
	if err := <-res1; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Ensure returned %v", err)
	}
	<-started // the waiter started its own download
	close(release)
	if err := <-res2; err != nil {
		t.Fatalf("waiter: %v", err)
	}
	if _, err := m.Path("alpha"); err != nil {
		t.Fatal(err)
	}
}

func TestResumeAndRetry(t *testing.T) {
	fx := newFixture(t)
	m := fx.manager()
	md := fx.mf.Models["m1"]
	data := fx.srv.files["/models/m1.bin"]

	// A partial file from an earlier run is continued with a Range request.
	part := m.partPath("m1", md.SHA256)
	os.MkdirAll(filepath.Dir(part), 0o755)
	os.WriteFile(part, data[:10000], 0o644)
	if err := m.Ensure(context.Background(), []string{"m1"}, nil); err != nil {
		t.Fatal(err)
	}
	if r := fx.srv.rng["/models/m1.bin"]; len(r) != 1 || r[0] != "bytes=10000-" {
		t.Fatalf("Range headers = %v", r)
	}
	if p, _, _ := m.ModelPath("m1"); p == "" {
		t.Fatal("model not installed")
	} else if got, _ := os.ReadFile(p); !bytes.Equal(got, data) {
		t.Fatal("resumed file differs")
	}

	// A connection that dies halfway is retried from where it stopped.
	var calls atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.Write(data[:5000])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		if r.Header.Get("Range") == "" {
			t.Error("retry did not resume")
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	defer flaky.Close()
	fx2 := newFixture(t)
	fx2.mf.Models["m1"] = Model{URL: flaky.URL + "/m1.bin", SHA256: md.SHA256, Size: md.Size, File: "m1.bin"}
	if err := fx2.manager().Ensure(context.Background(), []string{"m1"}, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("requests = %d, want 2", calls.Load())
	}
}

func TestNetworkErrors(t *testing.T) {
	fx := newFixture(t)
	p := fx.mf.Tools["alpha"].Platforms[testPlatform]
	p.URL = fx.srv.URL + "/gone.zip"
	fx.mf.Tools["alpha"].Platforms[testPlatform] = p
	m := fx.manager()
	err := m.Ensure(context.Background(), []string{"alpha"}, nil)
	wantCode(t, err, app.CodeNetworkError)
	if n := fx.srv.hitCount("/gone.zip"); n != 1 {
		t.Errorf("a 404 was retried %d times", n)
	}
	wantCode(t, m.Ensure(context.Background(), []string{"nope"}, nil), app.CodeInvalidArgument)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Ensure(ctx, []string{"beta"}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: %v", err)
	}
}

func TestImport(t *testing.T) {
	fx := newFixture(t)
	m := fx.manager()
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ctx := context.Background()

	wantCode(t, m.Import(ctx, "alpha", write("wrong.zip", zipOf(t, map[string]string{"alpha": "other"}))), app.CodeChecksumMismatch)
	wantCode(t, m.Import(ctx, "m1", write("wrong.bin", []byte("x"))), app.CodeChecksumMismatch)
	if fx.exec.count() != 0 {
		t.Fatal("an unverified import was executed")
	}
	wantCode(t, m.Import(ctx, "emb", write("emb", []byte("x"))), app.CodeInvalidArgument)
	wantCode(t, m.Import(ctx, "alpha", filepath.Join(dir, "missing")), app.CodeInvalidArgument)
	wantCode(t, m.Import(ctx, "nope", write("n", nil)), app.CodeInvalidArgument)

	alphaFile := write("alpha.zip", fx.srv.files["/alpha.zip"])
	if err := m.Import(ctx, "alpha", alphaFile); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(ctx, "beta", write("beta", fx.srv.files["/dl/1.0/beta_bin"])); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(ctx, "m1", write("m1.bin", fx.srv.files["/models/m1.bin"])); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(alphaFile); err != nil {
		t.Error("Import consumed the user's file")
	}
	for _, n := range []string{"alpha", "beta"} {
		if _, err := m.Path(n); err != nil {
			t.Errorf("Path(%s): %v", n, err)
		}
	}
	if _, _, err := m.ModelPath("m1"); err != nil {
		t.Error(err)
	}
	if len(fx.srv.hits) != 0 {
		t.Errorf("Import used the network: %v", fx.srv.hits)
	}
	if src := readInstalled(t, fx.dir).Tools["alpha"].Versions["1.0"].Source; src != sourceImport {
		t.Errorf("source = %s", src)
	}
}

func TestInstalledJSONAtomicity(t *testing.T) {
	fx := newFixture(t)
	m := fx.manager()
	names := []string{"alpha", "beta", "gamma", "emb", "m1"}

	// Readers never see a partial file while writers install in parallel.
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(m.installedPath())
			if err != nil {
				continue
			}
			if !json.Valid(data) {
				t.Errorf("observed a partial installed.json: %q", data)
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Ensure(context.Background(), []string{n}, nil); err != nil {
				t.Errorf("%s: %v", n, err)
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-readerDone

	inst := readInstalled(t, fx.dir)
	if len(inst.Tools) != 4 || len(inst.Models) != 1 {
		t.Fatalf("lost updates: %d tools, %d models", len(inst.Tools), len(inst.Models))
	}
	entries, _ := os.ReadDir(filepath.Join(fx.dir, "tools"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	// A corrupt file means "nothing installed", not a crash; the next
	// Ensure repairs it.
	os.WriteFile(m.installedPath(), []byte(`{"tools": {"alpha"`), 0o644)
	if _, err := m.Path("alpha"); !app.IsCode(err, app.CodeToolMissing) {
		t.Fatalf("Path with corrupt installed.json: %v", err)
	}
	if err := m.Ensure(context.Background(), []string{"alpha"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Path("alpha"); err != nil {
		t.Fatal(err)
	}

	// A binary that changed size on disk is not reported as ready.
	p, _ := m.Path("alpha")
	os.WriteFile(p, []byte("truncated"), 0o755)
	if _, err := m.Path("alpha"); !app.IsCode(err, app.CodeToolMissing) {
		t.Fatalf("Path of a modified binary: %v", err)
	}
}

func TestEmbedded(t *testing.T) {
	fx := newFixture(t)
	bin := []byte("embedded-binary")

	t.Run("absent", func(t *testing.T) {
		m := fx.manager(func(o *Options) { o.Embedded = fstest.MapFS{testPlatform + "/.gitkeep": {}} })
		err := m.Ensure(context.Background(), []string{"emb"}, nil)
		wantCode(t, err, app.CodeToolMissing)
		if !strings.Contains(app.AsError(err).Hint, "build.sh") {
			t.Errorf("hint = %q", app.AsError(err).Hint)
		}
		_, err = m.Path("emb")
		wantCode(t, err, app.CodeToolMissing)
	})
	t.Run("sha mismatch", func(t *testing.T) {
		m := fx.manager(func(o *Options) {
			o.Embedded = fstest.MapFS{
				testPlatform + "/emb":        {Data: bin},
				testPlatform + "/emb.sha256": {Data: []byte(sum([]byte("something else")))},
			}
		})
		wantCode(t, m.Ensure(context.Background(), []string{"emb"}, nil), app.CodeChecksumMismatch)
		if fx.exec.count() != 0 {
			t.Fatal("unverified embedded binary was executed")
		}
	})
	t.Run("rebuilt binary is extracted again", func(t *testing.T) {
		m := fx.manager()
		if err := m.Ensure(context.Background(), []string{"emb"}, nil); err != nil {
			t.Fatal(err)
		}
		newBin := []byte("embedded-binary-rebuilt")
		m2 := fx.manager(func(o *Options) {
			o.Embedded = fstest.MapFS{
				testPlatform + "/emb":        {Data: newBin},
				testPlatform + "/emb.sha256": {Data: []byte(sum(newBin) + "  emb\n")},
			}
		})
		if _, err := m2.Path("emb"); !app.IsCode(err, app.CodeToolMissing) {
			t.Fatalf("stale extraction reported ready: %v", err)
		}
		if err := m2.Ensure(context.Background(), []string{"emb"}, nil); err != nil {
			t.Fatal(err)
		}
		p, _ := m2.Path("emb")
		if data, _ := os.ReadFile(p); !bytes.Equal(data, newBin) {
			t.Fatalf("content = %q", data)
		}
	})
}

func TestWhisperEnvOverride(t *testing.T) {
	fx := newFixture(t)
	fx.mf.Tools[ToolWhisperCLI] = Tool{Version: "v1", Embedded: true}
	m := fx.manager(func(o *Options) { o.Embedded = fstest.MapFS{} })
	wantCode(t, m.Ensure(context.Background(), []string{ToolWhisperCLI}, nil), app.CodeToolMissing)

	ext := filepath.Join(t.TempDir(), "my-whisper")
	os.WriteFile(ext, []byte("x"), 0o755)
	t.Setenv(EnvWhisperBin, ext)
	if p, err := m.Path(ToolWhisperCLI); err != nil || p != ext {
		t.Fatalf("Path = %q, %v", p, err)
	}
	if err := m.Ensure(context.Background(), []string{ToolWhisperCLI}, nil); err != nil {
		t.Fatal(err)
	}
	if fx.exec.count() != 0 {
		t.Error("override must not be extracted or smoke-tested")
	}
	t.Setenv(EnvWhisperBin, ext+".missing")
	_, err := m.Path(ToolWhisperCLI)
	wantCode(t, err, app.CodeToolMissing)
}

func TestUpdateAndRollback(t *testing.T) {
	fx := newFixture(t)
	m := fx.manager()
	ctx := context.Background()
	if err := m.Ensure(ctx, []string{"beta"}, nil); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	latest := "2.0"
	v2 := []byte("beta-v2")
	sums := "deadbeef  other\n" + sum(v2) + "  beta_bin\n"
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+latest, http.StatusFound)
	})
	mux.HandleFunc("/dl/2.0/SUMS", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sums)) })
	mux.HandleFunc("/dl/2.0/beta_bin", func(w http.ResponseWriter, r *http.Request) { w.Write(v2) })
	mux.HandleFunc("/dl/3.0/SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sum([]byte("expected")) + "  beta_bin\n"))
	})
	mux.HandleFunc("/dl/3.0/beta_bin", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("tampered")) })
	up := httptest.NewServer(mux)
	defer up.Close()
	fx.mf.Tools["beta"].Update.LatestURL = up.URL + "/latest"
	fx.mf.Tools["beta"].Update.DownloadURL = up.URL + "/dl/{version}/{asset}"

	v, err := m.UpdateTool(ctx, "beta")
	if err != nil || v != "2.0" {
		t.Fatalf("UpdateTool = %q, %v", v, err)
	}
	p, _ := m.Path("beta")
	if data, _ := os.ReadFile(p); string(data) != "beta-v2" || m.Version("beta") != "2.0" {
		t.Fatalf("active after update: %s %q", m.Version("beta"), data)
	}
	if data, _ := os.ReadFile(filepath.Join(m.BinDir(), "beta")); string(data) != "beta-v2" {
		t.Error("BinDir link not switched")
	}
	if _, err := os.Stat(filepath.Join(fx.dir, "tools", "beta", "1.0", "beta")); err != nil {
		t.Fatal("old version was not kept")
	}
	// The pinned choice survives a restart even though the manifest says 1.0.
	if got := fx.manager().Version("beta"); got != "2.0" {
		t.Errorf("version after restart = %s", got)
	}
	if vs, active := m.InstalledVersions("beta"); len(vs) != 2 || active != "2.0" {
		t.Errorf("InstalledVersions = %v, %s", vs, active)
	}

	// A release whose download does not match its checksum list is refused.
	latest = "3.0"
	execs := fx.exec.count()
	_, err = m.UpdateTool(ctx, "beta")
	wantCode(t, err, app.CodeChecksumMismatch)
	if fx.exec.count() != execs || m.Version("beta") != "2.0" {
		t.Fatal("failed update changed the active version or ran the file")
	}

	v, err = m.Rollback("beta")
	if err != nil || v != "1.0" || m.Version("beta") != "1.0" {
		t.Fatalf("Rollback = %q, %v", v, err)
	}
	p, _ = m.Path("beta")
	if data, _ := os.ReadFile(p); string(data) != "beta-v1" {
		t.Fatalf("content after rollback = %q", data)
	}
	if v, _ := m.Rollback("beta"); v != "2.0" {
		t.Errorf("second rollback = %q", v)
	}
	// Already installed: update only switches.
	latest = "2.0"
	m.Rollback("beta")
	if v, err := m.UpdateTool(ctx, "beta"); err != nil || v != "2.0" || m.Version("beta") != "2.0" {
		t.Errorf("re-update = %q, %v", v, err)
	}
	_, err = m.UpdateTool(ctx, "alpha")
	wantCode(t, err, app.CodeInvalidArgument)
	_, err = m.Rollback("alpha")
	wantCode(t, err, app.CodeNotFound)
}

func TestStatusAndNeeded(t *testing.T) {
	m, err := New(Options{DataDir: t.TempDir(), Exec: (&fakeExec{}).run, Platform: "darwin-arm64"})
	if err != nil {
		t.Fatal(err)
	}
	def := m.DefaultASRModel()
	tests := []struct {
		source, model string
		vad           bool
		want          string
	}{
		{app.SourceFile, "", false, "ffmpeg ffprobe whisper-cli " + def},
		{app.SourceUpload, ModelSmallQ5, true, "ffmpeg ffprobe whisper-cli silero-vad whisper-small-q5_1"},
		{app.SourceBilibili, ModelSmallQ5, false, "ffmpeg ffprobe yt-dlp whisper-cli whisper-small-q5_1"},
		{app.SourceYouTube, ModelSmallQ5, false, "ffmpeg ffprobe yt-dlp deno whisper-cli whisper-small-q5_1"},
	}
	for _, tc := range tests {
		if got := strings.Join(m.Needed(tc.source, tc.model, tc.vad), " "); got != tc.want {
			t.Errorf("Needed(%s,%s,%v) = %s, want %s", tc.source, tc.model, tc.vad, got, tc.want)
		}
	}

	st := m.Status("", true)
	if st.Ready || st.DefaultASRModel != def || len(st.Items) != 8 {
		t.Fatalf("status: %+v", st)
	}
	var missing int64
	for _, it := range st.Items {
		wantReq := map[string]bool{"ffmpeg": true, "ffprobe": true, "whisper-cli": true, "silero-vad": true, def: true}[it.Name]
		if it.Required != wantReq {
			t.Errorf("%s required = %v", it.Name, it.Required)
		}
		if it.Name == "deno" && !it.Lazy {
			t.Error("deno must be lazy")
		}
		if it.Name == "whisper-cli" && (!it.Embedded || it.DownloadBytes != 0) {
			t.Errorf("whisper-cli item: %+v", it)
		}
		if it.Required && it.Status != app.ToolReady {
			missing += it.DownloadBytes
		}
	}
	if st.MissingDownloadBytes != missing || missing == 0 {
		t.Errorf("MissingDownloadBytes = %d, want %d", st.MissingDownloadBytes, missing)
	}
	if m.Status("no-such-model", false).Ready {
		t.Error("unknown model reported ready")
	}

	// No download for this platform: a clear TOOL_MISSING, not a crash.
	other, _ := New(Options{DataDir: t.TempDir(), Exec: (&fakeExec{}).run, Platform: "plan9-mips"})
	wantCode(t, other.Ensure(context.Background(), []string{"ffmpeg"}, nil), app.CodeToolMissing)
	_, err = other.Path("ffmpeg")
	wantCode(t, err, app.CodeToolMissing)
}

func TestDefaultManifest(t *testing.T) {
	mf, err := DefaultManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ToolFFmpeg, ToolFFprobe, ToolYtDlp, ToolDeno} {
		p, ok := mf.Tools[name].Platforms["darwin-arm64"]
		if !ok || !strings.HasPrefix(p.URL, "https://") || p.Size == 0 || strings.Contains(p.URL, "latest") {
			t.Errorf("%s darwin-arm64 entry is not pinned: %+v", name, p)
		}
	}
	for _, name := range []string{ToolFFmpeg, ToolFFprobe, ToolYtDlp, ToolDeno} {
		// BtbN's "latest" release is rebuilt daily; only a dated autobuild
		// tag keeps the same bytes.
		p, ok := mf.Tools[name].Platforms["linux-amd64"]
		if !ok || !strings.HasPrefix(p.URL, "https://") || p.Size == 0 || strings.Contains(p.URL, "latest") {
			t.Errorf("%s linux-amd64 entry is not pinned: %+v", name, p)
		}
	}
	// Windows: pinned like the others, and every executable is a ".exe" that
	// the entry names as its bin.
	for _, name := range []string{ToolFFmpeg, ToolFFprobe, ToolYtDlp, ToolDeno} {
		p, ok := mf.Tools[name].Platforms["windows-amd64"]
		if !ok || !strings.HasPrefix(p.URL, "https://") || p.Size == 0 || strings.Contains(p.URL, "latest") {
			t.Errorf("%s windows-amd64 entry is not pinned: %+v", name, p)
		}
		if p.Bin != name+".exe" {
			t.Errorf("%s windows-amd64 bin = %q", name, p.Bin)
		}
		installed := false
		for _, dst := range p.Files {
			installed = installed || dst == p.Bin
		}
		if !installed {
			t.Errorf("%s windows-amd64 files do not install %s: %v", name, p.Bin, p.Files)
		}
	}
	for _, name := range []string{ModelLargeV3TurboQ5, ModelSmallQ5, ModelSileroVAD} {
		md := mf.Models[name]
		if md.Size == 0 || !strings.Contains(md.URL, "/resolve/") || strings.Contains(md.URL, "/resolve/main/") {
			t.Errorf("%s is not pinned to a revision: %s", name, md.URL)
		}
	}
	if !mf.Tools[ToolWhisperCLI].Embedded || !mf.Tools[ToolDeno].Lazy {
		t.Error("embedded / lazy flags are wrong")
	}

	// The manifest and the build script must agree on the whisper.cpp tag.
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "third_party", "whisper", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "WHISPER_TAG=" + mf.Tools[ToolWhisperCLI].Version + "\n"; !strings.Contains(string(data), want) {
		t.Errorf("third_party/whisper/VERSION does not contain %q", strings.TrimSpace(want))
	}
}

func TestParseManifestRejectsUnsafeNames(t *testing.T) {
	h := strings.Repeat("a", 64)
	bad := []string{
		`{"manifest_version":2}`,
		`{"manifest_version":1,"tools":{"../x":{"version":"1"}}}`,
		`{"manifest_version":1,"tools":{"x":{"version":"../1"}}}`,
		`{"manifest_version":1,"tools":{"x":{"version":"1","platforms":{"p":{"url":"u","sha256":"` + h + `","archive":"zip","files":{"a":"../a"}}}}}}`,
		`{"manifest_version":1,"tools":{"x":{"version":"1","platforms":{"p":{"url":"u","sha256":"zz","archive":"zip","files":{"a":"a"}}}}}}`,
		`{"manifest_version":1,"models":{"m":{"url":"https://h/a/..","sha256":"` + h + `"}}}`,
		`{"manifest_version":1,"tools":{"m":{"version":"1"}},"models":{"m":{"url":"https://h/f.bin","sha256":"` + h + `"}}}`,
	}
	for _, s := range bad {
		if _, err := ParseManifest([]byte(s)); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}
