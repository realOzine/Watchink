//go:build desktop && !windows

package desktop

// WebViewOrigin is the origin of the page inside the Wails WebView on macOS
// (and Linux); the engine's CORS policy must allow exactly this value.
// Windows differs: see origin_windows.go.
const WebViewOrigin = "wails://wails"
