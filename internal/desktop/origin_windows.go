//go:build desktop && windows

package desktop

// WebViewOrigin is the origin of the page inside the Wails WebView; the
// engine's CORS policy must allow exactly this value (§15, S-04). WebView2
// has no custom schemes for top-level documents, so Wails serves the embedded
// assets from a virtual http host. Chromium treats *.localhost as loopback
// and as a secure context.
//
// Not verified on a real machine yet (S-04 on Windows): run
// cmd/_desktopsmoke there.
const WebViewOrigin = "http://wails.localhost"
