package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Log rotation defaults: rotate at 10 MiB, keep 5 files in total
// (engine.log plus engine.log.1 … engine.log.4).
const (
	DefaultLogMaxBytes = 10 << 20
	DefaultLogKeep     = 5
)

const redacted = "[REDACTED]"

// Redactor masks secrets in log output: registered values (the engine
// token, API keys) and common credential shapes.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

// Add registers a literal value to mask. Very short values are ignored: they
// would mask unrelated text.
func (r *Redactor) Add(secret string) {
	secret = strings.TrimSpace(secret)
	if len(secret) < 6 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.secrets {
		if s == secret {
			return
		}
	}
	r.secrets = append(r.secrets, secret)
}

const sensitiveNames = `authorization|proxy-authorization|x-api-key|api[_-]?key|access[_-]?token|refresh[_-]?token|engine[_-]?token|media[_-]?token|login[_-]?code|token|secret|password|passwd|cookie|set-cookie`

var redactPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Authorization: Bearer xxx / Basic xxx, wherever it appears.
	{regexp.MustCompile(`(?i)\b((?:bearer|basic)\s+)[A-Za-z0-9._~+/=-]{8,}`), "${1}" + redacted},
	// OpenAI-style API keys.
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`), redacted},
	// URL query parameters: ?token=…, &code=…, &mt=…, &key=….
	{regexp.MustCompile(`(?i)([?&](?:token|code|mt|key|access_token|api[_-]?key)=)[^&\s"'\\]+`), "${1}" + redacted},
	// JSON members, plain or escaped inside another JSON string.
	{regexp.MustCompile(`(?i)(\\?"(?:` + sensitiveNames + `)\\?"\s*:\s*\\?")[^"\\]+`), "${1}" + redacted},
	// key=value and header forms: api_key=…, Authorization: ….
	{regexp.MustCompile(`(?i)(\b(?:` + sensitiveNames + `)\s*[=:]\s*)[^\s"'&,;}\\\[]+`), "${1}" + redacted},
}

// Redact returns s with every secret masked.
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	for _, secret := range r.secrets {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, redacted)
		}
	}
	r.mu.RUnlock()
	for _, p := range redactPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// sensitiveKeys are attribute keys whose value is masked whatever it holds.
var sensitiveKeys = map[string]bool{
	"authorization": true, "token": true, "engine_token": true, "media_token": true,
	"mt": true, "api_key": true, "apikey": true, "password": true, "secret": true,
	"login_code": true, "cookie": true, "set-cookie": true, "access_token": true,
}

func replaceSensitiveAttr(_ []string, a slog.Attr) slog.Attr {
	if sensitiveKeys[strings.ToLower(a.Key)] && a.Value.Kind() != slog.KindGroup {
		a.Value = slog.StringValue(redacted)
	}
	return a
}

// redactWriter masks every formatted record on its way out. slog handlers
// issue one Write per record, so a secret cannot straddle two writes.
type redactWriter struct {
	w io.Writer
	r *Redactor
}

func (w redactWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.w, w.r.Redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// rotatingFile is an append-only log file rotated by size.
type rotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

func openRotating(path string, maxBytes int64, keep int) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	r := &rotatingFile{path: path, maxBytes: maxBytes, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate shifts engine.log → .1 → … and drops the oldest, keeping `keep`
// files including the live one.
func (r *rotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	r.f = nil
	backups := r.keep - 1
	if backups < 1 {
		if err := os.Remove(r.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return r.open()
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", r.path, backups))
	for i := backups - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil && !os.IsNotExist(err) {
		// Windows cannot rename a file that another program (a log viewer)
		// has open. Keep appending to the oversized file rather than lose
		// the log; the next write tries to rotate again.
		if r.open() == nil {
			return nil
		}
		return err
	}
	return r.open()
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// fanout sends each record to several handlers (file + stderr).
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, rec slog.Record) error {
	var first error
	for _, h := range f {
		if !h.Enabled(ctx, rec.Level) {
			continue
		}
		if err := h.Handle(ctx, rec.Clone()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// LoggerOptions tunes NewLogger; the zero value gives the defaults.
type LoggerOptions struct {
	// Stderr, when set, also receives human-readable lines (foreground mode).
	Stderr io.Writer
	// Level is the minimum level; nil means Info.
	Level slog.Leveler
	// MaxBytes and Keep override the rotation size and file count.
	MaxBytes int64
	Keep     int
}

// Logger is the engine log: JSON lines in <data>/logs/engine.log, rotated by
// size, with every record passed through a Redactor.
type Logger struct {
	*slog.Logger
	redactor *Redactor
	file     *rotatingFile
}

// NewLogger opens (creating directories as needed) the engine log of dataDir.
func NewLogger(dataDir string, opts LoggerOptions) (*Logger, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultLogMaxBytes
	}
	if opts.Keep <= 0 {
		opts.Keep = DefaultLogKeep
	}
	if opts.Level == nil {
		opts.Level = slog.LevelInfo
	}
	file, err := openRotating(LogPath(dataDir), opts.MaxBytes, opts.Keep)
	if err != nil {
		return nil, fmt.Errorf("engine: open log: %w", err)
	}
	red := &Redactor{}
	ho := &slog.HandlerOptions{Level: opts.Level, ReplaceAttr: replaceSensitiveAttr}
	var handler slog.Handler = slog.NewJSONHandler(redactWriter{file, red}, ho)
	if opts.Stderr != nil {
		handler = fanout{handler, slog.NewTextHandler(redactWriter{opts.Stderr, red}, ho)}
	}
	return &Logger{Logger: slog.New(handler), redactor: red, file: file}, nil
}

// AddSecret registers a value that must never appear in the log.
func (l *Logger) AddSecret(secret string) { l.redactor.Add(secret) }

// Close closes the log file.
func (l *Logger) Close() error { return l.file.Close() }
