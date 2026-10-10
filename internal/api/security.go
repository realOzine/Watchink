package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"

	"watchink/internal/app"
)

const (
	authBearer = "bearer"
	authWeb    = "web"
)

// hostAllowed implements the anti DNS-rebinding rule of §15: the Host header
// must name the loopback listener itself.
func (h *Handler) hostAllowed(host string) bool {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	if name != "127.0.0.1" && name != "localhost" {
		return false
	}
	if h.opts.Port != 0 {
		return port == strconv.Itoa(h.opts.Port)
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n < 65536
}

// corsAllowHeaders / corsExposeHeaders are what the desktop WebView frontend
// sends and reads.
const (
	corsAllowMethods  = "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders  = "Authorization, Content-Type, X-WK-Request, X-Filename, Last-Event-ID"
	corsExposeHeaders = "Content-Disposition, Content-Length, Content-Range, Accept-Ranges, ETag, " +
		HeaderStreamReset + ", " + HeaderExport + ", " + HeaderTruncated + ", " + HeaderNextStartMs
)

// cors adds the CORS response headers for an allowed WebView origin and
// answers preflights (including Chromium's private-network preflight, D-04).
// It reports whether the request was fully handled.
func (h *Handler) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	allowed := origin != "" && h.origins[origin]
	w.Header().Add("Vary", "Origin")
	if allowed {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders)
	}
	if r.Method != http.MethodOptions || r.Header.Get("Access-Control-Request-Method") == "" {
		return false
	}
	if !allowed {
		writeError(w, app.E(app.CodeUnauthorized, "来源不被允许").WithDetail("reason", "origin"))
		return true
	}
	w.Header().Add("Vary", "Access-Control-Request-Method, Access-Control-Request-Headers")
	w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
	w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
	w.Header().Set("Access-Control-Max-Age", "600")
	if strings.EqualFold(r.Header.Get("Access-Control-Request-Private-Network"), "true") {
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

// authenticate returns how the request is authorised: by bearer token
// (desktop, CLI, MCP), as a same-origin browser request while the optional
// Web UI is switched on, or not at all (""). A present but wrong
// Authorization header never falls back to the browser rule.
func (h *Handler) authenticate(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		scheme, tok, ok := strings.Cut(v, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || !h.hasToken {
			return ""
		}
		// Comparing digests keeps the comparison constant-time regardless of
		// the presented token's length.
		sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
		if subtle.ConstantTimeCompare(sum[:], h.tokenSum[:]) == 1 {
			return authBearer
		}
		return ""
	}
	if h.svc.WebEnabled() && sameOrigin(r) {
		return authWeb
	}
	return ""
}

// sameOrigin reports whether a tokenless request may come from the Web UI
// itself. The Web UI has no login, so other websites open in the same
// browser must not be able to drive the API: a request that the browser
// marks as cross-site, or whose Origin is not ours, is refused. (Reading a
// response cross-origin is already impossible: CORS only allows the desktop
// WebView.)
func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

func isStateChanging(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func unauthorized(reason, msg string) *app.Error {
	return app.E(app.CodeUnauthorized, "%s", msg).WithDetail("reason", reason)
}

// webDisabledHint tells a browser user how to switch the Web UI on.
const webDisabledHint = "在 Watchink 桌面应用的“设置 → Web 界面”中开启，或在终端运行 `watchink web`。"

// authed wraps a route that needs a bearer token or, while the Web UI is
// enabled, a same-origin browser request. A browser request that changes
// state must also carry our own Origin and X-WK-Request: 1: a custom header
// cannot be sent cross-site without a CORS preflight, which we refuse (§15).
func (h *Handler) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := h.authenticate(r)
		switch {
		case kind == "" && r.Header.Get("Authorization") == "" && !h.svc.WebEnabled():
			writeError(w, unauthorized("web_disabled", "Web 界面没有开启").WithHint(webDisabledHint))
			return
		case kind == "":
			w.Header().Set("WWW-Authenticate", `Bearer realm="watchink"`)
			writeError(w, unauthorized("credentials", "未授权：缺少或错误的令牌，或请求不是来自 Watchink 自己的页面"))
			return
		case kind == authWeb && isStateChanging(r.Method):
			if r.Header.Get("Origin") != "http://"+r.Host {
				writeError(w, unauthorized("origin", "Origin 不匹配"))
				return
			}
			if r.Header.Get(HeaderRequest) != "1" {
				writeError(w, unauthorized("csrf_header", "缺少 X-WK-Request 头"))
				return
			}
		}
		if sw, ok := w.(*statusWriter); ok {
			sw.auth = kind
		}
		h.track(func() { next(w, r) })
	}
}

// track marks an authorised request for idle-exit accounting (§5.4).
func (h *Handler) track(fn func()) {
	h.touch()
	h.inflight.Add(1)
	defer func() {
		h.inflight.Add(-1)
		h.touch()
	}()
	fn()
}

// withBase returns a context that is also cancelled by Handler.Close, so long
// polls do not hold up engine shutdown.
func (h *Handler) withBase(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.baseCtx, cancel)
	return ctx, func() { stop(); cancel() }
}
