package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"watchink/internal/app"
	"watchink/internal/transport/api"
	"watchink/internal/transport/api/apitest"
)

const testToken = "test-token-0123456789abcdef"

type env struct {
	t      *testing.T
	fake   *apitest.Fake
	h      *api.Handler
	srv    *httptest.Server
	now    time.Time
	nowMu  sync.Mutex
	logs   *bytes.Buffer
	logsMu sync.Mutex
	stops  chan bool
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func newEnv(t *testing.T, mutate func(o *api.Options)) *env {
	t.Helper()
	e := &env{t: t, fake: apitest.New(), now: time.Now(), logs: &bytes.Buffer{}, stops: make(chan bool, 4)}
	opts := api.Options{
		Service: e.fake,
		Token:   testToken,
		Logger: slog.New(slog.NewTextHandler(lockedWriter{&e.logsMu, e.logs},
			&slog.HandlerOptions{Level: slog.LevelDebug})),
		OnShutdown:        func(force bool) { e.stops <- force },
		HeartbeatInterval: 20 * time.Millisecond,
		Now: func() time.Time {
			e.nowMu.Lock()
			defer e.nowMu.Unlock()
			return e.now
		},
	}
	if mutate != nil {
		mutate(&opts)
	}
	e.h = api.NewHandler(opts)
	e.srv = httptest.NewServer(e.h)
	t.Cleanup(func() {
		e.h.Close()
		e.srv.CloseClientConnections()
		e.srv.Close()
	})
	return e
}

func (e *env) logText() string {
	e.logsMu.Lock()
	defer e.logsMu.Unlock()
	return e.logs.String()
}

// req builds a request; headers are "Key: value" strings. "Host: x" sets
// the Host header.
func (e *env) req(method, path string, body string, headers ...string) *http.Request {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, h := range headers {
		k, v, _ := strings.Cut(h, ": ")
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	return r
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

func (e *env) do(r *http.Request) (*http.Response, string) {
	e.t.Helper()
	resp, err := noRedirect.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp, string(b)
}

func bearer() string { return "Authorization: Bearer " + testToken }

// wantError asserts the {"error":{…}} envelope with the contract status.
func wantError(t *testing.T, resp *http.Response, body string, code app.Code) *app.Error {
	t.Helper()
	if resp.StatusCode != app.HTTPStatus(code) {
		t.Fatalf("status = %d, want %d (%s); body %s", resp.StatusCode, app.HTTPStatus(code), code, body)
	}
	var envl app.ErrorEnvelope
	if err := json.Unmarshal([]byte(body), &envl); err != nil || envl.Error == nil {
		t.Fatalf("body is not an error envelope: %q", body)
	}
	if envl.Error.Code != code {
		t.Fatalf("code = %s, want %s", envl.Error.Code, code)
	}
	return envl.Error
}

func TestHealthIsOpenAndMinimal(t *testing.T) {
	e := newEnv(t, nil)
	resp, body := e.do(e.req("GET", "/api/v1/health", ""))
	if resp.StatusCode != 200 || strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Fatalf("health: %d %q", resp.StatusCode, body)
	}
}

func TestHostValidation(t *testing.T) {
	e := newEnv(t, nil)
	port := e.srv.Listener.Addr().String()[strings.LastIndex(e.srv.Listener.Addr().String(), ":"):]
	cases := []struct {
		host string
		ok   bool
	}{
		{"127.0.0.1" + port, true},
		{"localhost" + port, true},
		{"evil.example.com" + port, false},
		{"evil.example.com", false},
		{"127.0.0.1", false},
		{"127.0.0.1.evil.example" + port, false},
		{"[::1]" + port, false},
	}
	for _, c := range cases {
		resp, body := e.do(e.req("GET", "/api/v1/health", "", "Host: "+c.host))
		if c.ok && resp.StatusCode != 200 {
			t.Errorf("host %q rejected: %d", c.host, resp.StatusCode)
		}
		if !c.ok {
			err := wantError(t, resp, body, app.CodeUnauthorized)
			if err.Details["reason"] != "host" {
				t.Errorf("host %q: reason = %v", c.host, err.Details["reason"])
			}
		}
	}

	// With a fixed port, another port on the same host name is refused too.
	fixed := newEnv(t, func(o *api.Options) { o.Port = 47823 })
	resp, body := fixed.do(fixed.req("GET", "/api/v1/health", ""))
	wantError(t, resp, body, app.CodeUnauthorized)
	resp, _ = fixed.do(fixed.req("GET", "/api/v1/health", "", "Host: localhost:47823"))
	if resp.StatusCode != 200 {
		t.Fatalf("fixed port: %d", resp.StatusCode)
	}
	// The SPA and media routes are behind the same check.
	resp, _ = fixed.do(fixed.req("GET", "/", "", "Host: attacker.test:47823"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("spa with bad host: %d", resp.StatusCode)
	}
}

func TestBearerAuth(t *testing.T) {
	e := newEnv(t, nil)
	cases := []struct {
		name   string
		header string
		ok     bool
	}{
		{"none", "", false},
		{"good", bearer(), true},
		{"lowercase scheme", "Authorization: bearer " + testToken, true},
		{"wrong", "Authorization: Bearer nope", false},
		{"prefix of token", "Authorization: Bearer " + testToken[:10], false},
		{"token plus suffix", "Authorization: Bearer " + testToken + "x", false},
		{"basic", "Authorization: Basic " + testToken, false},
		{"bare token", "Authorization: " + testToken, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hs []string
			if c.header != "" {
				hs = append(hs, c.header)
			}
			resp, body := e.do(e.req("GET", "/api/v1/engine", "", hs...))
			if c.ok {
				if resp.StatusCode != 200 {
					t.Fatalf("status %d: %s", resp.StatusCode, body)
				}
				return
			}
			wantError(t, resp, body, app.CodeUnauthorized)
		})
	}

	// An empty configured token never authenticates anything.
	open := newEnv(t, func(o *api.Options) { o.Token = "" })
	resp, body := open.do(open.req("GET", "/api/v1/engine", "", "Authorization: Bearer "))
	wantError(t, resp, body, app.CodeUnauthorized)
}

func TestEveryAPIRouteRequiresAuth(t *testing.T) {
	e := newEnv(t, nil)
	routes := []string{
		"GET /api/v1/engine", "POST /api/v1/engine/shutdown", "POST /api/v1/imports",
		"POST /api/v1/uploads", "GET /api/v1/jobs", "GET /api/v1/jobs/j", "POST /api/v1/jobs/j/cancel",
		"POST /api/v1/jobs/j/retry", "GET /api/v1/videos", "GET /api/v1/videos/v",
		"GET /api/v1/videos/v/note", "GET /api/v1/videos/v/transcript", "GET /api/v1/videos/v/playback",
		"POST /api/v1/videos/v/exports", "GET /api/v1/exports/x/download", "GET /api/v1/events",
		"GET /api/v1/settings", "PATCH /api/v1/settings", "PUT /api/v1/settings/secrets/llm_api_key",
		"POST /api/v1/settings/llm/test", "GET /api/v1/tools", "POST /api/v1/tools/prepare",
		"POST /api/v1/tools/update", "POST /api/v1/tools/import", "POST /api/v1/settings/llm/models",
	}
	for _, rt := range routes {
		method, path, _ := strings.Cut(rt, " ")
		resp, body := e.do(e.req(method, path, ""))
		wantError(t, resp, body, app.CodeUnauthorized)
	}
	if n := len(e.fake.Calls()); n != 0 {
		t.Fatalf("service reached without auth: %v", e.fake.Calls())
	}
	select {
	case <-e.stops:
		t.Fatal("shutdown reached without auth")
	default:
	}
}

// The Web UI has no login: while web.enabled is on, a same-origin browser
// request is accepted without a token; while it is off, only bearer clients
// get in.
func TestWebUIAccess(t *testing.T) {
	e := newEnv(t, nil)
	origin := "Origin: " + e.srv.URL
	patch := `{"llm":{"model":"m1"}}`

	// Off (the default): a tokenless request is refused with a reason the
	// frontend can show.
	resp, body := e.do(e.req("GET", "/api/v1/engine", ""))
	if err := wantError(t, resp, body, app.CodeUnauthorized); err.Details["reason"] != "web_disabled" || err.Hint == "" {
		t.Fatalf("disabled: %+v", err)
	}
	resp, body = e.do(e.req("PATCH", "/api/v1/settings", patch, origin, "X-WK-Request: 1"))
	wantError(t, resp, body, app.CodeUnauthorized)

	e.fake.WebOn = true
	// Reads work for a same-origin (or address-bar) request…
	for _, headers := range [][]string{nil, {"Sec-Fetch-Site: same-origin"}, {"Sec-Fetch-Site: none"}, {origin}} {
		resp, body = e.do(e.req("GET", "/api/v1/engine", "", headers...))
		if resp.StatusCode != 200 {
			t.Fatalf("web GET %v: %d %s", headers, resp.StatusCode, body)
		}
	}
	// …but not for one another website caused.
	for _, headers := range [][]string{{"Sec-Fetch-Site: cross-site"}, {"Sec-Fetch-Site: same-site"}, {"Origin: http://evil.example"}, {"Origin: wails://wails"}} {
		resp, body = e.do(e.req("GET", "/api/v1/engine", "", headers...))
		wantError(t, resp, body, app.CodeUnauthorized)
	}

	// State changes need our own Origin and X-WK-Request.
	cases := []struct {
		name    string
		headers []string
		reason  string
	}{
		{"bare", nil, "origin"},
		{"no custom header", []string{origin}, "csrf_header"},
		{"no origin", []string{"X-WK-Request: 1"}, "origin"},
		{"foreign origin", []string{"Origin: http://evil.example", "X-WK-Request: 1"}, "credentials"},
		{"same host other port", []string{"Origin: http://127.0.0.1:1", "X-WK-Request: 1"}, "credentials"},
		{"wrong header value", []string{origin, "X-WK-Request: true"}, "csrf_header"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := e.do(e.req("PATCH", "/api/v1/settings", patch, c.headers...))
			err := wantError(t, resp, body, app.CodeUnauthorized)
			if err.Details["reason"] != c.reason {
				t.Fatalf("reason = %v, want %s", err.Details["reason"], c.reason)
			}
		})
	}
	if e.fake.CallCount("PatchSettings") != 0 {
		t.Fatal("PatchSettings reached despite failed CSRF checks")
	}
	resp, body = e.do(e.req("PATCH", "/api/v1/settings", patch, origin, "X-WK-Request: 1"))
	if resp.StatusCode != 200 || e.fake.CallCount("PatchSettings") != 1 {
		t.Fatalf("valid web PATCH: %d %s", resp.StatusCode, body)
	}
	// Bearer needs neither, and a wrong bearer never falls back to the
	// browser rule.
	resp, _ = e.do(e.req("PATCH", "/api/v1/settings", patch, bearer()))
	if resp.StatusCode != 200 {
		t.Fatalf("bearer PATCH: %d", resp.StatusCode)
	}
	resp, body = e.do(e.req("GET", "/api/v1/engine", "", "Authorization: Bearer wrong"))
	wantError(t, resp, body, app.CodeUnauthorized)
}

func TestCORS(t *testing.T) {
	e := newEnv(t, nil)
	for _, origin := range []string{"wails://wails", "http://wails.localhost"} {
		// Preflight, including the private-network one.
		resp, _ := e.do(e.req("OPTIONS", "/api/v1/imports", "",
			"Origin: "+origin, "Access-Control-Request-Method: POST",
			"Access-Control-Request-Headers: authorization, content-type",
			"Access-Control-Request-Private-Network: true"))
		h := resp.Header
		if resp.StatusCode != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != origin ||
			h.Get("Access-Control-Allow-Private-Network") != "true" {
			t.Fatalf("preflight %s: %d %v", origin, resp.StatusCode, h)
		}
		for _, want := range []string{"Authorization", "Content-Type", "X-WK-Request", "X-Filename", "Last-Event-ID"} {
			if !strings.Contains(h.Get("Access-Control-Allow-Headers"), want) {
				t.Errorf("allow-headers misses %s", want)
			}
		}
		for _, m := range []string{"GET", "POST", "PUT", "PATCH"} {
			if !strings.Contains(h.Get("Access-Control-Allow-Methods"), m) {
				t.Errorf("allow-methods misses %s", m)
			}
		}
		if h.Get("Access-Control-Allow-Credentials") != "" {
			t.Error("credentials must not be allowed cross-origin")
		}
		// Actual request.
		resp, _ = e.do(e.req("GET", "/api/v1/engine", "", "Origin: "+origin, bearer()))
		if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != origin ||
			!strings.Contains(resp.Header.Get("Access-Control-Expose-Headers"), "Content-Disposition") {
			t.Fatalf("actual %s: %d %v", origin, resp.StatusCode, resp.Header)
		}
		// Errors carry the CORS headers too, or the frontend cannot read them.
		resp, _ = e.do(e.req("GET", "/api/v1/engine", "", "Origin: "+origin))
		if resp.StatusCode != 401 || resp.Header.Get("Access-Control-Allow-Origin") != origin {
			t.Fatalf("401 without CORS headers: %v", resp.Header)
		}
	}
	// A preflight without the private-network request header gets no PNA reply.
	resp, _ := e.do(e.req("OPTIONS", "/api/v1/imports", "", "Origin: wails://wails", "Access-Control-Request-Method: POST"))
	if resp.Header.Get("Access-Control-Allow-Private-Network") != "" {
		t.Fatal("unexpected private-network header")
	}

	// Other origins get nothing.
	for _, origin := range []string{"http://evil.example", "https://wails", "null", "http://localhost:5173"} {
		resp, body := e.do(e.req("OPTIONS", "/api/v1/imports", "", "Origin: "+origin, "Access-Control-Request-Method: POST"))
		wantError(t, resp, body, app.CodeUnauthorized)
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("preflight allowed for %s", origin)
		}
		resp, _ = e.do(e.req("GET", "/api/v1/engine", "", "Origin: "+origin, bearer()))
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("ACAO sent to %s", origin)
		}
	}

	// The list is an option.
	custom := newEnv(t, func(o *api.Options) { o.AllowedOrigins = []string{"http://localhost:5173"} })
	resp, _ = custom.do(custom.req("GET", "/api/v1/health", "", "Origin: http://localhost:5173"))
	if resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatal("custom origin not allowed")
	}
	resp, _ = custom.do(custom.req("GET", "/api/v1/health", "", "Origin: wails://wails"))
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("default origin still allowed with a custom list")
	}
}

func TestMediaIsAuthorisedByTokenOnly(t *testing.T) {
	e := newEnv(t, nil)
	e.fake.PlaybackData["vid_1"] = []byte("0123456789")
	// No bearer, no cookie: the mt token is enough.
	resp, body := e.do(e.req("GET", "/media/vid_1/playback?mt="+e.fake.MediaToken, ""))
	if resp.StatusCode != 200 || body != "0123456789" || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("media: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	resp, body = e.do(e.req("GET", "/media/vid_1/playback?mt=wrong", ""))
	wantError(t, resp, body, app.CodeUnauthorized)
	resp, body = e.do(e.req("GET", "/media/vid_1/playback", "", bearer()))
	wantError(t, resp, body, app.CodeUnauthorized)
	resp, body = e.do(e.req("GET", "/media/vid_404/playback?mt="+e.fake.MediaToken, ""))
	wantError(t, resp, body, app.CodeNotFound)
}

func TestSPA(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":          {Data: []byte("<!doctype html><title>LT</title>")},
		"assets/app-abc12.js": {Data: []byte("console.log(1)")},
		"favicon.svg":         {Data: []byte("<svg/>")},
	}
	e := newEnv(t, func(o *api.Options) { o.WebUI = ui })

	// The Web UI is optional and off by default: nothing is served.
	resp, body := e.do(e.req("GET", "/", ""))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "没有开启") || strings.Contains(body, "<title>") {
		t.Fatalf("disabled web UI: %d %q", resp.StatusCode, body)
	}
	e.fake.WebOn = true

	for _, p := range []string{"/", "/index.html", "/videos/vid_01JABC", "/settings", "/videos/vid_1/"} {
		resp, body := e.do(e.req("GET", p, ""))
		if resp.StatusCode != 200 || !strings.Contains(body, "<title>LT</title>") ||
			!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
			resp.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %d %q %v", p, resp.StatusCode, body, resp.Header)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "media-src 'self'", "connect-src 'self'", "object-src 'none'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Fatalf("%s: CSP misses %q: %s", p, want, csp)
			}
		}
		if strings.Contains(strings.Split(csp, "style-src")[0], "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Fatalf("CSP allows inline/eval scripts: %s", csp)
		}
	}

	// Conditional request on the shell.
	resp, _ = e.do(e.req("GET", "/", ""))
	etag := resp.Header.Get("ETag")
	resp, _ = e.do(e.req("GET", "/", "", "If-None-Match: "+etag))
	if etag == "" || resp.StatusCode != http.StatusNotModified {
		t.Fatalf("etag %q → %d", etag, resp.StatusCode)
	}

	resp, body = e.do(e.req("GET", "/assets/app-abc12.js", ""))
	if resp.StatusCode != 200 || body != "console.log(1)" ||
		resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		!strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("asset: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = e.do(e.req("GET", "/favicon.svg", ""))
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("root file: %d %v", resp.StatusCode, resp.Header)
	}
	// Missing assets and file-like paths are 404, not the shell.
	for _, p := range []string{"/assets/missing.js", "/robots.txt", "/assets/"} {
		resp, body := e.do(e.req("GET", p, ""))
		if resp.StatusCode != 404 || strings.Contains(body, "<title>") {
			t.Fatalf("%s: %d %q", p, resp.StatusCode, body)
		}
	}
	// Unknown API / media paths never fall back to the shell.
	for _, p := range []string{"/api/v1/nope", "/api/v2/engine", "/media/x"} {
		resp, body := e.do(e.req("GET", p, "", bearer()))
		wantError(t, resp, body, app.CodeNotFound)
	}
	// Wrong method on a known route stays inside the envelope.
	resp, body = e.do(e.req("DELETE", "/api/v1/jobs", "", bearer()))
	wantError(t, resp, body, app.CodeNotFound)
	resp, body = e.do(e.req("POST", "/videos/vid_1", "", bearer()))
	wantError(t, resp, body, app.CodeNotFound)

	// No built UI.
	for _, mut := range []func(o *api.Options){nil, func(o *api.Options) { o.WebUI = fstest.MapFS{".gitkeep": {}} }} {
		bare := newEnv(t, mut)
		bare.fake.WebOn = true
		resp, body := bare.do(bare.req("GET", "/videos/vid_1", ""))
		if resp.StatusCode != 404 || !strings.Contains(body, "尚未构建") ||
			!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
			t.Fatalf("not-built page: %d %q", resp.StatusCode, body)
		}
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	e := newEnv(t, nil)
	for _, p := range []string{"/api/v1/health", "/", "/api/v1/engine"} {
		resp, _ := e.do(e.req("GET", p, ""))
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: %v", p, resp.Header)
		}
	}
}

func TestBadBodiesAndParams(t *testing.T) {
	e := newEnv(t, nil)
	v := e.fake.AddVideo(app.Video{VideoID: "vid_1", Title: "t"})
	e.fake.Transcripts[v.VideoID] = &app.Transcript{TranscriptID: "tr_1", VideoID: v.VideoID}
	e.fake.Notes[v.VideoID] = &app.Note{NoteID: "note_1", VideoID: v.VideoID, Content: "x"}
	ids := strings.TrimSuffix(strings.Repeat("seg_x,", 51), ",")
	cases := []struct{ method, path, body string }{
		{"POST", "/api/v1/imports", ""},
		{"POST", "/api/v1/imports", "{"},
		{"POST", "/api/v1/imports", `{"source":{"type":"file","path":"/a"},"bogus":1}`},
		{"POST", "/api/v1/imports", `{"source":{"type":"file","path":"/a"}} {"again":1}`},
		{"POST", "/api/v1/imports", `[1]`},
		{"PATCH", "/api/v1/settings", `null`},
		{"PATCH", "/api/v1/settings", `"x"`},
		{"PUT", "/api/v1/settings/secrets/llm_api_key", `{"val":"x"}`},
		{"POST", "/api/v1/engine/shutdown", `{"force":"yes"}`},
		{"POST", "/api/v1/tools/update", ``},
		{"POST", "/api/v1/videos/vid_1/exports", `{"format":1}`},
		{"POST", "/api/v1/uploads", `data`}, // no X-Filename
		{"GET", "/api/v1/jobs?limit=abc", ""},
		{"GET", "/api/v1/jobs?limit=0", ""},
		{"GET", "/api/v1/videos?limit=-3", ""},
		{"GET", "/api/v1/jobs/j1?wait_seconds=-1", ""},
		{"GET", "/api/v1/jobs/j1?after_rev=x", ""},
		{"GET", "/api/v1/videos/vid_1/transcript?segment_ids=" + ids, ""},
		{"GET", "/api/v1/videos/vid_1/transcript?format=pdf", ""},
		{"GET", "/api/v1/videos/vid_1/transcript?start_ms=-5", ""},
		{"GET", "/api/v1/videos/vid_1/transcript?max_chars=many", ""},
		{"GET", "/api/v1/videos/vid_1/note?format=html", ""},
		{"GET", "/api/v1/videos/vid_1/note?offset=-1", ""},
		{"GET", "/api/v1/events?last_event_id=abc", ""},
	}
	for _, c := range cases {
		resp, body := e.do(e.req(c.method, c.path, c.body, bearer()))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s %q: status %d %s", c.method, c.path, c.body, resp.StatusCode, body)
			continue
		}
		wantError(t, resp, body, app.CodeInvalidArgument)
	}
	select {
	case <-e.stops:
		t.Fatal("shutdown called for a bad body")
	default:
	}

	// Limits are clamped, defaults applied.
	e.do(e.req("GET", "/api/v1/jobs", "", bearer()))
	call, _ := e.fake.LastCall("ListJobs")
	if got := call.Args[0].(app.ListJobsRequest).Limit; got != 20 {
		t.Fatalf("default limit = %d", got)
	}
	e.do(e.req("GET", "/api/v1/videos?limit=5000", "", bearer()))
	call, _ = e.fake.LastCall("ListVideos")
	if got := call.Args[0].(app.ListVideosRequest).Limit; got != 100 {
		t.Fatalf("max limit = %d", got)
	}
	j := e.fake.AddJob(app.Job{})
	e.do(e.req("GET", "/api/v1/jobs/"+j.JobID+"?wait_seconds=9999&after_rev=0", "", bearer()))
	call, _ = e.fake.LastCall("GetJob")
	if got := call.Args[0].(app.GetJobRequest).WaitSeconds; got != 30 {
		t.Fatalf("wait_seconds = %d", got)
	}
	// Exactly 50 segment IDs are fine.
	ok50 := strings.TrimSuffix(strings.Repeat("seg_x,", 50), ",")
	resp, body := e.do(e.req("GET", "/api/v1/videos/vid_1/transcript?segment_ids="+ok50, "", bearer()))
	if resp.StatusCode != 200 {
		t.Fatalf("50 ids: %d %s", resp.StatusCode, body)
	}
	// Empty bodies are fine where every field is optional.
	resp, _ = e.do(e.req("POST", "/api/v1/tools/prepare", "", bearer()))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prepare with empty body: %d", resp.StatusCode)
	}
	resp, _ = e.do(e.req("POST", "/api/v1/engine/shutdown", "", bearer()))
	if resp.StatusCode != http.StatusAccepted || <-e.stops != false {
		t.Fatalf("shutdown with empty body: %d", resp.StatusCode)
	}
}

func TestTranscriptFormatsAndExportDownloadHeaders(t *testing.T) {
	e := newEnv(t, nil)
	next := int64(9000)
	e.fake.Transcripts["vid_1"] = &app.Transcript{
		TranscriptID: "tr_1", VideoID: "vid_1", Truncated: true, NextStartMs: &next,
		Segments: []app.Segment{{SegmentID: "seg_a", Idx: 1, EndMs: 1000, Text: "你好"}},
	}
	types := map[string]string{
		"srt": "application/x-subrip; charset=utf-8",
		"vtt": "text/vtt; charset=utf-8",
		"txt": "text/plain; charset=utf-8",
	}
	for format, ct := range types {
		resp, body := e.do(e.req("GET", "/api/v1/videos/vid_1/transcript?format="+format, "", bearer()))
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != ct || body != strings.ToUpper(format)+"\n你好\n" {
			t.Fatalf("%s: %d %q %q", format, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
		if resp.Header.Get(api.HeaderTruncated) != "true" || resp.Header.Get(api.HeaderNextStartMs) != "9000" {
			t.Fatalf("%s: paging headers %v", format, resp.Header)
		}
	}
	for _, q := range []string{"", "?format=json"} {
		resp, body := e.do(e.req("GET", "/api/v1/videos/vid_1/transcript"+q, "", bearer()))
		var tr app.Transcript
		if err := json.Unmarshal([]byte(body), &tr); err != nil || resp.StatusCode != 200 ||
			tr.Format != "json" || len(tr.Segments) != 1 || tr.Content != "" {
			t.Fatalf("json transcript: %d %s", resp.StatusCode, body)
		}
	}

	exp := e.fake.AddExport("vid_1", app.FormatMD, `第1讲 "导论".md`, []byte("# hi\n"))
	resp, body := e.do(e.req("GET", "/api/v1/exports/"+exp.ExportID+"/download", "", bearer()))
	cd := resp.Header.Get("Content-Disposition")
	want := `attachment; filename="_1_ ____.md"; filename*=UTF-8''%E7%AC%AC1%E8%AE%B2%20%22%E5%AF%BC%E8%AE%BA%22.md`
	if resp.StatusCode != 200 || body != "# hi\n" || cd != want ||
		resp.Header.Get("Content-Type") != "text/markdown; charset=utf-8" || resp.Header.Get("Content-Length") != "5" {
		t.Fatalf("download: %d %q\n got %s\nwant %s", resp.StatusCode, body, cd, want)
	}
}

func TestUploadIsStreamedNotBuffered(t *testing.T) {
	e := newEnv(t, nil)
	// The handler must hand over the body while the client is still sending:
	// the second half is only written after the service consumed the first.
	pr, pw := io.Pipe()
	r := e.req("POST", "/api/v1/uploads", "", bearer(), "X-Filename: "+url.PathEscape("C:\\课程\\第 1 讲.mp4"))
	r.Body = pr
	r.ContentLength = -1
	done := make(chan struct{})
	var up app.Upload
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("upload status %d", resp.StatusCode)
		}
		_ = json.NewDecoder(resp.Body).Decode(&up)
	}()
	_, _ = pw.Write(bytes.Repeat([]byte("a"), 1<<16))
	deadline := time.Now().Add(5 * time.Second)
	for e.fake.CallCount("CreateUpload") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("service did not receive the upload while the body was still open")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, _ = pw.Write(bytes.Repeat([]byte("b"), 1<<16))
	pw.Close()
	<-done
	call, _ := e.fake.LastCall("CreateUpload")
	if call.Args[0] != "第 1 讲.mp4" || call.Args[1] != int64(-1) {
		t.Fatalf("CreateUpload args = %v", call.Args)
	}
	if data, _ := e.fake.UploadData(up.UploadID); len(data) != 2<<16 || up.SizeBytes != 2<<16 {
		t.Fatalf("stored %d bytes, reported %d", len(data), up.SizeBytes)
	}
}

func TestEventStreamWireFormat(t *testing.T) {
	e := newEnv(t, nil)
	e.fake.Emit(app.Event{Type: app.EventVideoUpdated, Video: &app.Video{VideoID: "vid_1"}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := e.req("GET", "/api/v1/events", "", bearer()).WithContext(ctx)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	if resp.Header.Get(api.HeaderStreamReset) != "" {
		t.Fatal("unexpected reset")
	}
	waitFor(t, func() bool { return e.h.Stats().Subscribers == 1 })

	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	rev := int64(7)
	e.fake.Emit(app.Event{Type: app.EventJobUpdated, Job: &app.Job{JobID: "job_1", Rev: rev}})
	e.fake.Emit(app.Event{Type: app.EventToolsUpdated, Tools: &app.ToolsStatus{Ready: true}})
	e.fake.Emit(app.Event{Type: app.EventEngineStopping, Reason: "idle"})

	var got []string
	heartbeat, afterComment := false, false
	timeout := time.After(5 * time.Second)
	for !(heartbeat && len(got) >= 16) {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("stream ended early: %q", got)
			}
			if l == ": hb" {
				heartbeat = true
			}
			// A comment is its own block: skip it and its blank line.
			if strings.HasPrefix(l, ":") || (afterComment && l == "") {
				afterComment = l != ""
				continue
			}
			afterComment = false
			got = append(got, l)
		case <-timeout:
			t.Fatalf("timeout; heartbeat=%v lines=%q", heartbeat, got)
		}
	}
	text := strings.Join(got[:16], "\n")
	for _, want := range []string{
		"id: 1\nevent: video.updated\ndata: {\"video_id\":\"vid_1\",",
		"id: 2\nevent: job.updated\ndata: {\"job_id\":\"job_1\",",
		"id: 3\nevent: tools.updated\ndata: {\"items\":null,\"ready\":true,",
		"id: 4\nevent: engine.stopping\ndata: {\"reason\":\"idle\"}\n",
	} {
		if !strings.Contains(text+"\n", want) {
			t.Fatalf("stream misses %q in:\n%s", want, text)
		}
	}
	// Each event is a 3-line block followed by a blank line; data is the bare object.
	if got[3] != "" || !strings.HasPrefix(got[2], "data: {") || strings.Contains(got[2], `"video":`) {
		t.Fatalf("framing: %q", got[:4])
	}

	cancel()
	waitFor(t, func() bool { return e.h.Stats().Subscribers == 0 })

	// Resume by query parameter, and reset when the position is unknown.
	resp2, err := http.DefaultClient.Do(e.req("GET", "/api/v1/events?last_event_id=999", "", bearer()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	br := bufio.NewReader(resp2.Body)
	first, _ := br.ReadString('\n')
	second, _ := br.ReadString('\n')
	if resp2.Header.Get(api.HeaderStreamReset) != "1" || first != "event: reset\n" || second != "data: {}\n" {
		t.Fatalf("reset stream starts with %q %q (header %q)", first, second, resp2.Header.Get(api.HeaderStreamReset))
	}
}

func TestCloseEndsStreamsAfterFlushingQueuedEvents(t *testing.T) {
	e := newEnv(t, func(o *api.Options) { o.HeartbeatInterval = time.Hour })
	resp, err := http.DefaultClient.Do(e.req("GET", "/api/v1/events", "", bearer()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	waitFor(t, func() bool { return e.h.Stats().Subscribers == 1 })
	// A long poll is woken too.
	j := e.fake.AddJob(app.Job{})
	pollDone := make(chan int, 1)
	go func() {
		r, _ := e.do(e.req("GET", "/api/v1/jobs/"+j.JobID+"?wait_seconds=30&after_rev=1", "", bearer()))
		pollDone <- r.StatusCode
	}()
	waitFor(t, func() bool { return e.fake.CallCount("GetJob") == 1 })

	e.fake.Emit(app.Event{Type: app.EventEngineStopping, Reason: "shutdown"})
	e.h.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "event: engine.stopping\ndata: {\"reason\":\"shutdown\"}") {
		t.Fatalf("stream ended without the queued event: %q", body)
	}
	select {
	case code := <-pollDone:
		if code != 200 {
			t.Fatalf("long poll: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long poll not released by Close")
	}
	waitFor(t, func() bool { return e.h.Stats().Subscribers == 0 })
}

func TestStatsTrackAuthorisedRequestsOnly(t *testing.T) {
	e := newEnv(t, nil)
	e.do(e.req("GET", "/api/v1/health", ""))
	e.do(e.req("GET", "/", ""))
	e.do(e.req("GET", "/api/v1/engine", ""))
	if st := e.h.Stats(); !st.LastRequest.IsZero() || st.InFlight != 0 {
		t.Fatalf("unauthenticated traffic counted: %+v", st)
	}
	before := time.Now()
	e.do(e.req("GET", "/api/v1/engine", "", bearer()))
	if st := e.h.Stats(); st.LastRequest.Before(before) || st.InFlight != 0 {
		t.Fatalf("stats after request: %+v (before %v)", st, before)
	}
}

func TestRequestLogRedactsSecrets(t *testing.T) {
	e := newEnv(t, nil)
	e.fake.PlaybackData["vid_1"] = []byte("x")
	e.do(e.req("GET", "/media/vid_1/playback?mt="+e.fake.MediaToken+"&x=1", ""))
	e.do(e.req("GET", "/api/v1/engine?code=SECRETCODE&next=/a", "", bearer()))
	e.do(e.req("GET", "/api/v1/engine?token=QUERYTOKEN", "", bearer()))
	logs := e.logText()
	for _, secret := range []string{e.fake.MediaToken, "SECRETCODE", "QUERYTOKEN", testToken} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log leaks %q:\n%s", secret, logs)
		}
	}
	for _, want := range []string{"mt=[REDACTED]&x=1", "code=[REDACTED]&next=/a", "path=/media/vid_1/playback", "auth=bearer"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log misses %q:\n%s", want, logs)
		}
	}
	if got := api.RedactQuery("a=1&TOKEN=x&mt=y&code=&flag"); got != "a=1&TOKEN=[REDACTED]&mt=[REDACTED]&code=[REDACTED]&flag" {
		t.Fatalf("RedactQuery = %q", got)
	}
}

func TestEngineInfoOverlayAndServiceErrors(t *testing.T) {
	e := newEnv(t, func(o *api.Options) {
		o.EngineInfo = func(info *app.EngineInfo) { info.PID = 4242; info.Mode = "background"; info.API = app.APIVersion }
	})
	e.fake.Info.LLMConfigured = true
	resp, body := e.do(e.req("GET", "/api/v1/engine", "", bearer()))
	var info app.EngineInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil || resp.StatusCode != 200 ||
		info.PID != 4242 || info.Mode != "background" || !info.LLMConfigured || !info.ToolsReady || info.API != 1 {
		t.Fatalf("engine info: %s", body)
	}

	// Every contract code keeps its status and flags through the envelope.
	for _, code := range append(app.KnownCodes(), app.CodeUnauthorized) {
		e.fake.SetError("GetVideo", app.E(code, "boom").WithHint("h").WithStage("probe").WithDetail("k", "v"))
		resp, body := e.do(e.req("GET", "/api/v1/videos/vid_x", "", bearer()))
		got := wantError(t, resp, body, code)
		want := app.E(code, "boom")
		if got.Retryable != want.Retryable || got.UserActionRequired != want.UserActionRequired ||
			got.Hint != "h" || got.Stage != "probe" || got.Details["k"] != "v" {
			t.Fatalf("%s: envelope %+v", code, got)
		}
	}
	// A non-app error becomes INTERNAL; a panic too.
	e.fake.SetError("GetVideo", io.ErrUnexpectedEOF)
	resp, body = e.do(e.req("GET", "/api/v1/videos/vid_x", "", bearer()))
	wantError(t, resp, body, app.CodeInternal)
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
