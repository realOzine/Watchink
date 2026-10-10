package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"watchink/internal/app"
)

func printTools(w io.Writer, t *app.ToolsStatus) {
	rows := make([][]string, 0, len(t.Items))
	for _, it := range t.Items {
		kind := "工具"
		if it.Kind == app.ToolKindModel {
			kind = "模型"
		}
		need := "否"
		switch {
		case it.Required:
			need = "是"
		case it.Lazy:
			need = "按需"
		}
		status := statusName(it.Status)
		if it.Status == app.ToolDownloading {
			status += " " + percent(it.Progress)
		}
		size := "-"
		switch {
		case it.SizeBytes > 0:
			size = humanBytes(it.SizeBytes)
		case it.DownloadBytes > 0:
			size = "需下载 " + humanBytes(it.DownloadBytes)
		}
		version := it.Version
		if version == "" {
			version = "-"
		}
		rows = append(rows, []string{it.Name, kind, version, status, need, size})
	}
	table(w, []string{"名称", "类型", "版本", "状态", "必需", "大小"}, rows)
	if t.Ready {
		fmt.Fprintln(w, "必需的工具和模型均已就绪")
	} else {
		fmt.Fprintf(w, "尚未就绪，还需下载约 %s；运行 `watchink tools prepare --wait`\n", humanBytes(t.MissingDownloadBytes))
	}
	for _, it := range t.Items {
		if it.Error != "" {
			fmt.Fprintf(w, "%s：%s\n", it.Name, it.Error)
		}
	}
}

func normalizeTools(t *app.ToolsStatus) *app.ToolsStatus {
	if t.Items == nil {
		t.Items = []app.ToolItem{}
	}
	return t
}

func (e *env) cmdTools() *cobra.Command {
	emitTools := func(c *call, done string, t *app.ToolsStatus) error {
		normalizeTools(t)
		return c.emit(t, func(w io.Writer) {
			if done != "" {
				fmt.Fprintln(w, done)
			}
			printTools(w, t)
		})
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "查看外部工具与模型的状态",
		Args:  noArgs,
		RunE: e.run(func(c *call) error {
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				defer cancel()
				t, err := r.GetTools(ctx)
				if err != nil {
					return err
				}
				return emitTools(c, "", t)
			})
		}),
	}

	var wait bool
	prepare := &cobra.Command{
		Use:   "prepare [名称…]",
		Short: "下载缺失的工具和模型（默认全部必需项）；--wait 等待完成",
		RunE: e.run(func(c *call) error {
			return c.withRemote(func(r app.Remote) error {
				items := c.args
				if items == nil {
					items = []string{}
				}
				ctx, cancel := c.reqCtx()
				job, err := r.PrepareTools(ctx, app.PrepareToolsRequest{Items: items})
				cancel()
				if err != nil {
					return err
				}
				if !wait {
					return c.emit(job, func(w io.Writer) {
						printJob(w, job)
						if !job.Terminal() {
							fmt.Fprintf(w, "查看进度：watchink jobs get %s --follow\n", job.JobID)
						}
					})
				}
				return c.waitAndEmitJob(r, job)
			})
		}),
	}
	prepare.Flags().BoolVar(&wait, "wait", false, "等待准备任务结束，并在 stderr 显示进度")

	update := &cobra.Command{
		Use:   "update yt-dlp",
		Short: "把工具更新到上游最新版本（v0 仅支持 yt-dlp）",
		Args:  exactArgs(1, "tools update yt-dlp"),
		RunE: e.run(func(c *call) error {
			return c.withRemote(func(r app.Remote) error {
				c.logf("正在更新 %s…", c.args[0])
				ctx, cancel := c.reqCtx()
				defer cancel()
				t, err := r.UpdateTool(ctx, c.args[0])
				if err != nil {
					return err
				}
				return emitTools(c, "已更新 "+c.args[0], t)
			})
		}),
	}

	importCmd := func(use, short, usage string) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  exactArgs(2, usage),
			RunE: e.run(func(c *call) error {
				// The engine reads the file itself, from its own working
				// directory: send an absolute path.
				path, err := filepath.Abs(c.args[1])
				if err != nil {
					return app.E(app.CodeInvalidArgument, "无法解析路径：%v", err)
				}
				return c.withRemote(func(r app.Remote) error {
					ctx, cancel := c.reqCtx()
					defer cancel()
					t, err := r.ImportTool(ctx, app.ToolImportRequest{Name: c.args[0], Path: path})
					if err != nil {
						return err
					}
					return emitTools(c, "已导入 "+c.args[0], t)
				})
			}),
		}
	}
	imp := importCmd("import <名称> <文件>", "导入手动下载的工具文件（离线或下载受限时使用）", "tools import <名称> <文件>")
	impModel := importCmd("import-model <模型ID> <文件>", "导入手动下载的模型文件", "tools import-model <模型ID> <文件>")

	return group("tools", "外部工具与模型", status, prepare, update, imp, impModel)
}

// waitAndEmitJob waits for job without a time limit, showing progress on
// stderr, then emits it or the wait failure.
func (c *call) waitAndEmitJob(r app.Remote, job *app.Job) error {
	onUpdate := c.progressPrinter()
	onUpdate(job)
	job, timedOut, err := c.waitJob(r, job, 0, onUpdate)
	if err != nil {
		return err
	}
	if fail := waitFailure(job, timedOut); fail != nil {
		if !c.jsonMode {
			printJob(c.out, job)
		}
		return fail
	}
	return c.emit(job, func(w io.Writer) { printJob(w, job) })
}

func (e *env) cmdLLM() *cobra.Command {
	test := &cobra.Command{
		Use:   "test",
		Short: "用当前配置向在线模型发一次测试请求",
		Args:  noArgs,
		RunE: e.run(func(c *call) error {
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				defer cancel()
				res, err := r.TestLLM(ctx)
				if err != nil {
					return err
				}
				if !res.OK {
					ae := res.Error
					if ae == nil {
						ae = app.E(app.CodeInternal, "在线模型测试失败，但没有返回错误信息")
					}
					return &failure{err: ae, exit: app.ExitCode(ae.Code), extra: map[string]any{"result": res}}
				}
				return c.emit(res, func(w io.Writer) {
					fmt.Fprintln(w, "在线模型可用")
					kv(w,
						[2]string{"模型", res.Model},
						[2]string{"结构化输出", res.StructuredOutput},
						[2]string{"耗时", fmt.Sprintf("%d ms", res.LatencyMs)},
					)
				})
			})
		}),
	}
	models := &cobra.Command{
		Use:   "models",
		Short: "列出当前接口提供的模型（用于选择 llm.model）",
		Args:  noArgs,
		RunE: e.run(func(c *call) error {
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				defer cancel()
				res, err := r.ListLLMModels(ctx, app.LLMModelsRequest{})
				if err != nil {
					return err
				}
				return c.emit(res, func(w io.Writer) {
					for _, m := range res.Models {
						fmt.Fprintln(w, m)
					}
				})
			})
		}),
	}
	return group("llm", "在线模型", test, models)
}
