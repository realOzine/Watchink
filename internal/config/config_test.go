package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"watchink/internal/app"
)

// isolate points the user config dir at a temp dir and clears the key
// variables so tests never touch the real user configuration.
func isolate(t *testing.T) (cfgDir string) {
	t.Helper()
	cfgDir = filepath.Join(t.TempDir(), "cfg")
	t.Setenv(EnvConfigDir, cfgDir)
	t.Setenv(EnvDataDir, "")
	t.Setenv(EnvLLMAPIKey, "")
	return cfgDir
}

func wantCode(t *testing.T, err error, code app.Code, keyPart string) {
	t.Helper()
	if !app.IsCode(err, code) {
		t.Fatalf("error = %v, want code %s", err, code)
	}
	if keyPart != "" && !strings.Contains(err.Error(), keyPart) {
		t.Fatalf("error %q does not name %q", err, keyPart)
	}
}

func TestLoadDefaultsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := app.DefaultSettings()
	want.DataDir = dir
	want.Security.ImportRoots = []string{"~"}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("got %+v\nwant %+v", s, want)
	}
	if err := Validate(app.DefaultSettings()); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "data") // Save creates it
	s := app.DefaultSettings()
	s.Engine.Port = 5000
	s.Engine.AutoResume = false
	s.ASR.Language = "zh"
	s.ASR.VAD = false
	s.LLM.BaseURL = "https://api.example.com/v1"
	s.LLM.Model = "m-1"
	s.LLM.APIKeyEnv = "MY_KEY"
	s.LLM.Temperature = 0
	s.Download.Mirrors = map[string]string{"huggingface.co": "https://hf-mirror.com"}
	s.Security.ImportRoots = []string{"~", "/Volumes/Media"}
	s.Network.Proxy = "socks5://127.0.0.1:1080"
	// Read-only fields must not reach the file.
	s.DataDir = "/should/not/be/written"
	s.ConfigDir = "/nor/this"
	s.LLM.APIKeyConfigured = true
	s.LLM.APIKeyLast4 = "WXYZ"
	s.LLM.APIKeySource = "file"

	if err := Save(dir, s); err != nil {
		t.Fatal(err)
	}
	p := SettingsFile(dir)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(p)
	for _, leak := range []string{"should/not", "nor/this", "WXYZ", "api_key_configured", "api_key_source", "data_dir"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("config.toml contains read-only data %q:\n%s", leak, raw)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := s
	want.DataDir = dir
	want.ConfigDir = ""
	want.LLM.APIKeyConfigured, want.LLM.APIKeyLast4, want.LLM.APIKeySource = false, "", ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch\n got %+v\nwant %+v", got, want)
	}

	// Saving again over an existing file works and keeps the mode.
	got.LLM.Model = "m-2"
	if err := Save(dir, got); err != nil {
		t.Fatal(err)
	}
	again, _ := Load(dir)
	if again.LLM.Model != "m-2" {
		t.Fatalf("second save not visible: %q", again.LLM.Model)
	}
}

func TestLoadPartialUnknownAndInvalid(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(SettingsFile(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("future_top = 1\n[llm]\nmodel = \"x\"\nfuture_key = true\n[future]\na = 1\n[engine]\nauto_resume = false\n")
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("unknown keys must be tolerated: %v", err)
	}
	if s.LLM.Model != "x" || s.Engine.AutoResume || s.LLM.Timeout != "120s" || s.Engine.Port != app.DefaultEnginePort || !s.Tools.AutoPrepare {
		t.Fatalf("overlay wrong: %+v", s)
	}

	write("[llm\nmodel = ")
	_, err = Load(dir)
	wantCode(t, err, app.CodeInvalidArgument, "config.toml")

	write("[asr]\nlanguage = \"fr\"\n")
	_, err = Load(dir)
	wantCode(t, err, app.CodeInvalidArgument, "asr.language")

	write("[engine]\nport = \"abc\"\n")
	_, err = Load(dir)
	wantCode(t, err, app.CodeInvalidArgument, "config.toml")
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*app.Settings)
		key  string // empty = valid
	}{
		{"defaults", func(s *app.Settings) {}, ""},
		{"port 0 (random)", func(s *app.Settings) { s.Engine.Port = 0 }, ""},
		{"port negative", func(s *app.Settings) { s.Engine.Port = -1 }, "engine.port"},
		{"port high", func(s *app.Settings) { s.Engine.Port = 65536 }, "engine.port"},
		{"idle_exit bad", func(s *app.Settings) { s.Engine.IdleExit = "soon" }, "engine.idle_exit"},
		{"idle_exit negative", func(s *app.Settings) { s.Engine.IdleExit = "-1m" }, "engine.idle_exit"},
		{"language", func(s *app.Settings) { s.ASR.Language = "de" }, "asr.language"},
		{"language en", func(s *app.Settings) { s.ASR.Language = "en" }, ""},
		{"threads", func(s *app.Settings) { s.ASR.Threads = -2 }, "asr.threads"},
		{"structured_output", func(s *app.Settings) { s.LLM.StructuredOutput = "xml" }, "llm.structured_output"},
		{"structured_output prompt", func(s *app.Settings) { s.LLM.StructuredOutput = "prompt" }, ""},
		{"temperature high", func(s *app.Settings) { s.LLM.Temperature = 2.01 }, "llm.temperature"},
		{"temperature low", func(s *app.Settings) { s.LLM.Temperature = -0.1 }, "llm.temperature"},
		{"temperature 2", func(s *app.Settings) { s.LLM.Temperature = 2 }, ""},
		{"base_url http", func(s *app.Settings) { s.LLM.BaseURL = "http://localhost:11434/v1" }, ""},
		{"base_url scheme", func(s *app.Settings) { s.LLM.BaseURL = "ftp://x/v1" }, "llm.base_url"},
		{"base_url no scheme", func(s *app.Settings) { s.LLM.BaseURL = "api.example.com/v1" }, "llm.base_url"},
		{"base_url no host", func(s *app.Settings) { s.LLM.BaseURL = "https:///v1" }, "llm.base_url"},
		{"api_key_env", func(s *app.Settings) { s.LLM.APIKeyEnv = "MY-KEY" }, "llm.api_key_env"},
		{"timeout zero", func(s *app.Settings) { s.LLM.Timeout = "0s" }, "llm.timeout"},
		{"timeout bad", func(s *app.Settings) { s.LLM.Timeout = "120" }, "llm.timeout"},
		{"max_concurrency", func(s *app.Settings) { s.LLM.MaxConcurrency = 0 }, "llm.max_concurrency"},
		{"chunk tokens", func(s *app.Settings) { s.Note.ChunkMaxTokens = 0 }, "note.chunk_max_tokens"},
		{"chunk duration", func(s *app.Settings) { s.Note.ChunkMaxDuration = "0m" }, "note.chunk_max_duration"},
		{"playback height", func(s *app.Settings) { s.Media.PlaybackMaxHeight = -1 }, "media.playback_max_height"},
		{"proxy ok", func(s *app.Settings) { s.Network.Proxy = "http://user:pw@127.0.0.1:7890" }, ""},
		{"proxy bad", func(s *app.Settings) { s.Network.Proxy = "127.0.0.1:7890" }, "network.proxy"},
		{"mirror ok", func(s *app.Settings) { s.Download.Mirrors = map[string]string{"github.com": "http://mirror.local/gh"} }, ""},
		{"mirror key is url", func(s *app.Settings) {
			s.Download.Mirrors = map[string]string{"https://github.com": "https://m.example"}
		}, "download.mirrors"},
		{"mirror value", func(s *app.Settings) { s.Download.Mirrors = map[string]string{"github.com": "m.example"} }, "download.mirrors.github.com"},
		{"root relative", func(s *app.Settings) { s.Security.ImportRoots = []string{"videos"} }, "security.import_roots[0]"},
		{"root empty list", func(s *app.Settings) { s.Security.ImportRoots = nil }, ""},
		{"size bad", func(s *app.Settings) { s.Security.MaxUploadSize = "big" }, "security.max_upload_size"},
		{"size zero", func(s *app.Settings) { s.Security.MaxUploadSize = "0" }, "security.max_upload_size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := app.DefaultSettings()
			tc.mut(&s)
			err := Validate(s)
			if tc.key == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			wantCode(t, err, app.CodeInvalidArgument, tc.key)
		})
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"10m": 10 * time.Minute, "120s": 2 * time.Minute, "1h30m": 90 * time.Minute, "0": 0, " 5s ": 5 * time.Second,
	} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "10", "abc", "-5s", "1d"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) should fail", in)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"8GiB": 8 << 30, "500MiB": 500 << 20, "1GB": 1e9, "1048576": 1 << 20, "0": 0,
		"1.5GiB": 3 << 29, "2 kb": 2000, "64K": 64 << 10, "10B": 10, " 3TiB ": 3 << 40, "1gib": 1 << 30,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "GiB", "-1", "1XB", "1.5", "1..2GB", "99999999999TiB", "8 GiB x"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) should fail", in)
		}
	}
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestApplyPatch(t *testing.T) {
	cur := app.DefaultSettings()
	cur.DataDir = "/data"
	cur.LLM.APIKeyConfigured = true
	cur.Download.Mirrors = map[string]string{"github.com": "https://gh.example"}

	got, err := ApplyPatch(cur, decode(t, `{
		"llm": {"model": "x", "temperature": 1, "max_concurrency": 5, "base_url": "https://api.example.com/v1"},
		"download": {"mirrors": {"huggingface.co": "https://hf-mirror.com"}},
		"engine": {"auto_resume": false, "port": 5001},
		"security": {"import_roots": ["~", "/srv/media"]},
		"asr": {"language": "zh"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.LLM.Model != "x" || got.LLM.Temperature != 1 || got.LLM.MaxConcurrency != 5 ||
		got.Engine.AutoResume || got.Engine.Port != 5001 || got.ASR.Language != "zh" {
		t.Fatalf("patch not applied: %+v", got)
	}
	if !reflect.DeepEqual(got.Download.Mirrors, map[string]string{
		"github.com": "https://gh.example", "huggingface.co": "https://hf-mirror.com"}) {
		t.Fatalf("mirrors must merge: %v", got.Download.Mirrors)
	}
	if !reflect.DeepEqual(got.Security.ImportRoots, []string{"~", "/srv/media"}) {
		t.Fatalf("roots: %v", got.Security.ImportRoots)
	}
	// Untouched and read-only values survive.
	if got.LLM.Timeout != "120s" || got.DataDir != "/data" || !got.LLM.APIKeyConfigured {
		t.Fatalf("unrelated fields changed: %+v", got)
	}
	// cur is not modified (maps and slices are copied).
	if len(cur.Download.Mirrors) != 1 || cur.LLM.Model != "" || len(cur.Security.ImportRoots) != 1 {
		t.Fatalf("cur was modified: %+v", cur)
	}

	// null deletes a map entry / resets a scalar to its default.
	got2, err := ApplyPatch(got, decode(t, `{"download":{"mirrors":{"github.com":null}},"llm":{"temperature":null},"engine":{"port":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got2.Download.Mirrors["github.com"]; ok || len(got2.Download.Mirrors) != 1 {
		t.Fatalf("null must delete the mirror: %v", got2.Download.Mirrors)
	}
	if got2.LLM.Temperature != 0.2 || got2.Engine.Port != app.DefaultEnginePort {
		t.Fatalf("null must reset to default: %+v", got2)
	}
	got3, err := ApplyPatch(got2, decode(t, `{"download":{"mirrors":null}}`))
	if err != nil || len(got3.Download.Mirrors) != 0 || got3.Download.Mirrors == nil {
		t.Fatalf("mirrors:null must clear: %v %v", got3.Download.Mirrors, err)
	}

	// Empty and nil patches are no-ops; in-process callers may pass Go ints.
	if same, err := ApplyPatch(cur, nil); err != nil || !reflect.DeepEqual(same, cur) {
		t.Fatalf("nil patch: %v", err)
	}
	g, err := ApplyPatch(cur, map[string]any{"llm": map[string]any{"max_concurrency": 2, "temperature": 1}})
	if err != nil || g.LLM.MaxConcurrency != 2 || g.LLM.Temperature != 1 {
		t.Fatalf("go-typed patch: %+v %v", g.LLM, err)
	}
}

func TestApplyPatchErrors(t *testing.T) {
	tests := []struct {
		name, patch, key string
	}{
		{"unknown top", `{"nope": 1}`, "nope"},
		{"unknown nested", `{"llm": {"modle": "x"}}`, "llm.modle"},
		{"read-only data_dir", `{"data_dir": "/x"}`, "data_dir"},
		{"read-only config_dir", `{"config_dir": "/x"}`, "config_dir"},
		{"read-only configured", `{"llm": {"api_key_configured": true}}`, "llm.api_key_configured"},
		{"read-only last4", `{"llm": {"api_key_last4": "abcd"}}`, "llm.api_key_last4"},
		{"read-only source", `{"llm": {"api_key_source": "env"}}`, "llm.api_key_source"},
		{"secret smuggling", `{"llm": {"api_key": "sk-x"}}`, "llm.api_key"},
		{"section not object", `{"llm": "x"}`, "llm"},
		{"string type", `{"llm": {"model": 5}}`, "llm.model"},
		{"bool type", `{"asr": {"vad": "yes"}}`, "asr.vad"},
		{"int type", `{"engine": {"port": "80"}}`, "engine.port"},
		{"int fraction", `{"engine": {"port": 80.5}}`, "engine.port"},
		{"float type", `{"llm": {"temperature": "hot"}}`, "llm.temperature"},
		{"slice type", `{"security": {"import_roots": "~"}}`, "security.import_roots"},
		{"slice elem", `{"security": {"import_roots": ["~", 3]}}`, "security.import_roots[1]"},
		{"map type", `{"download": {"mirrors": ["a"]}}`, "download.mirrors"},
		{"map value", `{"download": {"mirrors": {"a.com": 1}}}`, "download.mirrors.a.com"},
		{"validation", `{"asr": {"language": "jp"}}`, "asr.language"},
		{"validation range", `{"llm": {"temperature": 3}}`, "llm.temperature"},
		{"validation mirror", `{"download": {"mirrors": {"a.com": "not a url"}}}`, "download.mirrors.a.com"},
	}
	cur := app.DefaultSettings()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApplyPatch(cur, decode(t, tc.patch))
			wantCode(t, err, app.CodeInvalidArgument, tc.key)
			if k := app.AsError(err).Details["key"]; k != tc.key {
				t.Fatalf("details.key = %v, want %q", k, tc.key)
			}
			if !reflect.DeepEqual(got, cur) {
				t.Fatalf("a failed patch must return cur unchanged")
			}
		})
	}
}

// TestPatchCoversEverySetting guards the reflection walk against a new field
// kind being added to app.Settings without patch support.
func TestPatchCoversEverySetting(t *testing.T) {
	s := app.DefaultSettings()
	s.LLM.BaseURL = "https://e.example/v1"
	b, _ := json.Marshal(s)
	patch := decode(t, string(b))
	delete(patch["llm"].(map[string]any), "api_key_configured")
	got, err := ApplyPatch(app.DefaultSettings(), patch)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("full-shape patch mismatch\n got %+v\nwant %+v", got, s)
	}
}

func TestSecretsLifecycle(t *testing.T) {
	cfg := isolate(t)
	p, err := SecretsFile()
	if err != nil || p != filepath.Join(cfg, "secrets.toml") {
		t.Fatalf("SecretsFile = %q, %v", p, err)
	}

	if sec, err := LoadSecrets(); err != nil || sec.LLMAPIKey != "" {
		t.Fatalf("missing file: %+v %v", sec, err)
	}
	// Deleting when nothing exists must not create the directory.
	if err := SetSecret(app.SecretLLMAPIKey, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatalf("config dir should not exist yet: %v", err)
	}

	if err := SetSecret(app.SecretLLMAPIKey, "  sk-test-1234567890  "); err != nil {
		t.Fatal(err)
	}
	di, _ := os.Stat(cfg)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", di.Mode().Perm())
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "[llm]") || !strings.Contains(string(raw), "api_key = 'sk-test-1234567890'") &&
		!strings.Contains(string(raw), `api_key = "sk-test-1234567890"`) {
		t.Fatalf("unexpected file shape:\n%s", raw)
	}
	sec, err := LoadSecrets()
	if err != nil || sec.LLMAPIKey != "sk-test-1234567890" {
		t.Fatalf("LoadSecrets = %+v, %v", sec, err)
	}

	// Overwrite, then delete.
	if err := SetSecret(app.SecretLLMAPIKey, "sk-other-000000000"); err != nil {
		t.Fatal(err)
	}
	if sec, _ := LoadSecrets(); sec.LLMAPIKey != "sk-other-000000000" {
		t.Fatalf("overwrite failed")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after overwrite = %v", fi.Mode().Perm())
	}
	if err := SetSecret(app.SecretLLMAPIKey, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("secrets file should be removed: %v", err)
	}
	if entries, _ := os.ReadDir(cfg); len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}

	wantCode(t, SetSecret("other", "v"), app.CodeInvalidArgument, "other")
	wantCode(t, SetSecret(app.SecretLLMAPIKey, "a\nb"), app.CodeInvalidArgument, "")
}

func TestSecretsRefuseSymlink(t *testing.T) {
	cfg := isolate(t)
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "victim.toml")
	if err := os.WriteFile(target, []byte("[llm]\napi_key = \"stolen-key-0000\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cfg, "secrets.toml")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecrets(); err == nil {
		t.Fatal("LoadSecrets must refuse a symlinked secrets file")
	}
	if err := SetSecret(app.SecretLLMAPIKey, "sk-new-key-12345"); err == nil {
		t.Fatal("SetSecret must refuse a symlinked secrets file")
	}
	if err := SetSecret(app.SecretLLMAPIKey, ""); err == nil {
		t.Fatal("deleting through a symlink must be refused")
	}
	b, _ := os.ReadFile(target)
	if !strings.Contains(string(b), "stolen-key-0000") {
		t.Fatalf("symlink target was modified: %s", b)
	}
	if key, src := ResolveLLMAPIKey(app.DefaultSettings()); key != "" || src != "" {
		t.Fatalf("key resolved through a symlink (source %q)", src)
	}
}

func TestSecretsErrorDoesNotLeakContent(t *testing.T) {
	cfg := isolate(t)
	os.MkdirAll(cfg, 0o700)
	os.WriteFile(filepath.Join(cfg, "secrets.toml"), []byte("[llm]\napi_key = sk-unquoted-secret-9999\n"), 0o600)
	_, err := LoadSecrets()
	if err == nil {
		t.Fatal("expected a TOML error")
	}
	if strings.Contains(err.Error(), "sk-unquoted") {
		t.Fatalf("error leaks file content: %v", err)
	}
}

func TestResolveLLMAPIKeyPriority(t *testing.T) {
	isolate(t)
	s := app.DefaultSettings()
	s.LLM.APIKeyEnv = "WK_TEST_CUSTOM_KEY"
	t.Setenv("WK_TEST_CUSTOM_KEY", "")

	check := func(wantKey, wantSource string) {
		t.Helper()
		key, src := ResolveLLMAPIKey(s)
		if key != wantKey || src != wantSource {
			t.Fatalf("got (%d chars, %q), want (%d chars, %q)", len(key), src, len(wantKey), wantSource)
		}
	}

	check("", "")
	Describe(&s)
	if s.LLM.APIKeyConfigured || s.LLM.APIKeyLast4 != "" || s.LLM.APIKeySource != "" {
		t.Fatalf("unconfigured state wrong: %+v", s.LLM)
	}

	if err := SetSecret(app.SecretLLMAPIKey, "file-key-00001111"); err != nil {
		t.Fatal(err)
	}
	check("file-key-00001111", "file")

	t.Setenv("WK_TEST_CUSTOM_KEY", "custom-key-00002222")
	check("custom-key-00002222", "env:WK_TEST_CUSTOM_KEY")

	t.Setenv(EnvLLMAPIKey, "main-key-000033334444")
	check("main-key-000033334444", "env")
	Describe(&s)
	if !s.LLM.APIKeyConfigured || s.LLM.APIKeyLast4 != "4444" || s.LLM.APIKeySource != "env" {
		t.Fatalf("Describe: %+v", s.LLM)
	}

	// Whitespace-only variables do not count; fall through in order.
	t.Setenv(EnvLLMAPIKey, "  ")
	check("custom-key-00002222", "env:WK_TEST_CUSTOM_KEY")
	s.LLM.APIKeyEnv = ""
	check("file-key-00001111", "file")
	Describe(&s)
	if s.LLM.APIKeyLast4 != "1111" || s.LLM.APIKeySource != "file" {
		t.Fatalf("Describe: %+v", s.LLM)
	}
}

func TestDescribe(t *testing.T) {
	cfg := isolate(t)
	dataDir := t.TempDir()
	s, err := Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	// A short key is reported as configured but its tail is not revealed.
	t.Setenv(EnvLLMAPIKey, "short")
	Describe(&s)
	if s.DataDir != dataDir || s.ConfigDir != cfg {
		t.Fatalf("dirs: %q %q", s.DataDir, s.ConfigDir)
	}
	if !s.LLM.APIKeyConfigured || s.LLM.APIKeyLast4 != "" {
		t.Fatalf("short key: %+v", s.LLM)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), `"short"`) {
		t.Fatalf("settings JSON contains the key: %s", b)
	}

	// With no DataDir, Describe resolves it the standard way.
	t.Setenv(EnvDataDir, dataDir)
	var bare app.Settings
	Describe(&bare)
	if bare.DataDir != dataDir {
		t.Fatalf("resolved data dir = %q", bare.DataDir)
	}
}

func TestConfigDirIsolation(t *testing.T) {
	cfg := isolate(t)
	got, err := ConfigDir()
	if err != nil || got != cfg {
		t.Fatalf("ConfigDir = %q, %v; want %q", got, err, cfg)
	}
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(got, filepath.Join(home, "Library")) {
		t.Fatalf("tests must not use the real config dir: %s", got)
	}
}

func TestExpandRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	realHome, _ := filepath.EvalSymlinks(home)
	sub := filepath.Join(home, "Movies")
	os.Mkdir(sub, 0o755)
	link := filepath.Join(home, "link")
	os.Symlink(sub, link)

	got, err := ExpandRoots([]string{"~", "~/Movies", link, filepath.Join(home, "missing")})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{realHome, filepath.Join(realHome, "Movies"), filepath.Join(realHome, "Movies")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for _, bad := range []string{"relative/dir", "", "~other/x"} {
		_, err := ExpandRoots([]string{bad})
		wantCode(t, err, app.CodeInvalidArgument, "")
	}
}

func TestCheckImportPath(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	sibling := filepath.Join(base, "rootx") // shares the string prefix of root
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "sub"), sibling, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(p string) string {
		t.Helper()
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	inside := mk(filepath.Join(root, "sub", "a.mp4"))
	inSibling := mk(filepath.Join(sibling, "b.mp4"))
	secret := mk(filepath.Join(outside, "secret.mp4"))

	escape := filepath.Join(root, "escape.mp4") // symlink in root → outside
	os.Symlink(secret, escape)
	dirEscape := filepath.Join(root, "dirlink") // symlinked dir in root → outside
	os.Symlink(outside, dirEscape)
	inward := filepath.Join(outside, "inward.mp4") // symlink outside → in root
	os.Symlink(inside, inward)
	rootLink := filepath.Join(base, "rootlink") // a root given as a symlink
	os.Symlink(root, rootLink)

	realInside, _ := filepath.EvalSymlinks(inside)
	roots := []string{root}

	tests := []struct {
		name  string
		path  string
		roots []string
		code  app.Code // empty = allowed
	}{
		{"inside", inside, roots, ""},
		{"inside with dotdot", filepath.Join(root, "sub", "..", "sub", "a.mp4"), roots, ""},
		{"symlink pointing in", inward, roots, ""},
		{"root is a symlink", inside, []string{rootLink}, ""},
		{"second root matches", inside, []string{outside, root}, ""},
		{"empty", "", roots, app.CodeInvalidArgument},
		{"relative", "root/sub/a.mp4", roots, app.CodeInvalidArgument},
		{"missing", filepath.Join(root, "nope.mp4"), roots, app.CodeNotFound},
		{"directory", filepath.Join(root, "sub"), roots, app.CodeInvalidArgument},
		{"outside", secret, roots, app.CodePathNotAllowed},
		{"string-prefix sibling", inSibling, roots, app.CodePathNotAllowed},
		{"dotdot escape", filepath.Join(root, "..", "outside", "secret.mp4"), roots, app.CodePathNotAllowed},
		{"symlink escape", escape, roots, app.CodePathNotAllowed},
		{"symlinked dir escape", filepath.Join(dirEscape, "secret.mp4"), roots, app.CodePathNotAllowed},
		{"no roots", inside, nil, app.CodePathNotAllowed},
		{"only missing root", inside, []string{filepath.Join(base, "gone")}, app.CodePathNotAllowed},
		{"bad root", inside, []string{"relative"}, app.CodeInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckImportPath(tc.path, tc.roots)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != realInside {
					t.Fatalf("resolved = %q, want %q", got, realInside)
				}
				return
			}
			wantCode(t, err, tc.code, "")
			if got != "" {
				t.Fatalf("resolved path returned with an error: %q", got)
			}
		})
	}

	// "~" as a root follows HOME.
	t.Setenv("HOME", root)
	if _, err := CheckImportPath(inside, []string{"~"}); err != nil {
		t.Fatalf("~ root: %v", err)
	}
	if _, err := CheckImportPath(secret, []string{"~"}); !app.IsCode(err, app.CodePathNotAllowed) {
		t.Fatalf("~ root must not allow files outside: %v", err)
	}
}
