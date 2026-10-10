package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"watchink/internal/app"
)

// contentSecurityPolicy fits a Vite-built React SPA that talks to and plays
// media from its own origin only: no inline or remote scripts, no framing.
// 'unsafe-inline' is limited to styles, which React's style attributes need.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; " +
	"media-src 'self' blob:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"worker-src 'self' blob:; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

const uiNotBuilt = "Watchink Web 界面尚未构建。\n\n" +
	"此程序未内嵌前端资源：请先构建前端（frontend/ → internal/webui/dist）后重新编译。\n" +
	"Engine 与 HTTP API 不受影响，CLI 与 MCP 仍可正常使用。\n"

func (h *Handler) loadUI(ui fs.FS) {
	if ui == nil {
		return
	}
	index, err := fs.ReadFile(ui, "index.html")
	if err != nil {
		return
	}
	sum := sha256.Sum256(index)
	h.ui, h.index = ui, index
	h.indexETag = `"` + hex.EncodeToString(sum[:8]) + `"`
}

// spa serves the embedded frontend to browsers. The Web UI is optional
// (settings web.enabled): while it is off, nothing but a short notice is
// served. The desktop window does not come through here; it loads the same
// frontend from its own embedded assets.
func (h *Handler) spa(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, app.E(app.CodeNotFound, "接口不存在：%s %s", r.Method, r.URL.Path))
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Security-Policy", contentSecurityPolicy)
	hdr.Set("X-Frame-Options", "DENY")

	if !h.svc.WebEnabled() {
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		hdr.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusForbidden)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("Watchink 的 Web 界面没有开启。\n\n" + webDisabledHint + "\n"))
		}
		return
	}
	if h.index == nil {
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		hdr.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(uiNotBuilt))
		}
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "index.html" {
		h.serveIndex(w, r)
		return
	}
	if fs.ValidPath(name) {
		if st, err := fs.Stat(h.ui, name); err == nil && !st.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				// Vite puts a content hash in every file name under assets/.
				hdr.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				hdr.Set("Cache-Control", "no-cache")
			}
			http.ServeFileFS(w, r, h.ui, name)
			return
		}
	}
	// History-API fallback: client-side routes such as /videos/<id> get the
	// shell. A missing asset or file-like path is a real 404, otherwise the
	// browser would try to run index.html as a script.
	if name == "assets" || strings.HasPrefix(name, "assets/") || path.Ext(name) != "" {
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		hdr.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 not found\n"))
		return
	}
	h.serveIndex(w, r)
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	// index.html names the hashed assets, so it must always be revalidated.
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("ETag", h.indexETag)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, h.indexETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(h.index)
	}
}
