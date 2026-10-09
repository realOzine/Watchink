package engine_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"watchink/internal/engine"
)

func TestRedactor(t *testing.T) {
	var r engine.Redactor
	r.Add("engine-token-AAAABBBBCCCC")
	r.Add("abc") // too short to register
	cases := []struct{ in, gone, keep string }{
		{"token is engine-token-AAAABBBBCCCC here", "engine-token-AAAABBBBCCCC", "here"},
		{"Authorization: Bearer abcdefghijklmnop", "abcdefghijklmnop", "Authorization"},
		{`{"Authorization":"Bearer zzzzyyyyxxxxwwww"}`, "zzzzyyyyxxxxwwww", "Authorization"},
		{"header authorization=Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA", "header"},
		{"key sk-proj-1234567890abcdef used", "sk-proj-1234567890abcdef", "used"},
		{"GET /auth/login?code=LOGINCODE123&next=/x", "LOGINCODE123", "next=/x"},
		{"GET /media/v/playback?x=1&mt=MEDIATOKEN99", "MEDIATOKEN99", "x=1"},
		{`body {"api_key":"plainsecretvalue","model":"gpt"}`, "plainsecretvalue", `"model":"gpt"`},
		{`nested {\"api_key\":\"escapedsecret1\",\"n\":1}`, "escapedsecret1", `\"n\":1`},
		{"api_key=querysecretvalue&model=x", "querysecretvalue", "model=x"},
		{"X-Api-Key: headersecretvalue", "headersecretvalue", "X-Api-Key"},
		{"password: hunter2hunter2", "hunter2hunter2", "password"},
	}
	for _, c := range cases {
		got := r.Redact(c.in)
		if strings.Contains(got, c.gone) || !strings.Contains(got, c.keep) || !strings.Contains(got, "[REDACTED]") {
			t.Errorf("Redact(%q) = %q", c.in, got)
		}
	}
	// Ordinary text, including look-alike keys, is left alone.
	for _, s := range []string{
		"prompt_tokens=1234 completion_tokens=99 exit_code=1",
		`{"code":"INVALID_ARGUMENT","max_tokens":3000,"abc":"abc"}`,
		"stage=transcribe progress=0.42 path=/api/v1/jobs",
	} {
		if got := r.Redact(s); got != s {
			t.Errorf("Redact changed %q → %q", s, got)
		}
	}
}

func TestLoggerWritesRedactedJSONLines(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	lg, err := engine.NewLogger(dir, engine.LoggerOptions{Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	lg.AddSecret("registered-secret-value")
	lg.Info("llm call", "url", "https://api.example/v1?api_key=urlsecret12345",
		"authorization", "Bearer headersecret12345", "token", "tok",
		"error", errors.New(`upstream said {"api_key":"errsecret12345"}`),
		"note", "contains registered-secret-value inline", "prompt_tokens", 12)
	lg.With("api_key", "withsecret12345").WithGroup("g").Info("grouped", "password", "grouppassword1")
	lg.Debug("below the default level")
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(engine.LogPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(engine.LogPath(dir)); st.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v", st.Mode().Perm())
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSON lines, got %d:\n%s", len(lines), data)
	}
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not a JSON line after redaction: %v\n%s", err, line)
		}
		if rec["time"] == nil || rec["level"] != "INFO" || rec["msg"] == nil {
			t.Fatalf("record = %v", rec)
		}
	}
	for _, out := range []string{string(data), stderr.String()} {
		for _, secret := range []string{"registered-secret-value", "urlsecret12345", "headersecret12345",
			"errsecret12345", "withsecret12345", "grouppassword1", `"token":"tok"`, "token=tok"} {
			if strings.Contains(out, secret) {
				t.Errorf("output leaks %q:\n%s", secret, out)
			}
		}
		if !strings.Contains(out, "llm call") || !strings.Contains(out, "prompt_tokens") || strings.Contains(out, "below the default level") {
			t.Errorf("unexpected output:\n%s", out)
		}
	}
	if !strings.Contains(stderr.String(), "level=INFO") {
		t.Errorf("stderr is not human-readable text: %q", stderr.String())
	}
}

func TestLogRotationKeepsFiveFiles(t *testing.T) {
	dir := t.TempDir()
	lg, err := engine.NewLogger(dir, engine.LoggerOptions{MaxBytes: 2048, Level: slog.LevelDebug})
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 200)
	for i := 0; i < 400; i++ {
		lg.Debug("line", "i", i, "pad", pad)
	}
	lg.Close()
	path := engine.LogPath(dir)
	var newest, oldest string
	for i := 0; i <= 4; i++ {
		name := path
		if i > 0 {
			name = fmt.Sprintf("%s.%d", path, i)
		}
		st, err := os.Stat(name)
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		if st.Size() > 2048 || st.Size() == 0 {
			t.Errorf("%s has %d bytes", name, st.Size())
		}
		b, _ := os.ReadFile(name)
		if i == 0 {
			newest = string(b)
		}
		oldest = string(b)
	}
	if _, err := os.Stat(path + ".5"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("more than 5 log files kept")
	}
	if !strings.Contains(newest, `"i":399`) || strings.Contains(oldest, `"i":399`) || strings.Contains(newest, `"i":0,`) {
		t.Fatal("rotation order is wrong: newest records must be in engine.log")
	}

	// Reopening appends rather than truncates.
	lg2, err := engine.NewLogger(dir, engine.LoggerOptions{MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	lg2.Info("after reopen")
	lg2.Close()
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"i":399`) || !strings.Contains(string(b), "after reopen") {
		t.Fatal("reopen truncated the log")
	}
}
