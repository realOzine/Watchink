package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"watchink/internal/app"
)

func (e *env) cmdVideos() *cobra.Command {
	var req app.ListVideosRequest
	list := &cobra.Command{
		Use:   "list",
		Short: "列出视频",
		Args:  noArgs,
		RunE: e.run(func(c *call) error {
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				defer cancel()
				res, err := r.ListVideos(ctx, req)
				if err != nil {
					return err
				}
				if res.Videos == nil {
					res.Videos = []app.Video{}
				}
				return c.emit(res, func(w io.Writer) {
					if len(res.Videos) == 0 {
						fmt.Fprintln(w, "资料库中还没有视频")
						return
					}
					rows := make([][]string, 0, len(res.Videos))
					for _, v := range res.Videos {
						dur := "-"
						if v.DurationMs != nil {
							dur = clock(*v.DurationMs)
						}
						rows = append(rows, []string{v.VideoID, truncate(v.Title, 40), v.Source.Type, dur, statusName(v.Status), localTime(v.CreatedAt)})
					}
					table(w, []string{"视频", "标题", "来源", "时长", "状态", "创建时间"}, rows)
					if res.NextCursor != nil {
						fmt.Fprintf(w, "还有更多，继续查看：--cursor %s\n", *res.NextCursor)
					}
				})
			})
		}),
	}
	lf := list.Flags()
	lf.IntVar(&req.Limit, "limit", 0, "返回条数（默认 20，上限 100）")
	lf.StringVar(&req.TitleContains, "title-contains", "", "只列出标题包含该文本的视频")
	lf.StringVar(&req.Cursor, "cursor", "", "上一页返回的 next_cursor")

	get := &cobra.Command{
		Use:   "get <video_id>",
		Short: "查看视频",
		Args:  exactArgs(1, "videos get <video_id>"),
		RunE: e.run(func(c *call) error {
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				defer cancel()
				v, err := r.GetVideo(ctx, c.args[0])
				if err != nil {
					return err
				}
				return c.emit(v, func(w io.Writer) {
					dur := "-"
					if v.DurationMs != nil {
						dur = clock(*v.DurationMs)
					}
					src := v.Source.Type
					if v.Source.URI != "" {
						src += "  " + v.Source.URI
					}
					kv(w,
						[2]string{"视频", v.VideoID},
						[2]string{"标题", v.Title},
						[2]string{"来源", src},
						[2]string{"时长", dur},
						[2]string{"语言", deref(v.Language)},
						[2]string{"状态", fmt.Sprintf("%s (%s)", statusName(v.Status), v.Status)},
						[2]string{"最近任务", deref(v.LatestJobID)},
						[2]string{"字幕", deref(v.TranscriptID)},
						[2]string{"笔记", deref(v.NoteID)},
						[2]string{"可播放", yesNo(v.PlaybackReady)},
						[2]string{"Web", v.WebURL},
						[2]string{"创建时间", localTime(v.CreatedAt)},
						[2]string{"更新时间", localTime(v.UpdatedAt)},
					)
				})
			})
		}),
	}
	return group("videos", "视频资料库", list, get)
}

// maxNotePages bounds the pagination loop of `note get`.
const maxNotePages = 10000

func (e *env) cmdNote() *cobra.Command {
	var format string
	get := &cobra.Command{
		Use:   "get <video_id>",
		Short: "读取笔记（默认输出规范 Markdown）",
		Args:  exactArgs(1, "note get <video_id> [--format md|json]"),
		RunE: e.run(func(c *call) error {
			var apiFormat string
			switch format {
			case "md", "markdown":
				apiFormat = app.FormatMarkdown
			case "json":
				apiFormat = app.FormatJSON
			default:
				return oneOf("format", format, "md", "json")
			}
			return c.withRemote(func(r app.Remote) error {
				// The API pages long text (§13.1); the CLI always returns
				// the whole note, pinned to one note version.
				var note *app.Note
				var content strings.Builder
				req := app.GetNoteRequest{VideoID: c.args[0], Format: apiFormat}
				for page := 0; ; page++ {
					ctx, cancel := c.reqCtx()
					part, err := r.GetNote(ctx, req)
					cancel()
					if err != nil {
						return err
					}
					if note == nil {
						note = part
						req.NoteID = part.NoteID
					}
					content.WriteString(part.Content)
					if part.NextOffset == nil || *part.NextOffset <= req.Offset || page >= maxNotePages {
						break
					}
					req.Offset = *part.NextOffset
				}
				note.Content = content.String()
				note.NextOffset = nil
				if note.Citations == nil {
					note.Citations = []app.Citation{}
				}
				return c.emit(note, func(w io.Writer) {
					io.WriteString(w, note.Content)
					if !strings.HasSuffix(note.Content, "\n") {
						fmt.Fprintln(w)
					}
				})
			})
		}),
	}
	get.Flags().StringVar(&format, "format", "md", "md（规范 Markdown）或 json（结构化笔记）")
	return group("note", "笔记", get)
}

// parseTime parses the CLI time syntax of §13.4: H:MM:SS, MM:SS (both with
// optional fractional seconds) or 12345ms.
func parseTime(s string) (int64, error) {
	bad := func() (int64, error) {
		return 0, app.E(app.CodeInvalidArgument, "无法解析时间 %q", s).
			WithHint("时间格式：H:MM:SS、MM:SS 或 12345ms")
	}
	s = strings.TrimSpace(s)
	if ms, ok := strings.CutSuffix(s, "ms"); ok {
		n, err := strconv.ParseInt(ms, 10, 64)
		if err != nil || n < 0 {
			return bad()
		}
		return n, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return bad()
	}
	var total int64
	for i, p := range parts {
		last := i == len(parts)-1
		if p == "" {
			return bad()
		}
		if last {
			sec, err := strconv.ParseFloat(p, 64)
			if err != nil || sec < 0 || sec >= 60 || strings.ContainsAny(p, "eE+-") {
				return bad()
			}
			total = total*60_000 + int64(sec*1000+0.5)
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 || (i > 0 && n >= 60) {
			return bad()
		}
		total = total*60 + n
	}
	return total, nil
}

// maxTranscriptPages bounds the pagination loop of `transcript get`.
const maxTranscriptPages = 10000

func (e *env) cmdTranscript() *cobra.Command {
	var from, to, format string
	get := &cobra.Command{
		Use:   "get <video_id>",
		Short: "读取字幕，可按时间区间截取",
		Args:  exactArgs(1, "transcript get <video_id> [--from T] [--to T] [--format json|srt|vtt|txt]"),
	}
	get.RunE = e.run(func(c *call) error {
		formatSet := get.Flags().Changed("format")
		if err := oneOf("format", format, app.FormatJSON, app.FormatSRT, app.FormatVTT, app.FormatTXT); err != nil {
			return err
		}
		// No limit: the CLI returns everything in the requested range.
		req := app.GetTranscriptRequest{VideoID: c.args[0], Format: format, MaxChars: -1}
		if from != "" {
			ms, err := parseTime(from)
			if err != nil {
				return err
			}
			req.StartMs = &ms
		}
		if to != "" {
			ms, err := parseTime(to)
			if err != nil {
				return err
			}
			req.EndMs = &ms
		}
		if req.StartMs != nil && req.EndMs != nil && *req.EndMs < *req.StartMs {
			return app.E(app.CodeInvalidArgument, "--to 不能早于 --from")
		}
		return c.withRemote(func(r app.Remote) error {
			ctx, cancel := c.reqCtx()
			tr, err := r.GetTranscript(ctx, req)
			cancel()
			if err != nil {
				return err
			}
			// Defensive: an engine that still truncates is paged through
			// for JSON; rendered text cannot be stitched, so it is flagged.
			for page := 0; format == app.FormatJSON && tr.Truncated && tr.NextStartMs != nil && page < maxTranscriptPages; page++ {
				next := *tr.NextStartMs
				if req.StartMs != nil && next <= *req.StartMs {
					break
				}
				req.StartMs = &next
				ctx, cancel := c.reqCtx()
				more, err := r.GetTranscript(ctx, req)
				cancel()
				if err != nil {
					return err
				}
				tr.Segments = append(tr.Segments, more.Segments...)
				tr.Truncated, tr.NextStartMs = more.Truncated, more.NextStartMs
			}
			if tr.Segments == nil {
				tr.Segments = []app.Segment{}
			}
			if tr.Truncated {
				c.logf("警告：字幕内容过长，输出被截断；用 --from / --to 分段读取")
			}
			return c.emit(tr, func(w io.Writer) {
				switch {
				case format != app.FormatJSON:
					io.WriteString(w, tr.Content)
					if tr.Content != "" && !strings.HasSuffix(tr.Content, "\n") {
						fmt.Fprintln(w)
					}
				case formatSet:
					// An explicit --format json is a request for JSON even
					// without the global --json flag.
					enc := json.NewEncoder(w)
					enc.SetEscapeHTML(false)
					enc.SetIndent("", "  ")
					_ = enc.Encode(tr)
				default:
					for _, s := range tr.Segments {
						fmt.Fprintf(w, "[%s] %s\n", clock(s.StartMs), s.Text)
					}
				}
			})
		})
	})
	f := get.Flags()
	f.StringVar(&from, "from", "", "起始时间：H:MM:SS、MM:SS 或 12345ms")
	f.StringVar(&to, "to", "", "结束时间：H:MM:SS、MM:SS 或 12345ms")
	f.StringVar(&format, "format", app.FormatJSON, "json / srt / vtt / txt")
	return group("transcript", "字幕", get)
}

// exportResult is the --json output of `export`.
type exportResult struct {
	*app.Export
	// OutputPath is the file written by the CLI, or "-" for stdout.
	OutputPath string `json:"output_path"`
	// Content is only set for `--out - --json`, where stdout must still be a
	// single JSON value.
	Content *string `json:"content,omitempty"`
}

func (e *env) cmdExport() *cobra.Command {
	var format, out string
	cmd := &cobra.Command{
		Use:   "export <video_id>",
		Short: "导出笔记或字幕；文件由 CLI 进程写到当前目录或 --out",
		Args:  exactArgs(1, "export <video_id> --format md|md-raw|json|srt|vtt|txt [--out 路径|-]"),
		RunE: e.run(func(c *call) error {
			if format == "" {
				return app.E(app.CodeInvalidArgument, "缺少 --format").
					WithHint("可选值：md / md-raw / json / srt / vtt / txt")
			}
			if err := oneOf("format", format, app.FormatMD, app.FormatMDRaw, app.FormatJSON, app.FormatSRT, app.FormatVTT, app.FormatTXT); err != nil {
				return err
			}
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				created, err := r.CreateExport(ctx, app.ExportRequest{VideoID: c.args[0], Format: format})
				cancel()
				if err != nil {
					return err
				}
				// The engine only writes inside the data dir (§15); the copy
				// the user asked for is written by this process.
				body, exp, err := r.OpenExport(c.ctx, created.ExportID)
				if err != nil {
					return err
				}
				defer body.Close()
				if exp == nil {
					exp = created
				}
				res := exportResult{Export: exp}

				if out == "-" {
					res.OutputPath = "-"
					if c.jsonMode {
						var buf bytes.Buffer
						if err := copyVerified(&buf, body, exp.SHA256); err != nil {
							return err
						}
						s := buf.String()
						res.Content = &s
						return writeJSON(c.out, res)
					}
					return copyVerified(c.out, body, exp.SHA256)
				}

				target, err := exportTarget(out, exp.Filename)
				if err != nil {
					return err
				}
				if err := writeFileAtomic(target, body, exp.SHA256); err != nil {
					return err
				}
				res.OutputPath = target
				return c.emit(res, func(w io.Writer) {
					fmt.Fprintf(w, "已导出 %s（%s）\n", target, humanBytes(exp.Bytes))
				})
			})
		}),
	}
	cmd.Flags().StringVar(&format, "format", "", "md / md-raw / json / srt / vtt / txt（必填）")
	cmd.Flags().StringVar(&out, "out", "", "输出路径；- 表示 stdout（默认 ./<文件名>）")
	return cmd
}

// exportTarget resolves --out to an absolute file path. The engine-chosen
// file name is reduced to its base name so it can never escape the target
// directory.
func exportTarget(out, filename string) (string, error) {
	name := filepath.Base(filepath.FromSlash(filename))
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = "export"
	}
	target := out
	switch {
	case out == "":
		target = name
	case strings.HasSuffix(out, "/") || strings.HasSuffix(out, string(filepath.Separator)):
		target = filepath.Join(out, name)
	default:
		if st, err := os.Stat(out); err == nil && st.IsDir() {
			target = filepath.Join(out, name)
		}
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", app.E(app.CodeInvalidArgument, "无法解析输出路径：%v", err)
	}
	return abs, nil
}

// copyVerified copies src to dst and checks the SHA-256 announced by the
// engine (skipped when wantSHA is empty).
func copyVerified(dst io.Writer, src io.Reader, wantSHA string) error {
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, h), src); err != nil {
		return app.E(app.CodeInternal, "读取导出内容失败：%v", err).WithCause(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); wantSHA != "" && !strings.EqualFold(got, wantSHA) {
		return app.E(app.CodeChecksumMismatch, "导出内容的校验和不符").WithHint("重新执行导出")
	}
	return nil
}

// writeFileAtomic writes src to path through a temporary file in the same
// directory, so a failed export never leaves a half-written file behind.
func writeFileAtomic(path string, src io.Reader, wantSHA string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".watchink-export-*")
	if err != nil {
		return writeError(path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := copyVerified(tmp, src, wantSHA); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return writeError(path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return writeError(path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return writeError(path, err)
	}
	return nil
}

func writeError(path string, err error) error {
	code := app.CodeInternal
	if os.IsNotExist(err) || os.IsPermission(err) {
		code = app.CodeInvalidArgument
	}
	return app.E(code, "无法写入 %s：%v", path, err).WithCause(err).
		WithHint("用 --out 指定一个可写的路径")
}
