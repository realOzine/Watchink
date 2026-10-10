package main

import (
	"context"

	"github.com/spf13/cobra"

	"watchink/internal/app"
	"watchink/internal/entry/cli"
	"watchink/internal/entry/mcpserver"
	"watchink/internal/transport/client"
	"watchink/internal/transport/discovery"
	"watchink/internal/version"
)

// One executable, several roles. Which one a process takes is decided by
// its first argument:
//
//	(none)        the desktop window, when this build has one
//	desktop       the desktop window
//	serve         an engine in the foreground
//	engine run    an engine in the background, started by a client
//	mcp           the MCP stdio server
//	_guard        the child-process guard (handled first thing in main)
//	anything else a CLI client of the engine (internal/entry/cli)

// roles returns the commands that make this process something other than a
// client. They are attached to the CLI's command tree so that they share its
// global flags and error output.
func roles() []cli.Launch {
	var port int
	var open bool
	return []cli.Launch{
		{
			Use: "desktop", Short: "打开桌面窗口",
			Run: openDesktop,
		},
		{
			Use: "serve", Short: "前台运行 Engine 并打印 Web 地址（不会因空闲退出）",
			Setup: func(cmd *cobra.Command) {
				cmd.Flags().IntVar(&port, "port", 0, "监听端口（默认取配置 engine.port）")
				cmd.Flags().BoolVar(&open, "open", false, "启动后在默认浏览器中打开 Web")
			},
			Run: func(ctx context.Context, g cli.Globals) error {
				return runEngine(ctx, g.DataDir, discovery.ModeForeground, port, open)
			},
		},
		{
			// Clients start this one detached; nobody types it.
			Parent: "engine", Use: "run", Short: "内部命令：后台 Engine", Hidden: true,
			Run: func(ctx context.Context, g cli.Globals) error {
				return runEngine(ctx, g.DataDir, discovery.ModeBackground, 0, false)
			},
		},
		{
			Use: "mcp", Short: "运行 MCP stdio 服务器（供 agent 客户端接入）",
			// stdout belongs to the MCP protocol: nothing else is printed.
			Run: func(ctx context.Context, g cli.Globals) error {
				return mcpserver.Run(ctx, mcpserver.Options{Connect: client.Connect, DataDir: g.DataDir, NoSpawn: g.NoSpawn, Version: version.Version})
			},
		},
	}
}

// defaultRole is what the program does without a subcommand: open the
// window, or (nil) print the help text in a build without one.
func defaultRole() func(context.Context, cli.Globals) error {
	if runDesktop == nil {
		return nil
	}
	return openDesktop
}

// openDesktop opens the window. runDesktop is nil in builds without the
// desktop tag (see desktop_nogui.go).
func openDesktop(ctx context.Context, g cli.Globals) error {
	if runDesktop == nil {
		return app.E(app.CodeInvalidArgument, "此构建不包含桌面界面").
			WithHint("改用 `watchink web` 在浏览器中打开，或使用其他子命令")
	}
	return runDesktop(ctx, g.DataDir)
}
