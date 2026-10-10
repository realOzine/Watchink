//go:build desktop

// Package desktop is the Wails v2 shell: one
// window that loads the embedded SPA and six bindings for native
// capabilities. All business calls go from the WebView straight to the
// engine's HTTP API; this package depends only on internal/app.
//
// The package only builds with the `desktop` tag, and a runnable binary also
// needs Wails' `production` tag and cgo (see scripts/build-macos.sh and
// scripts/build-linux.sh; Linux adds the `webkit2_41` tag). Windows needs no
// cgo (scripts/build-windows.sh).
package desktop

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/wailsapp/wails/v2"
	wailslogger "github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"watchink/internal/app"
)

// Window geometry: the default fits the player + notes layout, the
// minimum keeps both columns usable.
const (
	windowTitle   = "Watchink"
	defaultWidth  = 1280
	defaultHeight = 820
	minWidth      = 960
	minHeight     = 600

	// linuxProgramName is the desktop file ID of build/linux/watchink.desktop.
	linuxProgramName = "watchink"
)

// Options configures Run.
type Options struct {
	// Connect discovers (and by default spawns) the engine; production
	// passes internal/transport/client.Connect. The returned app.Remote must also
	// implement TokenProvider.
	Connect app.Connector
	// DataDir is the --data-dir flag value handed to Connect; may be empty.
	DataDir string
	// Assets is the built SPA rooted at index.html (webui.FS()).
	Assets fs.FS
	// Version is reported by the EngineInfo binding.
	Version string
	// Executable is the absolute path used in the agent setup snippets;
	// empty means os.Executable().
	Executable string
	// Reveal shows a path in the platform file manager. It is injected
	// because only internal/infra/procx may start processes.
	Reveal func(path string) error
	// Logger receives shell and Wails diagnostics; nil discards them.
	Logger *slog.Logger
}

// Run opens the desktop window and blocks until it is closed. It must be
// called from the main goroutine. Closing the window only ends this process;
// the engine keeps running until its jobs finish and it idles out.
func Run(opts Options) error {
	if opts.Assets == nil {
		return errors.New("desktop: Options.Assets is required")
	}
	if opts.Connect == nil {
		return errors.New("desktop: Options.Connect is required")
	}
	if err := preflight(); err != nil {
		return err
	}
	a := newApp(opts)
	a.pickFile = func(ctx context.Context) (string, error) {
		patterns := make([]string, len(VideoExtensions))
		for i, ext := range VideoExtensions {
			patterns[i] = "*." + ext
		}
		return runtime.OpenFileDialog(ctx, runtime.OpenDialogOptions{
			Title:           "选择视频文件",
			ResolvesAliases: true,
			Filters:         []runtime.FileFilter{{DisplayName: "视频文件", Pattern: strings.Join(patterns, ";")}},
		})
	}
	a.saveFile = func(ctx context.Context, defaultName string) (string, error) {
		return runtime.SaveFileDialog(ctx, runtime.SaveDialogOptions{
			Title:                "导出",
			DefaultFilename:      defaultName,
			CanCreateDirectories: true,
		})
	}
	a.openURL = runtime.BrowserOpenURL

	return wails.Run(&options.App{
		Title:     windowTitle,
		Width:     defaultWidth,
		Height:    defaultHeight,
		MinWidth:  minWidth,
		MinHeight: minHeight,
		AssetServer: &assetserver.Options{
			Assets: opts.Assets,
		},
		Bind:           []any{a},
		ErrorFormatter: formatError,
		OnStartup:      a.setContext,
		OnShutdown:     func(context.Context) { a.close() },
		Logger:         wailsLog{a.log},
		// Wails logs binding calls below ERROR; results include the engine
		// token, so nothing below ERROR is emitted in production.
		LogLevelProduction: wailslogger.ERROR,
		Mac: &mac.Options{
			About: &mac.AboutInfo{Title: windowTitle, Message: "版本 " + opts.Version},
			Preferences: &mac.Preferences{
				// The video detail page has a fullscreen player button.
				FullscreenEnabled: mac.Enabled,
			},
		},
		// Each platform ignores the other's options.
		Linux: &linux.Options{
			Icon: windowIcon(),
			// The GLib program name becomes the window's app id / WM_CLASS;
			// it must match build/linux/watchink.desktop for the window to
			// be grouped under that launcher entry.
			ProgramName: linuxProgramName,
			// Wails' own default when no Linux options are given: WebKitGTK
			// GPU compositing renders a blank window with some drivers
			// (wails#2977). Stated here because passing options resets it.
			WebviewGpuPolicy: linux.WebviewGpuPolicyNever,
		},
		Windows: windowsOptions(),
	})
}

// wailsLog adapts slog to the Wails logger interface.
type wailsLog struct{ l *slog.Logger }

func (w wailsLog) Print(m string)   { w.l.Info(m, "source", "wails") }
func (w wailsLog) Trace(m string)   { w.l.Debug(m, "source", "wails") }
func (w wailsLog) Debug(m string)   { w.l.Debug(m, "source", "wails") }
func (w wailsLog) Info(m string)    { w.l.Info(m, "source", "wails") }
func (w wailsLog) Warning(m string) { w.l.Warn(m, "source", "wails") }
func (w wailsLog) Error(m string)   { w.l.Error(m, "source", "wails") }
func (w wailsLog) Fatal(m string)   { w.l.Error(m, "source", "wails", "fatal", true) }
