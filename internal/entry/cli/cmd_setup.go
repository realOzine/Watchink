package cli

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"watchink/internal/app"
)

// setupResult is the --json output of `setup`.
type setupResult struct {
	LLMConfigured bool             `json:"llm_configured"`
	ToolsReady    bool             `json:"tools_ready"`
	Settings      *app.Settings    `json:"settings"`
	Tools         *app.ToolsStatus `json:"tools"`
	PrepareJob    *app.Job         `json:"prepare_job"`
}

func llmConfigured(s *app.Settings) bool {
	return s.LLM.BaseURL != "" && s.LLM.Model != "" && (s.LLM.APIKeyConfigured || !keyRequired(s.LLM.BaseURL))
}

// keyRequired reports whether an endpoint needs an API key: a loopback
// endpoint (a local OpenAI-compatible server) works without one, matching
// the engine's own rule.
func keyRequired(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return true
	}
	host := u.Hostname()
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// setupInput is what `setup` is going to apply: the settings patch, the API
// key (empty = leave as is) and whether to download the missing tools. It
// starts from the flags and is completed by the interactive questions.
type setupInput struct {
	llm, asr map[string]any
	apiKey   string
	prepare  bool
}

func (e *env) cmdSetup() *cobra.Command {
	var baseURL, model, asrModel string
	var keyStdin, prepare bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "首次配置：在线模型、语音模型、工具下载",
		Long: "首次配置。所有项都可以用参数给出；只有在交互式终端且未指定 --json 时才会提问。\n" +
			"API Key 通过 --llm-api-key-stdin 从 stdin 读取，例如：\n" +
			"  printf %s \"$KEY\" | watchink setup --llm-base-url https://api.example.com/v1 --llm-model m --llm-api-key-stdin --prepare",
		Args: noArgs,
	}
	cmd.RunE = e.run(func(c *call) error {
		flags := cmd.Flags()
		in := setupInput{llm: map[string]any{}, asr: map[string]any{}, prepare: prepare}
		if flags.Changed("llm-base-url") {
			in.llm["base_url"] = baseURL
		}
		if flags.Changed("llm-model") {
			in.llm["model"] = model
		}
		if flags.Changed("asr-model") {
			in.asr["model"] = asrModel
		}
		if keyStdin {
			key, err := c.pipedAPIKey()
			if err != nil {
				return err
			}
			in.apiKey = key
		}
		// Questions are only asked on an interactive terminal without
		// --json; stdin is not a prompt source when it carries the API key.
		interactive := c.isTerminal() && !c.jsonMode && !keyStdin

		return c.withRemote(func(r app.Remote) error {
			if interactive {
				if err := c.askSetup(r, &in); err != nil {
					return err
				}
			}
			if err := c.saveSetup(r, in); err != nil {
				return err
			}
			res := setupResult{}
			var fail *failure
			if in.prepare {
				job, timedOut, err := c.prepareAndWait(r)
				if err != nil {
					return err
				}
				res.PrepareJob = job
				fail = waitFailure(job, timedOut)
			}

			ctx, cancel := c.reqCtx()
			defer cancel()
			var err error
			if res.Settings, err = r.GetSettings(ctx); err != nil {
				return err
			}
			if res.Tools, err = r.GetTools(ctx); err != nil {
				return err
			}
			normalizeTools(res.Tools)
			res.LLMConfigured = llmConfigured(res.Settings)
			res.ToolsReady = res.Tools.Ready

			if fail != nil {
				if !c.jsonMode {
					printJob(c.out, res.PrepareJob)
				}
				return fail
			}
			return c.emit(res, func(w io.Writer) { printSetup(w, &res) })
		})
	})
	f := cmd.Flags()
	f.StringVar(&baseURL, "llm-base-url", "", "在线模型接口地址（OpenAI 兼容），如 https://api.example.com/v1")
	f.StringVar(&model, "llm-model", "", "在线模型名")
	f.BoolVar(&keyStdin, "llm-api-key-stdin", false, "从 stdin 读取 API Key（不回显）")
	f.StringVar(&asrModel, "asr-model", "", "语音模型 ID（留空使用平台默认模型）")
	f.BoolVar(&prepare, "prepare", false, "下载缺失的工具和模型并等待完成")
	return cmd
}

// pipedAPIKey reads the API key from stdin for --llm-api-key-stdin.
func (c *call) pipedAPIKey() (string, error) {
	if c.isTerminal() {
		return "", app.E(app.CodeInvalidArgument, "--llm-api-key-stdin 需要通过管道传入 API Key").
			WithHint("例如：printf %s \"$KEY\" | watchink setup --llm-api-key-stdin")
	}
	v, err := c.readSecret("")
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", app.E(app.CodeInvalidArgument, "没有从 stdin 读到 API Key")
	}
	return v, nil
}

// askSetup asks on the terminal for whatever the flags did not give: the
// model address and name, the API key, and whether to download the missing
// tools. An empty answer keeps the current value.
func (c *call) askSetup(r app.Remote, in *setupInput) error {
	ctx, cancel := c.reqCtx()
	current, err := r.GetSettings(ctx)
	cancel()
	if err != nil {
		return err
	}
	stdin := bufio.NewReader(c.stdin())
	ask := func(label, cur string) string {
		if cur != "" {
			fmt.Fprintf(c.errw, "%s [%s]：", label, cur)
		} else {
			fmt.Fprintf(c.errw, "%s：", label)
		}
		line, _ := stdin.ReadString('\n')
		return strings.TrimSpace(line)
	}

	fmt.Fprintln(c.errw, "配置在线模型（OpenAI 兼容接口）。直接回车表示保持当前值或跳过。")
	if _, given := in.llm["base_url"]; !given {
		if v := ask("接口地址 base_url", current.LLM.BaseURL); v != "" {
			in.llm["base_url"] = v
		}
	}
	if _, given := in.llm["model"]; !given {
		if v := ask("模型名", current.LLM.Model); v != "" {
			in.llm["model"] = v
		}
	}
	state := "未配置"
	if current.LLM.APIKeyConfigured {
		state = "已配置，回车保持不变"
	}
	if c.deps.ReadPassword != nil {
		fmt.Fprintf(c.errw, "API Key（%s）：", state)
		v, err := c.deps.ReadPassword()
		fmt.Fprintln(c.errw)
		if err != nil {
			return app.E(app.CodeInternal, "读取输入失败：%v", err)
		}
		in.apiKey = strings.TrimSpace(v)
	} else {
		in.apiKey = ask("API Key（"+state+"；输入内容会显示在屏幕上）", "")
	}

	if in.prepare {
		return nil
	}
	ctx, cancel = c.reqCtx()
	tools, err := r.GetTools(ctx)
	cancel()
	if err != nil {
		return err
	}
	if !tools.Ready {
		v := ask(fmt.Sprintf("现在下载缺失的工具和模型（约 %s）吗？[Y/n]", humanBytes(tools.MissingDownloadBytes)), "")
		in.prepare = v == "" || strings.EqualFold(v, "y") || strings.EqualFold(v, "yes")
	}
	return nil
}

// saveSetup writes the settings patch and the API key, when there are any.
func (c *call) saveSetup(r app.Remote, in setupInput) error {
	patch := map[string]any{}
	if len(in.llm) > 0 {
		patch["llm"] = in.llm
	}
	if len(in.asr) > 0 {
		patch["asr"] = in.asr
	}
	if len(patch) > 0 {
		ctx, cancel := c.reqCtx()
		_, err := r.PatchSettings(ctx, patch)
		cancel()
		if err != nil {
			return err
		}
		c.logf("已保存配置")
	}
	if in.apiKey != "" {
		ctx, cancel := c.reqCtx()
		err := r.SetSecret(ctx, app.SecretLLMAPIKey, in.apiKey)
		cancel()
		if err != nil {
			return err
		}
		c.logf("已保存 API Key")
	}
	return nil
}

// prepareAndWait starts the download of the missing tools and models and
// follows the job to its end.
func (c *call) prepareAndWait(r app.Remote) (job *app.Job, timedOut bool, err error) {
	ctx, cancel := c.reqCtx()
	job, err = r.PrepareTools(ctx, app.PrepareToolsRequest{Items: []string{}})
	cancel()
	if err != nil {
		return nil, false, err
	}
	c.logf("正在准备工具和模型…")
	onUpdate := c.progressPrinter()
	onUpdate(job)
	return c.waitJob(r, job, 0, onUpdate)
}

func printSetup(w io.Writer, res *setupResult) {
	llm := res.Settings.LLM
	key := "未配置"
	if llm.APIKeyConfigured {
		key = "已配置"
		if llm.APIKeyLast4 != "" {
			key += "（末四位 " + llm.APIKeyLast4 + "）"
		}
	}
	orDash := func(s string) string {
		if s == "" {
			return "（未设置）"
		}
		return s
	}
	asr := res.Settings.ASR.Model
	if asr == "" {
		asr = res.Tools.DefaultASRModel + "（默认）"
	}
	ready := "未就绪"
	if res.ToolsReady {
		ready = "已就绪"
	}
	kv(w,
		[2]string{"在线模型接口", orDash(llm.BaseURL)},
		[2]string{"在线模型", orDash(llm.Model)},
		[2]string{"API Key", key},
		[2]string{"语音模型", asr},
		[2]string{"工具与模型", ready},
	)
	if !res.LLMConfigured {
		fmt.Fprintln(w, "在线模型尚未配置完整：用 --llm-base-url、--llm-model、--llm-api-key-stdin 补全后才能生成笔记。")
	}
	if !res.ToolsReady {
		fmt.Fprintln(w, "工具或模型尚未就绪：运行 `watchink setup --prepare` 或 `watchink tools prepare --wait`。")
	}
	if res.LLMConfigured && res.ToolsReady {
		fmt.Fprintln(w, "配置完成。导入视频：watchink import <路径或URL> --wait")
	}
}
