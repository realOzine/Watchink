//go:build desktop && !windows

package desktop

// SystemWebView describes the WebView runtime the window depends on when the
// operating system does not guarantee one (§16.3). macOS ships WKWebView with
// the system, so nothing is required here.
func SystemWebView() (name, version string, required bool) { return "", "", false }
