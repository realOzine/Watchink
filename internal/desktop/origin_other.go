//go:build desktop && !windows

package desktop

// WebViewOrigin is the origin of the page inside the Wails WebView on macOS
// (and Linux); the engine's CORS policy must allow exactly this value (§15,
// S-04). Verified on macOS with cmd/_desktopsmoke. Windows differs: see
// origin_windows.go.
const WebViewOrigin = "wails://wails"
