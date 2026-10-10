//go:build desktop

package desktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"watchink/internal/app"
)

// fakeRemote implements the parts of app.Remote the shell uses; any other
// method panics through the nil embedded interface.
type fakeRemote struct {
	app.Remote
	base, token, dataDir string
	dead                 bool
	closed               bool
	exports              map[string][]byte
	corrupt              bool // serve fewer bytes than announced
	opens                int
}

func (f *fakeRemote) BaseURL() string { return f.base }
func (f *fakeRemote) Token() string   { return f.token }
func (f *fakeRemote) Close() error    { f.closed = true; return nil }

func (f *fakeRemote) EngineInfo(context.Context) (*app.EngineInfo, error) {
	if f.dead {
		return nil, app.E(app.CodeEngineUnavailable, "engine gone")
	}
	return &app.EngineInfo{Version: "9.9.9", DataDir: f.dataDir, BaseURL: f.base}, nil
}

func (f *fakeRemote) OpenExport(_ context.Context, id string) (io.ReadCloser, *app.Export, error) {
	b, ok := f.exports[id]
	if !ok {
		return nil, nil, app.E(app.CodeNotFound, "no export %s", id)
	}
	f.opens++
	sum := sha256.Sum256(b)
	meta := &app.Export{ExportID: id, Filename: "note.md", Bytes: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}
	if f.corrupt {
		b = b[:len(b)/2]
	}
	return io.NopCloser(bytes.NewReader(b)), meta, nil
}

// tokenless is an app.Remote that does not implement TokenProvider.
type tokenless struct{ app.Remote }

func (tokenless) EngineInfo(context.Context) (*app.EngineInfo, error) { return &app.EngineInfo{}, nil }
func (tokenless) BaseURL() string                                     { return "http://127.0.0.1:1" }
func (tokenless) Close() error                                        { return nil }

type harness struct {
	app      *App
	remotes  []*fakeRemote
	connects []app.ConnectOptions
	revealed []string
	opened   []string
	saveTo   string // what the save dialog returns
	saveName string // default name the dialog was given
}

func newHarness(t *testing.T, dataDir string) *harness {
	t.Helper()
	h := &harness{}
	h.app = newApp(Options{
		DataDir: "/flag/data",
		Version: "1.2.3",
		Connect: func(_ context.Context, o app.ConnectOptions) (app.Remote, error) {
			h.connects = append(h.connects, o)
			r := &fakeRemote{base: "http://127.0.0.1:47823/", token: "tok-secret", dataDir: dataDir,
				exports: map[string][]byte{"exp_1": []byte("# note\nbody\n")}}
			h.remotes = append(h.remotes, r)
			return r, nil
		},
		Reveal: func(p string) error { h.revealed = append(h.revealed, p); return nil },
	})
	h.app.openURL = func(_ context.Context, u string) { h.opened = append(h.opened, u) }
	h.app.saveFile = func(_ context.Context, name string) (string, error) { h.saveName = name; return h.saveTo, nil }
	h.app.pickFile = func(context.Context) (string, error) { return "/videos/a.mp4", nil }
	return h
}

func wantCode(t *testing.T, err error, code app.Code) {
	t.Helper()
	if !app.IsCode(err, code) {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func TestEngineInfoConnectsAndReconnects(t *testing.T) {
	h := newHarness(t, t.TempDir())
	info, err := h.app.EngineInfo()
	if err != nil {
		t.Fatal(err)
	}
	want := EngineInfo{BaseURL: "http://127.0.0.1:47823", Token: "tok-secret", Version: "1.2.3"}
	if info != want {
		t.Fatalf("got %+v, want %+v", info, want)
	}
	b, _ := json.Marshal(info)
	if string(b) != `{"base_url":"http://127.0.0.1:47823","token":"tok-secret","version":"1.2.3"}` {
		t.Fatalf("JSON shape: %s", b)
	}
	if len(h.connects) != 1 || h.connects[0].DataDir != "/flag/data" || h.connects[0].Requester != "desktop" || h.connects[0].NoSpawn {
		t.Fatalf("connect options: %+v", h.connects)
	}

	// A healthy connection is reused.
	if _, err := h.app.EngineInfo(); err != nil || len(h.connects) != 1 {
		t.Fatalf("connection not reused: %v, %d connects", err, len(h.connects))
	}

	// The engine went away (idle exit): the next call reconnects.
	h.remotes[0].dead = true
	if _, err := h.app.EngineInfo(); err != nil {
		t.Fatal(err)
	}
	if len(h.connects) != 2 || !h.remotes[0].closed {
		t.Fatalf("expected a reconnect and the old remote closed (connects=%d)", len(h.connects))
	}

	h.app.close()
	if !h.remotes[1].closed {
		t.Fatal("close must close the remote")
	}
}

func TestEngineInfoErrors(t *testing.T) {
	a := newApp(Options{Connect: func(context.Context, app.ConnectOptions) (app.Remote, error) {
		return nil, app.E(app.CodeEngineUnavailable, "cannot start")
	}})
	_, err := a.EngineInfo()
	wantCode(t, err, app.CodeEngineUnavailable)

	a = newApp(Options{Connect: func(context.Context, app.ConnectOptions) (app.Remote, error) {
		return tokenless{}, nil
	}})
	_, err = a.EngineInfo()
	wantCode(t, err, app.CodeInternal)

	// Falls back to the engine's version when Options.Version is empty.
	h := newHarness(t, t.TempDir())
	h.app.opts.Version = ""
	info, _ := h.app.EngineInfo()
	if info.Version != "9.9.9" {
		t.Fatalf("version = %q", info.Version)
	}
}

func TestPickVideoFile(t *testing.T) {
	h := newHarness(t, t.TempDir())
	if p, err := h.app.PickVideoFile(); err != nil || p != "/videos/a.mp4" {
		t.Fatalf("%q %v", p, err)
	}
	h.app.pickFile = func(context.Context) (string, error) { return "", nil }
	if p, err := h.app.PickVideoFile(); err != nil || p != "" {
		t.Fatalf("cancel must return an empty path: %q %v", p, err)
	}
	h.app.pickFile = func(context.Context) (string, error) { return "", errors.New("boom") }
	_, err := h.app.PickVideoFile()
	wantCode(t, err, app.CodeInternal)
}

func TestSaveExport(t *testing.T) {
	h := newHarness(t, t.TempDir())
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.md")
	os.WriteFile(dest, []byte("old"), 0o600) // overwritten atomically

	h.saveTo = dest
	got, err := h.app.SaveExport("exp_1", "../../My Note.md")
	if err != nil || got != dest {
		t.Fatalf("SaveExport = %q, %v", got, err)
	}
	if h.saveName != "My Note.md" {
		t.Fatalf("suggested name not sanitized: %q", h.saveName)
	}
	b, _ := os.ReadFile(dest)
	if string(b) != "# note\nbody\n" {
		t.Fatalf("content: %q", b)
	}
	if fi, _ := os.Stat(dest); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}

	// Empty suggestion falls back to the export's own file name.
	if _, err := h.app.SaveExport("exp_1", ""); err != nil || h.saveName != "note.md" {
		t.Fatalf("fallback name = %q, %v", h.saveName, err)
	}

	// Cancelled dialog: empty path, nothing written, no second download.
	h.saveTo = ""
	before := h.remotes[0].opens
	if got, err := h.app.SaveExport("exp_1", "x.md"); err != nil || got != "" {
		t.Fatalf("cancel: %q %v", got, err)
	}
	if h.remotes[0].opens != before+1 {
		t.Fatalf("a cancelled save must not download the export")
	}

	// Unknown export fails before any dialog is shown.
	h.saveName = ""
	_, err = h.app.SaveExport("exp_missing", "x.md")
	wantCode(t, err, app.CodeNotFound)
	if h.saveName != "" {
		t.Fatal("dialog shown for an unknown export")
	}
	_, err = h.app.SaveExport("", "x.md")
	wantCode(t, err, app.CodeInvalidArgument)

	// A truncated download leaves the existing file untouched.
	h.saveTo = dest
	h.remotes[0].corrupt = true
	if _, err := h.app.SaveExport("exp_1", "x.md"); err == nil {
		t.Fatal("truncated download must fail")
	}
	b, _ = os.ReadFile(dest)
	if string(b) != "# note\nbody\n" {
		t.Fatalf("existing file damaged: %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp files left after failure: %v", entries)
	}
}

func TestWriteExportChecksum(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "f.txt")
	err := writeExport(dest, strings.NewReader("abc"), &app.Export{Bytes: 3, SHA256: strings.Repeat("0", 64)})
	wantCode(t, err, app.CodeChecksumMismatch)
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("file must not exist after a checksum failure")
	}
	if err := writeExport(dest, strings.NewReader("abc"), nil); err != nil {
		t.Fatal(err)
	}
}

func TestSanitizeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"note.md": "note.md", "a/b/c.srt": "c.srt", `C:\x\y.txt`: "y.txt", "..": "", ".": "", "": "",
		" 讲座 笔记.md ": "讲座 笔记.md", "a\x00b:c.md": "abc.md", "dir/": "",
	} {
		if got := sanitizeFileName(in); got != want {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRevealInFileManager(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	sibling := filepath.Join(base, "data2")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(data, "exports"), sibling, outside} {
		os.MkdirAll(d, 0o755)
	}
	inFile := filepath.Join(data, "exports", "a.md")
	os.WriteFile(inFile, []byte("x"), 0o644)
	secret := filepath.Join(outside, "s.txt")
	os.WriteFile(secret, []byte("x"), 0o644)
	os.WriteFile(filepath.Join(sibling, "b.md"), []byte("x"), 0o644)
	os.Symlink(secret, filepath.Join(data, "escape"))
	os.Symlink(data, filepath.Join(base, "datalink"))
	realIn, _ := filepath.EvalSymlinks(inFile)
	realData, _ := filepath.EvalSymlinks(data)

	tests := []struct {
		name, path string
		code       app.Code
		revealed   string
	}{
		{"file inside", inFile, "", realIn},
		{"data dir itself", data, "", realData},
		{"via symlinked path", filepath.Join(base, "datalink", "exports", "a.md"), "", realIn},
		{"relative", "exports/a.md", app.CodeInvalidArgument, ""},
		{"empty", "", app.CodeInvalidArgument, ""},
		{"missing", filepath.Join(data, "nope"), app.CodeNotFound, ""},
		{"outside", secret, app.CodePathNotAllowed, ""},
		{"prefix sibling", filepath.Join(sibling, "b.md"), app.CodePathNotAllowed, ""},
		{"dotdot", filepath.Join(data, "..", "outside", "s.txt"), app.CodePathNotAllowed, ""},
		{"symlink escape", filepath.Join(data, "escape"), app.CodePathNotAllowed, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, data)
			err := h.app.RevealInFileManager(tc.path)
			if tc.code != "" {
				wantCode(t, err, tc.code)
				if len(h.revealed) != 0 {
					t.Fatalf("Reveal called for a rejected path: %v", h.revealed)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(h.revealed, []string{tc.revealed}) {
				t.Fatalf("revealed %v, want %s", h.revealed, tc.revealed)
			}
		})
	}

	h := newHarness(t, data)
	h.app.opts.Reveal = nil
	wantCode(t, h.app.RevealInFileManager(inFile), app.CodeInternal)
}

func TestOpenExternal(t *testing.T) {
	h := newHarness(t, t.TempDir())
	for _, ok := range []string{"https://example.com/a?b=c", "http://127.0.0.1:47823/x", " https://github.com "} {
		if err := h.app.OpenExternal(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	if len(h.opened) != 3 || h.opened[2] != "https://github.com" {
		t.Fatalf("opened: %v", h.opened)
	}
	for _, bad := range []string{"", "file:///etc/passwd", "javascript:alert(1)", "example.com", "https://", "ftp://x/y", "wk://seg/1", "HTTPS//x"} {
		wantCode(t, h.app.OpenExternal(bad), app.CodeInvalidArgument)
	}
	if len(h.opened) != 3 {
		t.Fatalf("a rejected URL was opened: %v", h.opened)
	}
}

func TestAgentSetupInfo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses macOS paths and POSIX quoting; see TestWindowsAgentSetup")
	}
	a := newApp(Options{Executable: "/Applications/Watchink.app/Contents/MacOS/Watchink"})
	s, err := a.AgentSetupInfo()
	if err != nil {
		t.Fatal(err)
	}
	exe := "/Applications/Watchink.app/Contents/MacOS/Watchink"
	if s.Executable != exe || !reflect.DeepEqual(s.MCPCommand, []string{exe, "mcp"}) {
		t.Fatalf("%+v", s)
	}
	if len(s.Snippets) != 2 {
		t.Fatalf("snippets: %+v", s.Snippets)
	}
	if got := s.Snippets[0]; got.Name != "Claude Code" || got.Language != "bash" ||
		got.Content != "claude mcp add watchink -- "+exe+" mcp" {
		t.Fatalf("claude code snippet: %+v", got)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(s.Snippets[1].Content), &cfg); err != nil {
		t.Fatal(err)
	}
	srv := cfg.MCPServers["watchink"]
	if s.Snippets[1].Language != "json" || srv.Command != exe || !reflect.DeepEqual(srv.Args, []string{"mcp"}) {
		t.Fatalf("claude desktop snippet: %s", s.Snippets[1].Content)
	}

	b, _ := json.Marshal(s)
	for _, key := range []string{`"executable"`, `"mcp_command"`, `"snippets"`, `"name"`, `"language"`, `"content"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("JSON shape lacks %s: %s", key, b)
		}
	}

	// A path with spaces or quotes is shell-quoted in the bash snippet.
	a = newApp(Options{Executable: "/Users/o'neil/My Apps/Watchink"})
	s, _ = a.AgentSetupInfo()
	if want := `claude mcp add watchink -- '/Users/o'\''neil/My Apps/Watchink' mcp`; s.Snippets[0].Content != want {
		t.Fatalf("quoting: %s", s.Snippets[0].Content)
	}

	// Default: the running executable, absolute.
	s, err = newApp(Options{}).AgentSetupInfo()
	if err != nil || !filepath.IsAbs(s.Executable) {
		t.Fatalf("default executable: %q %v", s.Executable, err)
	}
}

func TestFormatError(t *testing.T) {
	out, ok := formatError(app.E(app.CodePathNotAllowed, "nope").WithHint("h")).(string)
	if !ok {
		t.Fatal("formatError must return a JSON string")
	}
	var e app.Error
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != app.CodePathNotAllowed || e.Message != "nope" || e.Hint != "h" || !e.UserActionRequired {
		t.Fatalf("%+v", e)
	}
	out = formatError(errors.New("plain")).(string)
	json.Unmarshal([]byte(out), &e)
	if e.Code != app.CodeInternal {
		t.Fatalf("plain errors become INTERNAL: %s", out)
	}
	if formatError(nil) != nil {
		t.Fatal("nil stays nil")
	}
}

// TestBoundSurface pins the binding names the frontend relies on (§13.6).
func TestBoundSurface(t *testing.T) {
	typ := reflect.TypeOf(&App{})
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	want := []string{"AgentSetupInfo", "EngineInfo", "OpenExternal", "PickVideoFile", "RevealInFileManager", "SaveExport"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exported methods = %v, want %v", got, want)
	}
	if typ.Elem().Name() != "App" || !strings.HasSuffix(typ.Elem().PkgPath(), "/desktop") {
		t.Fatalf("binding must be desktop.App, got %s.%s", typ.Elem().PkgPath(), typ.Elem().Name())
	}
}
