package cli

import (
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"watchink/internal/app"
)

// importResult is the --json output of `import`: the import response, plus
// the final job when --wait was used.
type importResult struct {
	*app.ImportResponse
	Job *app.Job `json:"job,omitempty"`
}

// importSource classifies the positional argument of `import`: anything that
// parses as an http(s) URL is a URL source; everything else is a local path,
// made absolute here because the engine has a different working directory.
func importSource(arg string) (app.ImportSource, error) {
	if arg == "" {
		return app.ImportSource{}, app.E(app.CodeInvalidArgument, "缺少要导入的路径或链接")
	}
	if u, err := url.Parse(arg); err == nil && u.Host != "" {
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			return app.ImportSource{Type: app.SourceURL, URL: arg}, nil
		}
	}
	if strings.Contains(arg, "://") {
		return app.ImportSource{}, app.E(app.CodeUnsupportedSource, "不支持的链接：只接受 http / https 链接或本地路径")
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return app.ImportSource{}, app.E(app.CodeInvalidArgument, "无法解析路径：%v", err)
	}
	return app.ImportSource{Type: app.SourceFile, Path: abs}, nil
}

// splitWaitArg handles `--wait [seconds]`: pflag cannot attach an optional
// value given as a separate token, so `import --wait 600 a.mp4` arrives as
// two positional arguments. The all-digit one is the timeout.
func splitWaitArg(args []string, waitSet bool, waitVal string) (rest []string, seconds int, err error) {
	if waitVal != "" {
		n, perr := strconv.Atoi(waitVal)
		if perr != nil || n < 0 {
			return nil, 0, app.E(app.CodeInvalidArgument, "--wait 的秒数必须是非负整数")
		}
		seconds = n
	}
	if waitSet && waitVal == "" && len(args) == 2 {
		for i, a := range args {
			if n, perr := strconv.Atoi(a); perr == nil && n >= 0 {
				return []string{args[1-i]}, n, nil
			}
		}
	}
	return args, seconds, nil
}

func (e *env) cmdImport() *cobra.Command {
	var lang, reprocess, wait string
	cmd := &cobra.Command{
		Use:   "import <路径或URL>",
		Short: "导入本地视频或在线视频，立即返回任务 ID",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return app.E(app.CodeInvalidArgument, "需要一个路径或链接").
					WithHint("用法：watchink import <路径或URL> [--lang auto|zh|en] [--reprocess-from STAGE] [--wait [秒]]")
			}
			return nil
		},
	}
	cmd.RunE = e.run(func(c *call) error {
		waitSet := cmd.Flags().Changed("wait")
		args, waitSeconds, err := splitWaitArg(c.args, waitSet, strings.TrimSpace(wait))
		if err != nil {
			return err
		}
		if len(args) != 1 {
			return app.E(app.CodeInvalidArgument, "只能导入一个路径或链接")
		}
		if lang != "" {
			if err := oneOf("lang", lang, "auto", "zh", "en"); err != nil {
				return err
			}
		}
		if reprocess != "" {
			if err := oneOf("reprocess-from", reprocess, "acquire", "transcribe", "note"); err != nil {
				return err
			}
		}
		src, err := importSource(args[0])
		if err != nil {
			return err
		}
		return c.withRemote(func(r app.Remote) error {
			ctx, cancel := c.reqCtx()
			resp, err := r.Import(ctx, app.ImportRequest{Source: src, Language: lang, ReprocessFrom: reprocess, RequestedBy: requester})
			cancel()
			if err != nil {
				return err
			}
			if resp.Warnings == nil {
				resp.Warnings = []string{}
			}
			res := importResult{ImportResponse: resp}
			for _, w := range resp.Warnings {
				if w == app.WarnModelNotConfigured {
					c.logf("警告：尚未配置在线模型，任务会在生成笔记时失败；先运行 `watchink setup` 完成配置")
				} else {
					c.logf("警告：%s", w)
				}
			}

			var fail *failure
			// already_processed is success and has nothing to wait for.
			if waitSet && resp.Status != app.ImportStatusAlreadyProcessed && resp.JobID != "" {
				ctx, cancel := c.reqCtx()
				job, err := r.GetJob(ctx, app.GetJobRequest{JobID: resp.JobID})
				cancel()
				if err != nil {
					return err
				}
				onUpdate := c.progressPrinter()
				onUpdate(job)
				job, timedOut, err := c.waitJob(r, job, time.Duration(waitSeconds)*time.Second, onUpdate)
				if err != nil {
					return err
				}
				res.Job = job
				fail = waitFailure(job, timedOut)
			}
			if fail != nil {
				if !c.jsonMode {
					printJob(c.out, res.Job)
				}
				return fail
			}
			return c.emit(res, func(w io.Writer) {
				if res.Job != nil {
					printJob(w, res.Job)
					fmt.Fprintf(w, "读取笔记：watchink note get %s\n", resp.VideoID)
					return
				}
				pairs := [][2]string{{"视频", resp.VideoID}}
				if resp.JobID != "" {
					pairs = append(pairs, [2]string{"任务", resp.JobID})
				}
				pairs = append(pairs, [2]string{"状态", fmt.Sprintf("%s (%s)", statusName(resp.Status), resp.Status)})
				if resp.NoteID != nil {
					pairs = append(pairs, [2]string{"笔记", *resp.NoteID})
				}
				kv(w, pairs...)
				switch {
				case resp.Status == app.ImportStatusAlreadyProcessed:
					fmt.Fprintf(w, "该视频已处理过。读取笔记：watchink note get %s\n", resp.VideoID)
				case resp.Status == app.JobFailed || resp.Status == app.JobCancelled:
					fmt.Fprintf(w, "该视频上一次任务未成功，不会自动重试。重试：watchink jobs retry %s\n", resp.JobID)
				default:
					if resp.Deduplicated {
						fmt.Fprintln(w, "该视频已有进行中的任务，已复用。")
					}
					fmt.Fprintf(w, "查看进度：watchink jobs get %s --follow\n", resp.JobID)
				}
			})
		})
	})
	f := cmd.Flags()
	f.StringVar(&lang, "lang", "", "语言：auto / zh / en（默认取配置 asr.language）")
	f.StringVar(&reprocess, "reprocess-from", "", "从指定阶段起重新处理：acquire / transcribe / note")
	f.StringVar(&wait, "wait", "", "等待任务结束；可带超时秒数（0 或省略表示不限时）")
	f.Lookup("wait").NoOptDefVal = " "
	return cmd
}

func jobTable(w io.Writer, jobs []app.Job) {
	rows := make([][]string, 0, len(jobs))
	for _, j := range jobs {
		stage := j.Stage
		if stage == "" {
			stage = "-"
		}
		rows = append(rows, []string{j.JobID, j.Kind, statusName(j.Status), stage, percent(j.Progress), deref(j.VideoID), localTime(j.CreatedAt)})
	}
	table(w, []string{"任务", "类型", "状态", "阶段", "进度", "视频", "创建时间"}, rows)
}

func (e *env) cmdJobs() *cobra.Command {
	var listReq app.ListJobsRequest
	list := &cobra.Command{
		Use:   "list",
		Short: "列出任务",
		Args:  noArgs,
		RunE: e.run(func(c *call) error {
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				defer cancel()
				res, err := r.ListJobs(ctx, listReq)
				if err != nil {
					return err
				}
				if res.Jobs == nil {
					res.Jobs = []app.Job{}
				}
				return c.emit(res, func(w io.Writer) {
					if len(res.Jobs) == 0 {
						fmt.Fprintln(w, "没有任务")
						return
					}
					jobTable(w, res.Jobs)
					if res.NextCursor != nil {
						fmt.Fprintf(w, "还有更多，继续查看：--cursor %s\n", *res.NextCursor)
					}
				})
			})
		}),
	}
	lf := list.Flags()
	lf.StringVar(&listReq.Status, "status", "", "按状态过滤：queued / running / cancelling / succeeded / failed / cancelled / interrupted")
	lf.StringVar(&listReq.VideoID, "video", "", "按视频 ID 过滤")
	lf.IntVar(&listReq.Limit, "limit", 0, "返回条数（默认 20，上限 100）")
	lf.StringVar(&listReq.Cursor, "cursor", "", "上一页返回的 next_cursor")

	var waitSeconds int
	var follow bool
	get := &cobra.Command{
		Use:   "get <job_id>",
		Short: "查看任务；--wait 等待结束，--follow 持续输出进度",
		Args:  exactArgs(1, "jobs get <job_id> [--wait 秒] [--follow]"),
	}
	get.RunE = e.run(func(c *call) error {
		waitSet := get.Flags().Changed("wait")
		if waitSeconds < 0 {
			return app.E(app.CodeInvalidArgument, "--wait 的秒数必须是非负整数")
		}
		return c.withRemote(func(r app.Remote) error {
			ctx, cancel := c.reqCtx()
			job, err := r.GetJob(ctx, app.GetJobRequest{JobID: c.args[0]})
			cancel()
			if err != nil {
				return err
			}
			if !waitSet && !follow {
				return c.emit(job, func(w io.Writer) { printJob(w, job) })
			}

			ndjson := follow && c.jsonMode
			onUpdate := c.progressPrinter()
			if ndjson {
				// the one exception to "one JSON value" — one Job
				// per line whenever rev changes.
				onUpdate = func(j *app.Job) { _ = writeJSON(c.out, j) }
			}
			onUpdate(job)
			job, timedOut, err := c.waitJob(r, job, time.Duration(waitSeconds)*time.Second, onUpdate)
			if err != nil {
				return err
			}
			fail := waitFailure(job, timedOut)
			if ndjson {
				if fail != nil {
					// The job lines already carry the outcome.
					fail.silent = true
					return fail
				}
				return nil
			}
			if fail != nil {
				if !c.jsonMode {
					printJob(c.out, job)
				}
				return fail
			}
			return c.emit(job, func(w io.Writer) { printJob(w, job) })
		})
	})
	get.Flags().IntVar(&waitSeconds, "wait", 0, "最多等待多少秒直到任务结束（0 表示不限时）")
	get.Flags().BoolVar(&follow, "follow", false, "持续输出进度直到任务结束（与 --json 同用时输出 NDJSON）")

	action := func(use, short, done string, do func(r app.Remote, c *call, id string) (*app.Job, error)) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <job_id>",
			Short: short,
			Args:  exactArgs(1, "jobs "+use+" <job_id>"),
			RunE: e.run(func(c *call) error {
				return c.withRemote(func(r app.Remote) error {
					job, err := do(r, c, c.args[0])
					if err != nil {
						return err
					}
					return c.emit(job, func(w io.Writer) {
						fmt.Fprintln(w, done)
						printJob(w, job)
					})
				})
			}),
		}
	}
	cancel := action("cancel", "取消任务", "已请求取消", func(r app.Remote, c *call, id string) (*app.Job, error) {
		ctx, cancel := c.reqCtx()
		defer cancel()
		return r.CancelJob(ctx, id)
	})
	retry := action("retry", "重试失败、被取消或被中断的任务", "已重新排队", func(r app.Remote, c *call, id string) (*app.Job, error) {
		ctx, cancel := c.reqCtx()
		defer cancel()
		return r.RetryJob(ctx, id)
	})
	return group("jobs", "任务管理", list, get, cancel, retry)
}
