package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"watchink/internal/app"
)

// The Windows manifest platform differs from the others in ways that do not
// depend on the OS running the test: executables carry ".exe" (declared by
// the manifest for downloads, derived for embedded tools), and one upstream
// archive holds two tools (ffmpeg and ffprobe), which must be downloaded
// once.
func TestWindowsPlatform(t *testing.T) {
	const plat = "windows-amd64"
	fx := newFixture(t)
	ff := zipOf(t, map[string]string{"ff-9/bin/one.exe": "one-v1", "ff-9/bin/two.exe": "two-v1", "ff-9/LICENSE.txt": "x"})
	url := fx.srv.put("/ff.zip", ff)
	shared := func(name string) Tool {
		return Tool{Version: "9.0", Platforms: map[string]Platform{plat: {
			URL: url, SHA256: sum(ff), Size: int64(len(ff)), Archive: ArchiveZip,
			Files: map[string]string{"ff-9/bin/" + name + ".exe": name + ".exe"}, Bin: name + ".exe",
		}}}
	}
	fx.mf.Tools = map[string]Tool{
		"one": shared("one"),
		"two": shared("two"),
		"emb": {Version: "v9", Embedded: true, SmokeArgs: []string{"--help"}},
	}
	emb := []byte("embedded-binary")
	m := fx.manager(func(o *Options) {
		o.Platform = plat
		o.Embedded = fstest.MapFS{
			plat + "/emb.exe":        {Data: emb},
			plat + "/emb.exe.sha256": {Data: []byte(sum(emb) + "  emb.exe\n")},
		}
	})
	if err := m.Ensure(context.Background(), []string{"one", "two", "emb"}, nil); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"one": "one-v1", "two": "two-v1", "emb": string(emb)} {
		p, err := m.Path(name)
		if err != nil {
			t.Fatalf("Path(%s): %v", name, err)
		}
		if wantPath := filepath.Join(fx.dir, "tools", name, m.Version(name), name+".exe"); p != wantPath {
			t.Errorf("Path(%s) = %s, want %s", name, p, wantPath)
		}
		if data, _ := os.ReadFile(p); string(data) != want {
			t.Errorf("%s content = %q", name, data)
		}
		// yt-dlp is pointed at BinDir and looks for ffmpeg.exe / ffprobe.exe.
		if data, _ := os.ReadFile(filepath.Join(m.BinDir(), name+".exe")); string(data) != want {
			t.Errorf("BinDir entry for %s is wrong", name)
		}
	}
	if n := fx.srv.hitCount("/ff.zip"); n != 1 {
		t.Errorf("shared archive downloaded %d times, want 1", n)
	}
	if left := stagingLeftovers(t, fx.dir); len(left) != 0 {
		t.Errorf("staging not empty: %v", left)
	}

	// A binary embedded without the ".exe" is not picked up: such a build
	// reports TOOL_MISSING with the build hint instead of running anything.
	calls := fx.exec.count()
	bad := fx.manager(func(o *Options) {
		o.DataDir = t.TempDir()
		o.Platform = plat
		o.Embedded = fstest.MapFS{plat + "/emb": {Data: emb}, plat + "/emb.sha256": {Data: []byte(sum(emb))}}
	})
	wantCode(t, bad.Ensure(context.Background(), []string{"emb"}, nil), app.CodeToolMissing)
	if fx.exec.count() != calls {
		t.Error("a binary was executed although the embedded tool is missing")
	}
	if exeName(plat, "x") != "x.exe" || exeName("darwin-arm64", "x") != "x" {
		t.Error("exeName is wrong")
	}
}
