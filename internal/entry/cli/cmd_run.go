package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"watchink/internal/app"
)

// webResult is the --json output of `web`.
type webResult struct {
	URL    string `json:"url"`
	Opened bool   `json:"opened"`
	// Enabled is true when this call switched the Web UI on.
	Enabled bool `json:"enabled"`
}

func (e *env) cmdWeb() *cobra.Command {
	return &cobra.Command{
		Use:   "web",
		Short: "开启 Web 界面，并在默认浏览器中打开",
		Long: "确保 Engine 在运行，开启可选的 Web 界面（设置项 web.enabled）并在默认浏览器中打开。\n" +
			"Web 界面只在本机可访问，没有登录；关闭它：watchink config set web.enabled false",
		Args: noArgs,
		RunE: e.run(func(c *call) error {
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				defer cancel()
				cur, err := r.GetSettings(ctx)
				if err != nil {
					return err
				}
				res := webResult{URL: r.BaseURL()}
				if !cur.Web.Enabled {
					// Asking for the browser UI is the consent to switch
					// the optional Web UI on.
					if _, err := r.PatchSettings(ctx, map[string]any{"web": map[string]any{"enabled": true}}); err != nil {
						return err
					}
					res.Enabled = true
				}
				if c.deps.OpenBrowser != nil {
					if err := c.deps.OpenBrowser(res.URL); err != nil {
						c.logf("无法自动打开浏览器：%v", err)
					} else {
						res.Opened = true
					}
				}
				return c.emit(res, func(w io.Writer) {
					if res.Enabled {
						fmt.Fprintln(w, "已开启 Web 界面（关闭：watchink config set web.enabled false）")
					}
					if res.Opened {
						fmt.Fprintf(w, "已在浏览器中打开 %s\n", res.URL)
						return
					}
					fmt.Fprintf(w, "请用浏览器打开：%s\n", res.URL)
				})
			})
		}),
	}
}

// engineStatus is the --json output of `engine status`.
type engineStatus struct {
	Running bool            `json:"running"`
	DataDir string          `json:"data_dir,omitempty"`
	Engine  *app.EngineInfo `json:"engine"`
}

// engineStopResult is the --json output of `engine stop`.
type engineStopResult struct {
	Stopped    bool `json:"stopped"`
	WasRunning bool `json:"was_running"`
	Force      bool `json:"force"`
}

func (e *env) cmdEngine() *cobra.Command {
	status := &cobra.Command{
		Use:   "status",
		Short: "查看 Engine 是否在运行（不会拉起 Engine）",
		Args:  noArgs,
		RunE: e.run(func(c *call) error {
			res := engineStatus{}
			if dir, err := c.resolveDataDir(); err == nil {
				res.DataDir = dir
			}
			r, err := c.connect(true)
			switch {
			case app.IsCode(err, app.CodeEngineUnavailable):
				// "Not running" is an answer, not a failure.
			case err != nil:
				return err
			default:
				defer r.Close()
				ctx, cancel := c.reqCtx()
				defer cancel()
				info, err := r.EngineInfo(ctx)
				if err != nil {
					return err
				}
				res.Running, res.Engine = true, info
				if info.DataDir != "" {
					res.DataDir = info.DataDir
				}
			}
			return c.emit(res, func(w io.Writer) {
				if !res.Running {
					fmt.Fprintln(w, "Engine 未运行")
					if res.DataDir != "" {
						kv(w, [2]string{"数据目录", res.DataDir})
					}
					return
				}
				i := res.Engine
				fmt.Fprintln(w, "Engine 正在运行")
				kv(w,
					[2]string{"版本", fmt.Sprintf("%s (build %s)", i.Version, i.BuildID)},
					[2]string{"进程", fmt.Sprint(i.PID)},
					[2]string{"模式", i.Mode},
					[2]string{"地址", i.BaseURL},
					[2]string{"数据目录", i.DataDir},
					[2]string{"启动时间", localTime(i.StartedAt)},
					[2]string{"活动任务", fmt.Sprint(i.ActiveJobs)},
					[2]string{"工具就绪", yesNo(i.ToolsReady)},
					[2]string{"在线模型已配置", yesNo(i.LLMConfigured)},
				)
			})
		}),
	}

	var force bool
	stop := &cobra.Command{
		Use:   "stop",
		Short: "停止 Engine；被打断的任务下次启动时自动续跑",
		Args:  noArgs,
		RunE: e.run(func(c *call) error {
			res := engineStopResult{Stopped: true, Force: force}
			r, err := c.connect(true)
			switch {
			case app.IsCode(err, app.CodeEngineUnavailable):
				// Already stopped: stopping is idempotent.
			case err != nil:
				return err
			default:
				defer r.Close()
				ctx, cancel := c.reqCtx()
				defer cancel()
				if err := r.Shutdown(ctx, force); err != nil {
					return err
				}
				res.WasRunning = true
			}
			return c.emit(res, func(w io.Writer) {
				if res.WasRunning {
					fmt.Fprintln(w, "已请求 Engine 停止")
				} else {
					fmt.Fprintln(w, "Engine 未运行")
				}
			})
		}),
	}
	stop.Flags().BoolVar(&force, "force", false, "立即终止子进程，不等待阶段到达可中断点")

	return group("engine", "Engine 管理", status, stop)
}

// versionInfo is the --json output of `version`.
type versionInfo struct {
	Version string `json:"version"`
	BuildID string `json:"build_id"`
	API     int    `json:"api"`
	Desktop bool   `json:"desktop"`
}

func (e *env) cmdVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "显示版本与 build_id",
		Args:  noArgs,
		RunE: e.run(func(c *call) error {
			v := versionInfo{Version: c.deps.Version, BuildID: c.deps.BuildID, API: app.APIVersion, Desktop: c.deps.HasDesktop}
			return c.emit(v, func(w io.Writer) {
				fmt.Fprintf(w, "watchink %s (build %s)\n", v.Version, v.BuildID)
			})
		}),
	}
}
