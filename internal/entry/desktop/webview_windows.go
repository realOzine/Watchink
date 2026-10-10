//go:build desktop && windows

package desktop

import "github.com/wailsapp/go-webview2/webviewloader"

// SystemWebView describes the WebView runtime the window depends on when the
// operating system does not guarantee one. On Windows that is the
// WebView2 runtime: part of Windows 11, an optional install before that.
// version is "" when it is missing; the lookup is the one Wails itself does
// at startup (registry only, no process is started).
func SystemWebView() (name, version string, required bool) {
	version, err := webviewloader.GetAvailableCoreWebView2BrowserVersionString("")
	if err != nil {
		version = ""
	}
	return "WebView2 运行时", version, true
}
