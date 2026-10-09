// Command watchink is the single executable behind every run mode
// (docs/v0.md D-02, §4.2): desktop window, foreground engine (`serve`),
// background engine (`engine run`), MCP stdio server and the CLI client.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"watchink/internal/app"
	"watchink/internal/cli"
	"watchink/internal/client"
	"watchink/internal/config"
	"watchink/internal/engine"
	"watchink/internal/mcpserver"
	"watchink/internal/procx"
	"watchink/internal/service"
	"watchink/internal/version"
	"watchink/internal/webui"
)

func main() {
	// The guard wraps child tools on macOS (§5.5); it must start without
	// touching anything else.
	if len(os.Args) > 1 && os.Args[1] == procx.GuardSubcommand {
		os.Exit(procx.GuardMain(os.Args[2:]))
	}
	client.Spawner = procx.SpawnDetached

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps := cli.Deps{
		Connect:    client.Connect,
		RunDesktop: runDesktop, // nil in nogui builds
		RunServe: func(ctx context.Context, o cli.ServeOptions) error {
			return runEngine(ctx, o.DataDir, engine.ModeForeground, o.Port, o.Open)
		},
		RunEngine: func(ctx context.Context, dataDir string) error {
			return runEngine(ctx, dataDir, engine.ModeBackground, 0, false)
		},
		RunMCP: func(ctx context.Context, o cli.MCPOptions) error {
			return mcpserver.Run(ctx, mcpserver.Options{Connect: client.Connect, DataDir: o.DataDir, NoSpawn: o.NoSpawn, Version: version.Version})
		},
		Guard:           procx.GuardMain,
		OpenBrowser:     procx.OpenBrowser,
		ResolveDataDir:  config.ResolveDataDir,
		SetDataDir:      config.WriteLocation,
		IsEngineRunning: engine.IsRunning,
		DesktopPrereqs:  desktopPrereqs, // nil in nogui builds
		Stdin:           os.Stdin,
		IsTerminal: func() bool {
			fi, err := os.Stdin.Stat()
			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		},
	}
	code := cli.Execute(ctx, deps, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// runEngine runs the engine of dataDir in this process until it is asked to
// stop (signal, shutdown request or idle exit).
func runEngine(ctx context.Context, dataDir, mode string, port int, open bool) error {
	guard := ""
	if runtime.GOOS == "darwin" {
		// §5.5: on macOS children are wrapped by `watchink _guard`.
		guard, _ = os.Executable()
	}
	err := engine.Run(ctx, engine.Options{
		DataDir: dataDir, Mode: mode, Port: port,
		Version: version.Version, BuildID: version.BuildID,
		Stderr: os.Stderr, NoSignals: true,
		Build: func(ctx context.Context, env engine.Env) (*engine.Built, error) {
			svc, err := service.Open(ctx, service.Options{DataDir: env.DataDir, Logger: env.Logger, AddSecret: env.AddSecret, GuardExe: guard})
			if err != nil {
				return nil, err
			}
			assets, _ := webui.FS()
			return &engine.Built{
				Service: svc, Port: svc.Port(), IdleExit: svc.IdleExit(), WebUI: assets,
				SetBaseURL: svc.SetBaseURL, Start: svc.Start, Close: svc.Close,
			}, nil
		},
		OnReady: func(d engine.Discovery) {
			if mode != engine.ModeForeground {
				return
			}
			fmt.Fprintf(os.Stderr, "Watchink Engine 已启动：%s\n", d.BaseURL)
			if open {
				// --open is an explicit request for the browser UI, so it
				// also switches the optional Web UI on.
				r := client.NewWithToken(d.BaseURL, d.Token)
				if _, err := r.PatchSettings(ctx, map[string]any{"web": map[string]any{"enabled": true}}); err == nil {
					_ = procx.OpenBrowser(d.BaseURL)
				}
			} else {
				fmt.Fprintln(os.Stderr, "浏览器界面默认关闭：运行 `watchink web` 开启并打开，或在桌面应用的设置中开启。")
			}
		},
	})
	if errors.Is(err, engine.ErrAlreadyRunning) {
		if mode == engine.ModeBackground {
			// Lost the cold-start race (§5.2 step 4): the winner serves.
			return nil
		}
		return app.E(app.CodeConflict, "该数据目录已有 Engine 在运行").WithHint("直接使用 `watchink web`，或先执行 `watchink engine stop`")
	}
	return err
}
