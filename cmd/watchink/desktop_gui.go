//go:build desktop

package main

import (
	"context"
	"os"

	"watchink/internal/client"
	"watchink/internal/desktop"
	"watchink/internal/procx"
	"watchink/internal/version"
	"watchink/internal/webui"
)

// runDesktop opens the Wails window. It must run on the main goroutine.
var runDesktop = func(_ context.Context, dataDir string) error {
	assets, _ := webui.FS()
	exe, _ := os.Executable()
	return desktop.Run(desktop.Options{
		Connect: client.Connect, DataDir: dataDir, Assets: assets,
		Version: version.Version, Executable: exe, Reveal: procx.RevealInFileManager,
	})
}
