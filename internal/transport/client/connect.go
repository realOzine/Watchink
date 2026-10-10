package client

import (
	"context"
	"os"
	"time"

	"watchink/internal/app"
	"watchink/internal/engine"
	"watchink/internal/infra/config"
	"watchink/internal/version"
)

// Spawner starts `<exe> <args…>` as a detached process with the given
// environment and returns without waiting for it. Only internal/infra/procx may
// create processes, so the binary wires this to procx.SpawnDetached;
// while it is nil, Connect cannot start an engine and reports
// ENGINE_UNAVAILABLE.
var Spawner func(exe string, args []string, env []string) error

// buildID is the build this client belongs to; tests override it.
var buildID = func() string { return version.BuildID }

const (
	defaultConnectTimeout = 15 * time.Second
	pollInterval          = 100 * time.Millisecond
	probeTimeout          = 2 * time.Second
)

var _ app.Connector = Connect

// Connect finds the engine of the data directory, starting one when needed
// :
//
//  1. engine.json + GET /api/v1/engine with its token; same build_id → done.
//  2. Different build_id: an idle engine is asked to shut down and replaced;
//     a busy one yields ENGINE_VERSION_MISMATCH.
//  3. Not reachable: spawn `<exe> engine run --data-dir <dir>` detached and
//     poll every 100 ms until the timeout (15 s by default).
//  4. Concurrent cold starts converge: only one spawned engine gets the
//     lock, and every client finds that one while polling.
//  5. NoSpawn: ENGINE_UNAVAILABLE instead of step 3.
func Connect(ctx context.Context, opts app.ConnectOptions) (app.Remote, error) {
	dataDir, err := config.ResolveDataDir(opts.DataDir)
	if err != nil {
		return nil, app.E(app.CodeInvalidArgument, "无法确定数据目录：%v", err).WithCause(err)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	spawned, replaced := false, false
	for {
		c, info := probe(ctx, dataDir, opts.Requester)
		switch {
		case c != nil && (info.BuildID == buildID() || opts.AnyBuild):
			return c, nil

		case c != nil:
			// A reachable engine of another build (step 2).
			mismatch := app.E(app.CodeEngineVersionMismatch,
				"正在运行的 Engine 版本（%s）与当前程序（%s）不一致", info.BuildID, buildID()).
				WithDetail("engine_build_id", info.BuildID).
				WithDetail("client_build_id", buildID()).
				WithDetail("active_jobs", info.ActiveJobs)
			if info.ActiveJobs > 0 {
				c.Close()
				return nil, mismatch.WithHint("Engine 仍有任务在运行：请等待任务完成后重试，或执行 `watchink engine stop --force` 后重试。")
			}
			if opts.NoSpawn || replaced || spawned {
				// Either we may not start a replacement, or the engine that
				// came up after our own attempt is again of another build.
				c.Close()
				return nil, mismatch.WithHint("请执行 `watchink engine stop` 后重试，并确认只安装了一个版本的 watchink。")
			}
			err := c.Shutdown(ctx, false)
			c.Close()
			if err != nil && !app.IsCode(err, app.CodeEngineUnavailable) {
				return nil, err
			}
			if err := waitStopped(ctx, dataDir); err != nil {
				return nil, app.E(app.CodeEngineUnavailable, "等待旧版本 Engine 退出超时").
					WithHint("请执行 `watchink engine stop --force` 后重试。").WithCause(err)
			}
			replaced = true
			continue
		}

		// Not reachable (steps 3–5).
		if opts.NoSpawn {
			return nil, app.E(app.CodeEngineUnavailable, "Engine 未运行（已指定 --no-spawn）").
				WithHint("去掉 --no-spawn，或先运行 `watchink serve`。")
		}
		if !spawned {
			if err := spawn(dataDir); err != nil {
				return nil, err
			}
			spawned = true
		}
		select {
		case <-ctx.Done():
			if perr := context.Cause(ctx); perr != context.DeadlineExceeded {
				return nil, ctx.Err()
			}
			return nil, app.E(app.CodeEngineUnavailable, "Engine 在 %s 内没有就绪", timeout).
				WithHint("请查看日志：" + engine.LogPath(dataDir))
		case <-time.After(pollInterval):
		}
	}
}

// probe reads engine.json and asks that engine who it is. A stale file, a
// refused connection or a rejected token (the port now belongs to another
// engine) all mean "no engine reachable".
func probe(ctx context.Context, dataDir, requester string) (*Client, *app.EngineInfo) {
	d, err := engine.ReadDiscovery(dataDir)
	if err != nil {
		return nil, nil
	}
	c := New(Options{BaseURL: d.BaseURL, Token: d.Token, Requester: requester})
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	info, err := c.EngineInfo(pctx)
	if err != nil {
		c.Close()
		return nil, nil
	}
	return c, info
}

func spawn(dataDir string) error {
	if Spawner == nil {
		return app.E(app.CodeEngineUnavailable, "Engine 未运行，且当前程序未配置拉起 Engine 的方式").
			WithHint("请先运行 `watchink serve`，或在另一个终端运行 `watchink engine run`。")
	}
	exe, err := os.Executable()
	if err != nil {
		return app.E(app.CodeEngineUnavailable, "无法确定可执行文件路径：%v", err).WithCause(err)
	}
	args := []string{"engine", "run", "--data-dir", dataDir}
	if err := Spawner(exe, args, os.Environ()); err != nil {
		return app.E(app.CodeEngineUnavailable, "无法启动 Engine：%v", err).
			WithHint("请查看日志：" + engine.LogPath(dataDir)).WithCause(err)
	}
	return nil
}

// waitStopped waits until no process holds the engine lock.
func waitStopped(ctx context.Context, dataDir string) error {
	for engine.IsRunning(dataDir) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval / 2):
		}
	}
	return nil
}
