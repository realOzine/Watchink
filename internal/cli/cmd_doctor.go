package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"watchink/internal/app"
)

// Check statuses of `doctor`.
const (
	checkOK      = "ok"
	checkWarn    = "warn"
	checkFail    = "fail"
	checkSkipped = "skipped"
)

// doctorCheck is one line of the diagnosis.
type doctorCheck struct {
	Name    string         `json:"name"`
	Status  string         `json:"status"`
	Message string         `json:"message"`
	Hint    string         `json:"hint,omitempty"`
	Code    app.Code       `json:"code,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// doctorReport is the --json output of `doctor`. When a required check
// fails the same members are emitted next to "error".
type doctorReport struct {
	OK      bool          `json:"ok"`
	Version string        `json:"version"`
	BuildID string        `json:"build_id"`
	Checks  []doctorCheck `json:"checks"`
}

func failCheck(name string, err error, hint string) doctorCheck {
	ae := app.AsError(err)
	if ae.Hint != "" {
		hint = ae.Hint
	}
	return doctorCheck{Name: name, Status: checkFail, Message: ae.Message, Hint: hint, Code: ae.Code}
}

func (e *env) cmdDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "诊断：数据目录、Engine、工具、模型、在线模型配置",
		Args:  noArgs,
		RunE: e.run(func(c *call) error {
			rep := doctorReport{Version: c.deps.Version, BuildID: c.deps.BuildID}
			add := func(ch doctorCheck) { rep.Checks = append(rep.Checks, ch) }

			// 1. Data directory.
			if dir, err := c.resolveDataDir(); err != nil {
				add(failCheck("data_dir", err, "用 --data-dir 或 `watchink config set-data-dir` 指定一个可用的目录"))
			} else if dir == "" {
				add(doctorCheck{Name: "data_dir", Status: checkOK, Message: "使用默认数据目录"})
			} else {
				add(doctorCheck{Name: "data_dir", Status: checkOK, Message: dir, Details: map[string]any{"path": dir}})
			}

			// 2. Engine; the remaining checks need it.
			r, err := c.connect(false)
			if err != nil {
				add(failCheck("engine", err, "运行 `watchink serve` 查看 Engine 的启动错误"))
				for _, name := range []string{"tools", "models", "llm"} {
					add(doctorCheck{Name: name, Status: checkSkipped, Message: "无法连接 Engine，未检查"})
				}
			} else {
				defer r.Close()
				ctx, cancel := c.reqCtx()
				defer cancel()
				c.doctorEngine(ctx, r, add)
			}

			// WebView prerequisite (§13.4): macOS ships WKWebView with the
			// system, so only the presence of the desktop UI is reported.
			// Linux takes the WebView and the media decoders from system
			// packages (§16.3), which the injected check looks for. Windows
			// needs the WebView2 runtime.
			if c.deps.RunDesktop != nil {
				add(webViewCheck(desktopCheck(c.deps.DesktopPrereqs), c.deps.WebView))
			} else {
				add(doctorCheck{Name: "desktop", Status: checkSkipped, Message: "此构建不包含桌面界面（nogui）"})
			}

			var failed []doctorCheck
			for _, ch := range rep.Checks {
				if ch.Status == checkFail {
					failed = append(failed, ch)
				}
			}
			rep.OK = len(failed) == 0
			if !c.jsonMode {
				printDoctor(c.out, &rep)
			}
			if rep.OK {
				if c.jsonMode {
					return writeJSON(c.out, rep)
				}
				return nil
			}
			names := make([]string, len(failed))
			for i, ch := range failed {
				names[i] = ch.Name
			}
			// The first failed check decides the exit code; checks run in
			// dependency order (engine before tools before configuration).
			first := failed[0]
			ae := app.E(first.Code, "诊断发现 %d 个问题：%s", len(failed), strings.Join(names, "、")).
				WithDetail("failed_checks", names)
			if first.Hint != "" {
				ae = ae.WithHint(first.Hint)
			}
			return &failure{err: ae, exit: app.ExitCode(first.Code), extra: map[string]any{
				"ok": false, "version": rep.Version, "build_id": rep.BuildID, "checks": rep.Checks,
			}}
		}),
	}
}

// SystemPrereq is one system library of the desktop window, as reported by
// Deps.DesktopPrereqs.
type SystemPrereq struct {
	// Name says what the library is for; Package is the distribution
	// package that provides it (Ubuntu 24.04 naming).
	Name    string
	Package string
	// Optional libraries only affect video playback inside the window.
	Optional bool
	Found    bool
}

// desktopCheck reports the desktop UI of a GUI build together with its
// system prerequisites. A missing WebView library fails the check; missing
// media decoders are a warning, because everything but playback still works
// and the browser UI remains available (D-11).
func desktopCheck(prereqs func() []SystemPrereq) doctorCheck {
	ch := doctorCheck{Name: "desktop", Status: checkOK, Message: "此构建包含桌面界面"}
	if prereqs == nil {
		return ch
	}
	list := prereqs()
	if len(list) == 0 {
		return ch
	}
	var required, playback, packages []string
	items := make([]map[string]any, 0, len(list))
	for _, p := range list {
		items = append(items, map[string]any{"name": p.Name, "package": p.Package, "optional": p.Optional, "found": p.Found})
		if p.Found {
			continue
		}
		packages = append(packages, p.Package)
		if p.Optional {
			playback = append(playback, p.Name)
		} else {
			required = append(required, p.Name)
		}
	}
	ch.Details = map[string]any{"prerequisites": items}
	switch {
	case len(required) > 0:
		ch.Status, ch.Code = checkFail, app.CodeToolMissing
		ch.Message = "缺少桌面窗口所需的系统库：" + strings.Join(append(required, playback...), "、")
	case len(playback) > 0:
		ch.Status = checkWarn
		ch.Message = "此构建包含桌面界面，但缺少窗口内播放视频所需的系统库：" + strings.Join(playback, "、")
	default:
		ch.Message += "，所需系统库齐全"
	}
	if len(packages) > 0 {
		ch.Hint = "Ubuntu 24.04 可运行 `sudo apt install " + strings.Join(packages, " ") + "`；也可以用 `watchink web` 在浏览器中使用"
	}
	return ch
}

// webViewCheck adds the state of the system WebView to the desktop check on
// platforms where it is a separate install (Windows: the WebView2 runtime;
// macOS ships WKWebView). A missing runtime is a warning, not a failure:
// everything but the window still works, and the window offers to install it.
func webViewCheck(ch doctorCheck, probe func() (name, version string, required bool)) doctorCheck {
	if probe == nil {
		return ch
	}
	name, version, required := probe()
	if !required {
		return ch
	}
	if ch.Details == nil {
		ch.Details = map[string]any{}
	}
	ch.Details["webview"], ch.Details["webview_version"] = name, version
	if version == "" {
		if ch.Status == checkOK {
			ch.Status = checkWarn
		}
		ch.Message += "，但未检测到 " + name
		ch.Hint = "打开桌面窗口时会提示下载安装；也可以先从 https://developer.microsoft.com/microsoft-edge/webview2/ 安装“常青版”运行时。CLI、MCP 和浏览器界面不受影响"
		return ch
	}
	ch.Message += "；" + name + " " + version
	return ch
}

// doctorEngine runs the checks that need a connected engine.
func (c *call) doctorEngine(ctx context.Context, r app.Remote, add func(doctorCheck)) {
	// Engine reachability and version.
	if info, err := r.EngineInfo(ctx); err != nil {
		add(failCheck("engine", err, ""))
	} else {
		ch := doctorCheck{
			Name:    "engine",
			Status:  checkOK,
			Message: fmt.Sprintf("运行中，版本 %s (build %s)，%s", info.Version, info.BuildID, info.BaseURL),
			Details: map[string]any{
				"version": info.Version, "build_id": info.BuildID, "pid": info.PID,
				"mode": info.Mode, "base_url": info.BaseURL, "data_dir": info.DataDir,
				"active_jobs": info.ActiveJobs,
			},
		}
		if info.BuildID != c.deps.BuildID {
			ch.Status = checkWarn
			ch.Message += "；与本程序的 build 不一致"
			ch.Hint = "任务结束后运行 `watchink engine stop`，下次会自动启动新版本"
		}
		add(ch)
	}

	// Tools and models (§11): only Required items make the check fail.
	if tools, err := r.GetTools(ctx); err != nil {
		add(failCheck("tools", err, ""))
		add(doctorCheck{Name: "models", Status: checkSkipped, Message: "无法读取工具状态，未检查"})
	} else {
		add(toolsCheck("tools", app.ToolKindTool, tools))
		add(toolsCheck("models", app.ToolKindModel, tools))
	}

	// Online model configuration: state only, never the key itself (D-14).
	s, err := r.GetSettings(ctx)
	if err != nil {
		add(failCheck("llm", err, ""))
		return
	}
	llm := s.LLM
	ch := doctorCheck{Name: "llm", Status: checkOK, Details: map[string]any{
		"base_url": llm.BaseURL, "model": llm.Model,
		"api_key_configured": llm.APIKeyConfigured, "api_key_source": llm.APIKeySource,
		"api_key_last4": llm.APIKeyLast4,
	}}
	var missing []string
	if llm.BaseURL == "" {
		missing = append(missing, "base_url")
	}
	if llm.Model == "" {
		missing = append(missing, "model")
	}
	if !llm.APIKeyConfigured && keyRequired(llm.BaseURL) {
		missing = append(missing, "API Key")
	}
	if len(missing) > 0 {
		ch.Status, ch.Code = checkFail, app.CodeModelNotConfigured
		ch.Message = "未配置完整，缺少 " + strings.Join(missing, "、")
		ch.Hint = "运行 `watchink setup --llm-base-url U --llm-model M --llm-api-key-stdin`"
	} else {
		ch.Message = fmt.Sprintf("%s @ %s", llm.Model, llm.BaseURL)
		if llm.APIKeySource != "" {
			ch.Message += "，API Key 来源 " + llm.APIKeySource
		} else {
			ch.Message += "（本机端点，无需 API Key）"
		}
		if llm.APIKeyLast4 != "" {
			ch.Message += "（末四位 " + llm.APIKeyLast4 + "）"
		}
	}
	add(ch)
}

// toolsCheck summarizes the items of one kind.
func toolsCheck(name, kind string, tools *app.ToolsStatus) doctorCheck {
	var ready, missing, optional []string
	items := []map[string]any{}
	for _, it := range tools.Items {
		if it.Kind != kind {
			continue
		}
		items = append(items, map[string]any{"name": it.Name, "status": it.Status, "version": it.Version, "required": it.Required})
		switch {
		case it.Status == app.ToolReady:
			ready = append(ready, it.Name)
		case it.Required:
			missing = append(missing, it.Name)
		default:
			optional = append(optional, it.Name)
		}
	}
	ch := doctorCheck{Name: name, Status: checkOK, Details: map[string]any{"items": items}}
	switch {
	case len(missing) > 0:
		ch.Status, ch.Code = checkFail, app.CodeToolMissing
		ch.Message = "缺少 " + strings.Join(missing, "、")
		ch.Hint = "运行 `watchink tools prepare --wait` 下载"
	case len(ready) == 0 && len(optional) == 0:
		ch.Message = "没有需要的项目"
	default:
		ch.Message = "已就绪：" + strings.Join(ready, "、")
		if len(ready) == 0 {
			ch.Message = "必需项均已就绪"
		}
	}
	if len(optional) > 0 && ch.Status == checkOK {
		ch.Message += "；按需下载：" + strings.Join(optional, "、")
	}
	return ch
}

func printDoctor(w io.Writer, rep *doctorReport) {
	labels := map[string]string{
		"data_dir": "数据目录", "engine": "Engine", "tools": "工具", "models": "语音模型",
		"llm": "在线模型", "desktop": "桌面界面",
	}
	marks := map[string]string{checkOK: "✓", checkWarn: "!", checkFail: "✗", checkSkipped: "-"}
	width := 0
	for _, ch := range rep.Checks {
		width = max(width, displayWidth(labels[ch.Name]))
	}
	fmt.Fprintf(w, "watchink %s (build %s)\n", rep.Version, rep.BuildID)
	for _, ch := range rep.Checks {
		fmt.Fprintf(w, "%s %s  %s\n", marks[ch.Status], pad(labels[ch.Name], width), ch.Message)
		if ch.Hint != "" && ch.Status != checkOK {
			fmt.Fprintf(w, "  %s  提示：%s\n", pad("", width), ch.Hint)
		}
	}
	if rep.OK {
		fmt.Fprintln(w, "一切就绪")
	}
}
