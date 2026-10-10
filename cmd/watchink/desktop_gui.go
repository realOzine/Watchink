//go:build desktop

package main

import (
	"context"
	"os"

	"watchink/internal/cli"
	"watchink/internal/client"
	"watchink/internal/desktop"
	"watchink/internal/procx"
	"watchink/internal/sysreq"
	"watchink/internal/version"
	"watchink/internal/webui"
)

// webView is the `doctor` probe of the system WebView.
var webView = desktop.SystemWebView

// runDesktop opens the Wails window. It must run on the main goroutine.
var runDesktop = func(_ context.Context, dataDir string) error {
	assets, _ := webui.FS()
	exe, _ := os.Executable()
	return desktop.Run(desktop.Options{
		Connect: client.Connect, DataDir: dataDir, Assets: assets,
		Version: version.Version, Executable: exe, Reveal: procx.RevealInFileManager,
	})
}

// desktopPrereqs hands `doctor` the system libraries the window needs;
// the list is empty on macOS.
var desktopPrereqs = func() []cli.SystemPrereq {
	var out []cli.SystemPrereq
	for _, r := range sysreq.Desktop() {
		out = append(out, cli.SystemPrereq{Name: r.Name, Package: r.Package, Optional: r.Optional, Found: r.Found})
	}
	return out
}
