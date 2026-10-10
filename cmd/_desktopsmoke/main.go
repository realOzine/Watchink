//go:build desktop

// Command _desktopsmoke is the S-01 / S-04 evidence program (docs/v0.md
// §18): it opens the real desktop shell with a test page and checks, from
// inside the WebView, that the page can call a binding and reach a loopback
// HTTP server that applies the engine's CORS policy (bearer auth, preflight,
// streamed SSE, Range media). It prints one PASS/FAIL line per check and
// exits by itself.
//
// Build (macOS):
//
//	CGO_ENABLED=1 CGO_CFLAGS=-mmacosx-version-min=11.0 \
//	CGO_LDFLAGS="-framework UniformTypeIdentifiers -mmacosx-version-min=11.0" \
//	go build -tags desktop,production -o smoke ./cmd/_desktopsmoke
//
// Build (Windows; a console program, so the result lines are visible):
//
//	go build -tags desktop,production -o smoke.exe ./cmd/_desktopsmoke
//
// Flags: -media <file> serves that file with Range support and adds the media
// checks; -timeout bounds the whole run.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	"watchink/internal/app"
	"watchink/internal/desktop"
)

const token = "smoke-token-0123456789abcdef"

// server mimics the engine's HTTP surface as far as the WebView is concerned.
type server struct {
	base  string
	media string

	mu         sync.Mutex
	origins    map[string]int
	preflights []string
	ranges     []string
	userAgent  string
}

func (s *server) note(f func()) { s.mu.Lock(); f(); s.mu.Unlock() }

// cors applies the policy of §15: only the Wails WebView origin is allowed.
func (s *server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != strings.TrimPrefix(s.base, "http://") {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		origin := r.Header.Get("Origin")
		s.note(func() {
			if origin != "" {
				s.origins[origin]++
			}
			s.userAgent = r.Header.Get("User-Agent")
		})
		w.Header().Add("Vary", "Origin")
		if origin == desktop.WebViewOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Expose-Headers", "Content-Range, Accept-Ranges, Content-Length")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			s.note(func() {
				s.preflights = append(s.preflights, fmt.Sprintf("%s %s headers=[%s] origin=%s",
					r.Header.Get("Access-Control-Request-Method"), r.URL.Path,
					r.Header.Get("Access-Control-Request-Headers"), origin))
			})
			if origin == desktop.WebViewOrigin {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID, X-WK-Request")
				w.Header().Set("Access-Control-Max-Age", "600")
				// Chromium (WebView2) may ask before a page reaches loopback;
				// the engine answers the same way (internal/api/security.go).
				if strings.EqualFold(r.Header.Get("Access-Control-Request-Private-Network"), "true") {
					w.Header().Set("Access-Control-Allow-Private-Network", "true")
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(app.ErrorEnvelope{Error: app.E(app.CodeUnauthorized, "missing or wrong token")})
			return
		}
		next(w, r)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/engine", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(app.EngineInfo{Version: "smoke", API: app.APIVersion, BaseURL: s.base})
	}))
	mux.HandleFunc("POST /api/v1/echo", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"echo": body, "content_type": r.Header.Get("Content-Type")})
	}))
	// Five events 300 ms apart: the page checks that they arrive one by one.
	mux.HandleFunc("GET /api/v1/events", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		fl := w.(http.Flusher)
		fmt.Fprint(w, ": hello\n\n")
		fl.Flush()
		for i := 1; i <= 5; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
			fmt.Fprintf(w, "id: %d\nevent: job.updated\ndata: {\"n\":%d}\n\n", i, i)
			fl.Flush()
		}
	}))
	// Media is authorized by a token in the URL, as in §10.2.
	mux.HandleFunc("GET /media/sample", func(w http.ResponseWriter, r *http.Request) {
		if s.media == "" || r.URL.Query().Get("t") != "media-token" {
			http.NotFound(w, r)
			return
		}
		s.note(func() { s.ranges = append(s.ranges, r.Header.Get("Range")) })
		http.ServeFile(w, r, s.media)
	})
	return s.cors(mux)
}

// remote is the fake app.Remote handed to the shell; only what the shell
// itself calls is implemented.
type remote struct {
	app.Remote
	base, dataDir string
}

func (r remote) BaseURL() string { return r.base }
func (r remote) Token() string   { return token }
func (r remote) Close() error    { return nil }
func (r remote) EngineInfo(context.Context) (*app.EngineInfo, error) {
	return &app.EngineInfo{Version: "smoke", BaseURL: r.base, DataDir: r.dataDir}, nil
}

type result struct {
	OK     bool
	Detail string
}

// Smoke is the extra, smoke-only binding the page reports through.
type Smoke struct {
	mu      sync.Mutex
	results map[string]result
	order   []string
	done    chan struct{}
	once    sync.Once
}

// Report records one check.
func (s *Smoke) Report(name string, ok bool, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.results[name]; !seen {
		s.order = append(s.order, name)
	}
	s.results[name] = result{ok, detail}
}

// Done signals that the page ran every check.
func (s *Smoke) Done() { s.once.Do(func() { close(s.done) }) }

func main() {
	mediaPath := flag.String("media", "", "media file to serve with Range support (optional)")
	timeout := flag.Duration("timeout", 25*time.Second, "overall timeout")
	flag.Parse()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("FAIL listen:", err)
		os.Exit(1)
	}
	srv := &server{base: "http://" + ln.Addr().String(), origins: map[string]int{}}
	if *mediaPath != "" {
		if abs, err := filepath.Abs(*mediaPath); err == nil {
			if _, err := os.Stat(abs); err == nil {
				srv.media = abs
			}
		}
	}
	go http.Serve(ln, srv.routes())

	dataDir, _ := os.MkdirTemp("", "ltsmoke-data-*")
	defer os.RemoveAll(dataDir)
	os.WriteFile(filepath.Join(dataDir, "inside.txt"), []byte("x"), 0o600)

	smoke := &Smoke{results: map[string]result{}, done: make(chan struct{})}
	var revealed []string
	var revealMu sync.Mutex

	page := strings.NewReplacer(
		"__HAS_MEDIA__", fmt.Sprint(srv.media != ""),
		// Forward slashes: the value lands in a JavaScript string literal, where
		// the backslashes of a Windows path would be read as escapes.
		"__DATA_DIR__", filepath.ToSlash(dataDir),
	).Replace(pageHTML)
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(page)}}

	// expected lists every check the page must report; a missing one fails.
	expected := []string{"binding.EngineInfo", "page.origin", "fetch.auth", "fetch.unauthorized", "fetch.post",
		"sse.stream", "binding.error_format", "binding.reveal", "binding.agent_setup"}
	if srv.media != "" {
		expected = append(expected, "media.metadata", "media.seek")
	}

	exit := func(reason string) {
		srv.mu.Lock()
		smoke.mu.Lock()
		revealMu.Lock()
		fmt.Printf("--- desktop smoke (%s) ---\n", reason)
		fmt.Printf("server: %s  media: %v\n", srv.base, srv.media != "")
		fmt.Printf("user-agent: %s\n", srv.userAgent)
		var origins []string
		for o, n := range srv.origins {
			origins = append(origins, fmt.Sprintf("%q x%d", o, n))
		}
		sort.Strings(origins)
		fmt.Printf("Origin headers seen by the server: %s\n", strings.Join(origins, ", "))
		fmt.Printf("CORS preflights seen: %d\n", len(srv.preflights))
		for _, p := range srv.preflights {
			fmt.Println("  preflight:", p)
		}
		if srv.media != "" {
			fmt.Printf("media Range headers: %q\n", srv.ranges)
		}
		fmt.Printf("reveal calls: %q\n", revealed)
		failed := 0
		names := append([]string{}, expected...)
		for _, n := range smoke.order {
			found := false
			for _, e := range expected {
				found = found || e == n
			}
			if !found {
				names = append(names, n) // informational extras
			}
		}
		for _, n := range names {
			r, ok := smoke.results[n]
			status := "PASS"
			switch {
			case !ok:
				status, r.Detail = "FAIL", "not reported (page did not get that far)"
				failed++
			case !r.OK && strings.HasPrefix(n, "info."):
				status = "INFO"
			case !r.OK:
				status = "FAIL"
				failed++
			}
			fmt.Printf("%s %-24s %s\n", status, n, r.Detail)
		}
		if failed > 0 {
			fmt.Printf("RESULT: FAIL (%d failed)\n", failed)
			os.Exit(1)
		}
		fmt.Println("RESULT: PASS")
		os.Exit(0)
	}
	go func() {
		select {
		case <-smoke.done:
			time.Sleep(300 * time.Millisecond)
			exit("page finished")
		case <-time.After(*timeout):
			exit("timeout")
		}
	}()

	err = desktop.Run(desktop.Options{
		Connect: func(context.Context, app.ConnectOptions) (app.Remote, error) {
			return remote{base: srv.base, dataDir: dataDir}, nil
		},
		Assets:  assets,
		Version: "smoke",
		Reveal: func(p string) error {
			revealMu.Lock()
			revealed = append(revealed, p)
			revealMu.Unlock()
			return nil
		},
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		ExtraBind: []any{smoke},
	})
	if err != nil {
		fmt.Println("FAIL desktop.Run:", err)
		os.Exit(1)
	}
	exit("window closed")
}

const pageHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Watchink smoke</title></head>
<body style="font:14px -apple-system;padding:16px">
<h3>Watchink desktop smoke test</h3><pre id="log"></pre>
<script>
const HAS_MEDIA = __HAS_MEDIA__;
const DATA_DIR = "__DATA_DIR__";
const logEl = document.getElementById('log');
function withTimeout(p, ms, what) {
  return Promise.race([p, new Promise((_, rej) => setTimeout(() => rej(new Error(what + ' timed out after ' + ms + ' ms')), ms))]);
}
async function check(name, fn) {
  let ok = false, detail = '';
  try { detail = String(await withTimeout(fn(), 8000, name)); ok = true; }
  catch (e) { detail = (e && e.name ? e.name + ': ' : '') + (e && e.message ? e.message : String(e)); }
  logEl.textContent += (ok ? 'PASS ' : 'FAIL ') + name + ' ' + detail + '\n';
  try { await window.go.main.Smoke.Report(name, ok, detail); } catch (e) { logEl.textContent += 'report failed: ' + e + '\n'; }
  return ok;
}
function assert(c, msg) { if (!c) throw new Error(msg); }
function errOf(e) { return JSON.parse(e instanceof Error ? e.message : e); }

async function main() {
  const App = window.go.desktop.App;
  let info = null;

  await check('binding.EngineInfo', async () => {
    info = await App.EngineInfo();
    assert(/^http:\/\/127\.0\.0\.1:\d+$/.test(info.base_url), 'base_url=' + info.base_url);
    assert(info.token && info.version === 'smoke', 'token/version missing');
    return 'keys=' + Object.keys(info).join(',') + ' go=' + Object.keys(window.go).join(',') +
      ' methods=' + Object.keys(App).sort().join(',');
  });
  if (!info) return;
  const H = { Authorization: 'Bearer ' + info.token };

  await check('page.origin', async () =>
    'location.origin=' + location.origin + ' href=' + location.href + ' isSecureContext=' + window.isSecureContext);

  await check('fetch.auth', async () => {
    const r = await fetch(info.base_url + '/api/v1/engine', { headers: H });
    assert(r.status === 200, 'status ' + r.status);
    const j = await r.json();
    assert(j.version === 'smoke' && j.api === 1, 'body ' + JSON.stringify(j));
    return 'GET with Authorization -> 200, JSON readable';
  });

  await check('fetch.unauthorized', async () => {
    const r = await fetch(info.base_url + '/api/v1/engine');
    assert(r.status === 401, 'status ' + r.status);
    const j = await r.json();
    assert(j.error && j.error.code === 'UNAUTHORIZED', 'body ' + JSON.stringify(j));
    return '401 error body readable cross-origin';
  });

  await check('fetch.post', async () => {
    const r = await fetch(info.base_url + '/api/v1/echo', {
      method: 'POST', headers: { ...H, 'Content-Type': 'application/json', 'X-WK-Request': '1' },
      body: JSON.stringify({ hello: '世界' }) });
    assert(r.status === 200, 'status ' + r.status);
    const j = await r.json();
    assert(j.echo && j.echo.hello === '世界', 'body ' + JSON.stringify(j));
    return 'POST JSON with Authorization + custom header -> 200';
  });

  await check('sse.stream', async () => {
    const t0 = performance.now();
    const r = await fetch(info.base_url + '/api/v1/events', { headers: { ...H, 'Last-Event-ID': '0' } });
    assert(r.status === 200, 'status ' + r.status);
    assert((r.headers.get('content-type') || '').startsWith('text/event-stream'), 'content-type ' + r.headers.get('content-type'));
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let buf = '', arrivals = [];
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        const frame = buf.slice(0, i); buf = buf.slice(i + 2);
        if (frame.startsWith('id:')) arrivals.push(Math.round(performance.now() - t0));
      }
    }
    assert(arrivals.length === 5, 'events=' + arrivals.length);
    // Sent 300 ms apart: incremental delivery means the first one arrives
    // long before the last. Buffered delivery would bunch them together.
    const spread = arrivals[4] - arrivals[0];
    assert(spread > 800, 'events were buffered: arrivals(ms)=' + arrivals.join(','));
    return '5 events delivered incrementally, arrivals(ms)=' + arrivals.join(',');
  });

  await check('binding.error_format', async () => {
    let rejected = null;
    try { await App.OpenExternal('file:///etc/passwd'); } catch (e) { rejected = e; }
    assert(rejected !== null, 'OpenExternal(file://) was not rejected');
    // Wails v2.16 rejects with new Error(<formatter output>): the app.Error
    // JSON is in .message.
    const shape = rejected instanceof Error ? 'Error(.message=JSON)' : typeof rejected;
    const err = errOf(rejected);
    assert(err.code === 'INVALID_ARGUMENT' && typeof err.message === 'string', 'error ' + rejected);
    return 'rejection is ' + shape + ', code=' + err.code + ' retryable=' + err.retryable;
  });

  await check('binding.reveal', async () => {
    await App.RevealInFileManager(DATA_DIR + '/inside.txt');
    let rejected = null;
    try { await App.RevealInFileManager('/etc/hosts'); } catch (e) { rejected = errOf(e); }
    assert(rejected && rejected.code === 'PATH_NOT_ALLOWED', 'outside path: ' + JSON.stringify(rejected));
    return 'inside data dir ok; /etc/hosts -> PATH_NOT_ALLOWED';
  });

  await check('binding.agent_setup', async () => {
    const s = await App.AgentSetupInfo();
    assert(s.executable.startsWith('/') && s.mcp_command.length === 2 && s.mcp_command[1] === 'mcp', JSON.stringify(s));
    assert(s.snippets.length === 2 && s.snippets.every(x => x.name && x.language && x.content), 'snippets');
    return 'executable=' + s.executable;
  });

  if (HAS_MEDIA) {
    const el = document.createElement('audio');
    el.preload = 'auto'; el.muted = true;
    document.body.appendChild(el);
    const once = (ev) => new Promise((res, rej) => {
      el.addEventListener(ev, () => res(), { once: true });
      el.addEventListener('error', () => rej(new Error('media error code ' + (el.error && el.error.code) + ' ' + (el.error && el.error.message))), { once: true });
    });
    const metaOK = await check('media.metadata', async () => {
      const p = once('loadedmetadata');
      el.src = info.base_url + '/media/sample?t=media-token';
      await p;
      assert(el.duration > 5, 'duration ' + el.duration);
      return 'loadedmetadata, duration=' + el.duration.toFixed(2) + 's';
    });
    if (metaOK) {
      await check('media.seek', async () => {
        const target = el.duration * 0.8;
        const p = once('seeked');
        el.currentTime = target;
        await p;
        assert(Math.abs(el.currentTime - target) < 1, 'currentTime ' + el.currentTime);
        return 'seeked to ' + el.currentTime.toFixed(2) + 's, readyState=' + el.readyState;
      });
      // Informational: playback without a user gesture is a WebKit policy
      // matter, not a transport one.
      await check('info.media.play', async () => {
        await el.play();
        await new Promise(r => setTimeout(r, 600));
        const t = el.currentTime; el.pause();
        return 'muted play() resolved, currentTime=' + t.toFixed(2);
      });
    }
  }
}
window.addEventListener('load', () => {
  // The Wails runtime injects window.go before load in production builds.
  main().catch(e => { logEl.textContent += 'fatal: ' + e + '\n'; })
        .finally(() => { try { window.go.main.Smoke.Done(); } catch (e) {} });
});
</script></body></html>`
