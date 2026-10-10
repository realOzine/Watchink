// Package api is the HTTP transport of the engine. It maps every route 1:1 onto an app.Local and holds no business logic:
// argument parsing, authentication, CORS, the event stream framing and the
// embedded SPA are the only things implemented here.
package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"watchink/internal/app"
)

// DefaultAllowedOrigins are the WebView origins allowed by CORS: the
// Wails v2 origin on macOS, and the one used on Windows (WebView2).
var DefaultAllowedOrigins = []string{"wails://wails", "http://wails.localhost"}

// DefaultHeartbeat is the event-stream heartbeat interval.
const DefaultHeartbeat = 15 * time.Second

// Response headers added by this package (all listed in
// Access-Control-Expose-Headers).
const (
	// HeaderStreamReset is "1" on an event-stream response whose subscriber
	// must do a full refresh; the same fact is sent as a leading `reset` event.
	HeaderStreamReset = "X-WK-Stream-Reset"
	// HeaderExport carries the base64url-encoded app.Export JSON of a download.
	HeaderExport = "X-WK-Export"
	// HeaderTruncated / HeaderNextStartMs describe a text-format transcript.
	HeaderTruncated   = "X-WK-Truncated"
	HeaderNextStartMs = "X-WK-Next-Start-Ms"
	// HeaderRequest is the custom header required on cookie-authenticated
	// state-changing requests.
	HeaderRequest = "X-WK-Request"
	// HeaderFilename carries the percent-encoded upload file name.
	HeaderFilename = "X-Filename"
)

// EventReset is the event-stream event sent first when the subscriber's
// Last-Event-ID can no longer be resumed.
const EventReset = "reset"

// Options configures NewHandler.
type Options struct {
	// Service is the engine-side service every route maps onto.
	Service app.Local
	// Token is the engine bearer token. An empty token disables bearer auth
	// (every authenticated route then answers 401).
	Token string
	// Port is the port the engine listens on; the Host header must be
	// 127.0.0.1:<Port> or localhost:<Port>. Zero accepts any port on those two
	// host names (tests behind httptest).
	Port int
	// WebUI is the built SPA rooted at index.html; nil or an FS without
	// index.html serves a "not built" page instead.
	WebUI fs.FS
	// Logger receives request logs; nil discards them.
	Logger *slog.Logger
	// OnShutdown is called for POST /api/v1/engine/shutdown after the response
	// has been written. It must not block.
	OnShutdown func(force bool)
	// AllowedOrigins lists the cross-origin WebView origins allowed by CORS;
	// nil means DefaultAllowedOrigins.
	AllowedOrigins []string
	// EngineInfo overlays the process-level fields (pid, base_url, mode, …)
	// on the service's EngineInfo before it is returned.
	EngineInfo func(info *app.EngineInfo)
	// HeartbeatInterval is the event-stream heartbeat; zero means 15 s.
	HeartbeatInterval time.Duration
	// Now is the clock used for login codes; nil means time.Now.
	Now func() time.Time
}

// Stats is the activity snapshot the engine uses for idle exit.
type Stats struct {
	// Subscribers is the number of live event-stream connections.
	Subscribers int
	// InFlight is the number of authenticated requests being served.
	InFlight int
	// LastRequest is when an authenticated request last started or finished;
	// zero when there has been none.
	LastRequest time.Time
}

// Handler is the engine's http.Handler.
type Handler struct {
	opts    Options
	svc     app.Local
	log     *slog.Logger
	mux     *http.ServeMux
	origins map[string]bool
	now     func() time.Time

	hasToken bool
	tokenSum [sha256.Size]byte

	ui        fs.FS
	index     []byte
	indexETag string

	subs     atomic.Int64
	inflight atomic.Int64
	last     atomic.Int64 // unix nanoseconds; 0 = never

	baseCtx   context.Context
	closeBase context.CancelFunc
	closeOnce sync.Once
}

// NewHandler builds the engine HTTP handler. The returned *Handler also
// exposes Stats (idle exit) and Close (ends event streams and long polls).
func NewHandler(opts Options) *Handler {
	h := &Handler{
		opts:    opts,
		svc:     opts.Service,
		log:     opts.Logger,
		origins: map[string]bool{},
		now:     opts.Now,
	}
	if h.log == nil {
		h.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.opts.HeartbeatInterval <= 0 {
		h.opts.HeartbeatInterval = DefaultHeartbeat
	}
	origins := opts.AllowedOrigins
	if origins == nil {
		origins = DefaultAllowedOrigins
	}
	for _, o := range origins {
		h.origins[o] = true
	}
	if opts.Token != "" {
		h.hasToken = true
		h.tokenSum = sha256.Sum256([]byte(opts.Token))
	}
	h.baseCtx, h.closeBase = context.WithCancel(context.Background())
	h.loadUI(opts.WebUI)
	h.routes()
	return h
}

// Stats returns the current activity snapshot.
func (h *Handler) Stats() Stats {
	s := Stats{Subscribers: int(h.subs.Load()), InFlight: int(h.inflight.Load())}
	if n := h.last.Load(); n != 0 {
		s.LastRequest = time.Unix(0, n)
	}
	return s
}

// Close ends every event stream (after flushing the events already queued)
// and wakes pending long polls, so that http.Server.Shutdown can finish.
func (h *Handler) Close() {
	h.closeOnce.Do(h.closeBase)
}

func (h *Handler) touch() { h.last.Store(time.Now().UnixNano()) }

func (h *Handler) routes() {
	mux := http.NewServeMux()
	p := app.APIPrefix

	mux.HandleFunc("GET "+p+"/health", h.health)
	mux.HandleFunc("GET "+p+"/engine", h.authed(h.engineInfo))
	mux.HandleFunc("POST "+p+"/engine/shutdown", h.authed(h.engineShutdown))

	mux.HandleFunc("POST "+p+"/imports", h.authed(h.createImport))
	mux.HandleFunc("POST "+p+"/uploads", h.authed(h.createUpload))

	mux.HandleFunc("GET "+p+"/jobs", h.authed(h.listJobs))
	mux.HandleFunc("GET "+p+"/jobs/{job_id}", h.authed(h.getJob))
	mux.HandleFunc("POST "+p+"/jobs/{job_id}/cancel", h.authed(h.cancelJob))
	mux.HandleFunc("POST "+p+"/jobs/{job_id}/retry", h.authed(h.retryJob))

	mux.HandleFunc("GET "+p+"/videos", h.authed(h.listVideos))
	mux.HandleFunc("GET "+p+"/videos/{video_id}", h.authed(h.getVideo))
	mux.HandleFunc("GET "+p+"/videos/{video_id}/note", h.authed(h.getNote))
	mux.HandleFunc("GET "+p+"/videos/{video_id}/transcript", h.authed(h.getTranscript))
	mux.HandleFunc("GET "+p+"/videos/{video_id}/playback", h.authed(h.getPlayback))
	mux.HandleFunc("POST "+p+"/videos/{video_id}/exports", h.authed(h.createExport))
	mux.HandleFunc("GET "+p+"/exports/{export_id}/download", h.authed(h.downloadExport))

	mux.HandleFunc("GET "+p+"/events", h.authed(h.events))

	mux.HandleFunc("GET "+p+"/settings", h.authed(h.getSettings))
	mux.HandleFunc("PATCH "+p+"/settings", h.authed(h.patchSettings))
	mux.HandleFunc("PUT "+p+"/settings/secrets/{name}", h.authed(h.setSecret))
	mux.HandleFunc("POST "+p+"/settings/llm/test", h.authed(h.testLLM))
	mux.HandleFunc("POST "+p+"/settings/llm/models", h.authed(h.listLLMModels))

	mux.HandleFunc("GET "+p+"/tools", h.authed(h.getTools))
	mux.HandleFunc("POST "+p+"/tools/prepare", h.authed(h.prepareTools))
	mux.HandleFunc("POST "+p+"/tools/update", h.authed(h.updateTool))
	mux.HandleFunc("POST "+p+"/tools/import", h.authed(h.importTool))

	mux.HandleFunc("GET /media/{video_id}/playback", h.media)

	// Method-less catch-alls keep unknown API routes (and wrong methods) inside
	// the JSON error envelope instead of ServeMux's plain-text replies.
	notFound := func(w http.ResponseWriter, r *http.Request) {
		writeError(w, app.E(app.CodeNotFound, "接口不存在：%s %s", r.Method, r.URL.Path))
	}
	mux.HandleFunc("/api/", notFound)
	mux.HandleFunc("/media/", notFound)
	mux.HandleFunc("/", h.spa)
	h.mux = mux
}

// ServeHTTP applies, in order: panic recovery and request logging, the common
// security headers, the Host check, CORS, then routing. Authentication
// is per route because /health, /media and the SPA shell are
// reachable without a bearer token or cookie.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	lw := &statusWriter{ResponseWriter: w}
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			h.log.Error("http panic", "method", r.Method, "path", r.URL.Path, "panic", fmt.Sprint(v))
			if !lw.wrote {
				writeError(lw, app.E(app.CodeInternal, "内部错误"))
			}
		}
		h.logRequest(r, lw, time.Since(start))
	}()

	hdr := lw.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	// Login codes and media tokens travel in URLs; never leak them by Referer.
	hdr.Set("Referrer-Policy", "no-referrer")

	if !h.hostAllowed(r.Host) {
		writeError(lw, app.E(app.CodeUnauthorized, "Host 头不被允许").WithDetail("reason", "host"))
		return
	}
	if h.cors(lw, r) {
		return
	}
	h.mux.ServeHTTP(lw, r)
}

// statusWriter records the status and size for the request log. Unwrap lets
// http.ResponseController reach Flush on the underlying writer.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
	auth   string
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// ReadFrom keeps the sendfile fast path of the underlying writer for media.
func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		w.bytes += n
		return n, err
	}
	n, err := io.Copy(struct{ io.Writer }{w.ResponseWriter}, r)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// redactedQueryKeys are the query parameters whose values are secrets.
var redactedQueryKeys = map[string]bool{"token": true, "code": true, "mt": true, "access_token": true}

// RedactQuery renders a raw query string with secret values masked.
func RedactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		key, _, hasValue := strings.Cut(part, "=")
		if hasValue && redactedQueryKeys[strings.ToLower(key)] {
			parts[i] = key + "=[REDACTED]"
		}
	}
	return strings.Join(parts, "&")
}

func (h *Handler) logRequest(r *http.Request, w *statusWriter, d time.Duration) {
	level := slog.LevelDebug
	switch {
	case w.status >= 500:
		level = slog.LevelError
	case w.status >= 400:
		level = slog.LevelWarn
	case r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions:
		level = slog.LevelInfo
	}
	if !h.log.Enabled(r.Context(), level) {
		return
	}
	attrs := []any{
		"method", r.Method, "path", r.URL.Path, "status", w.status,
		"bytes", w.bytes, "duration_ms", d.Milliseconds(),
	}
	if q := RedactQuery(r.URL.RawQuery); q != "" {
		attrs = append(attrs, "query", q)
	}
	if w.auth != "" {
		attrs = append(attrs, "auth", w.auth)
	}
	h.log.Log(r.Context(), level, "http request", attrs...)
}
