package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"watchink/internal/app"
)

// writeJSON writes v as one compact JSON value followed by a newline. HTML
// escaping is off so URLs and Markdown stay readable.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// emit writes the command result: the resource as JSON with --json, the
// human rendering otherwise.
func (c *call) emit(v any, human func(w io.Writer)) error {
	if c.jsonMode {
		return writeJSON(c.out, v)
	}
	human(c.out)
	return nil
}

// logf writes one progress / log line to stderr (silent with --json unless
// --verbose).
func (c *call) logf(format string, args ...any) {
	fmt.Fprintf(c.progress, format+"\n", args...)
}

func printHumanError(w io.Writer, e *app.Error) {
	if e.Stage != "" {
		fmt.Fprintf(w, "错误 [%s] (%s)：%s\n", e.Code, e.Stage, e.Message)
	} else {
		fmt.Fprintf(w, "错误 [%s]：%s\n", e.Code, e.Message)
	}
	if e.Hint != "" {
		fmt.Fprintf(w, "提示：%s\n", e.Hint)
	}
}

// runeWidth is a small East Asian width approximation, enough to align
// tables that mix CJK and ASCII without a third-party dependency.
func runeWidth(r rune) int {
	switch {
	case r < 0x1100:
		return 1
	case r <= 0x115F,
		r >= 0x2E80 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE4F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

func pad(s string, width int) string {
	if d := width - displayWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// truncate shortens s to at most width display columns.
func truncate(s string, width int) string {
	if displayWidth(s) <= width {
		return s
	}
	n := 0
	var b strings.Builder
	for _, r := range s {
		w := runeWidth(r)
		if n+w > width-1 {
			break
		}
		b.WriteRune(r)
		n += w
	}
	return b.String() + "…"
}

// table writes an aligned table; the last column is not padded.
func table(w io.Writer, header []string, rows [][]string) {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = displayWidth(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) {
				widths[i] = max(widths[i], displayWidth(cell))
			}
		}
	}
	line := func(cells []string) {
		var b strings.Builder
		for i, cell := range cells {
			if i == len(cells)-1 {
				b.WriteString(cell)
			} else {
				b.WriteString(pad(cell, widths[i]))
				b.WriteString("  ")
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	line(header)
	for _, row := range rows {
		line(row)
	}
}

// kv writes aligned "label  value" lines.
func kv(w io.Writer, pairs ...[2]string) {
	width := 0
	for _, p := range pairs {
		width = max(width, displayWidth(p[0]))
	}
	for _, p := range pairs {
		fmt.Fprintf(w, "%s  %s\n", pad(p[0], width), p[1])
	}
}

var statusNames = map[string]string{
	app.JobQueued:       "排队中",
	app.JobRunning:      "运行中",
	app.JobCancelling:   "取消中",
	app.JobSucceeded:    "已完成",
	app.JobFailed:       "失败",
	app.JobCancelled:    "已取消",
	app.JobInterrupted:  "已中断",
	app.StagePending:    "等待中",
	app.VideoProcessing: "处理中",
	app.VideoReady:      "就绪",

	app.ImportStatusAlreadyProcessed: "已处理过",
	app.ToolMissing:                  "缺失",
	app.ToolDownloading:              "下载中",
}

func statusName(s string) string {
	if n, ok := statusNames[s]; ok {
		return n
	}
	return s
}

func percent(p float64) string {
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	return fmt.Sprintf("%d%%", int(p*100+0.5))
}

func deref(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

// clock renders a media offset as H:MM:SS.
func clock(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	s := ms / 1000
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
}

// localTime renders an RFC 3339 instant in local time for humans.
func localTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

// printJob renders a job with its stage list (human mode).
func printJob(w io.Writer, j *app.Job) {
	pairs := [][2]string{
		{"任务", j.JobID},
		{"类型", j.Kind},
		{"状态", fmt.Sprintf("%s (%s)", statusName(j.Status), j.Status)},
		{"进度", percent(j.Progress)},
	}
	if j.VideoID != nil {
		pairs = append(pairs, [2]string{"视频", *j.VideoID})
	}
	if j.Result.NoteID != nil {
		pairs = append(pairs, [2]string{"笔记", *j.Result.NoteID})
	}
	if j.Attempt > 1 {
		pairs = append(pairs, [2]string{"尝试", fmt.Sprint(j.Attempt)})
	}
	kv(w, pairs...)
	if len(j.Stages) > 0 {
		fmt.Fprintln(w, "阶段")
		width := 0
		for _, s := range j.Stages {
			width = max(width, displayWidth(s.Stage))
		}
		for _, s := range j.Stages {
			mark := "·"
			switch s.Status {
			case app.StageSucceeded:
				mark = "✓"
			case app.StageRunning:
				mark = "▶"
			case app.StageFailed:
				mark = "✗"
			case app.StageCancelled:
				mark = "-"
			}
			line := fmt.Sprintf("  %s %s  %s  %4s", mark, pad(s.Stage, width), pad(statusName(s.Status), 6), percent(s.Progress))
			if s.Reused {
				line += "  (复用)"
			}
			fmt.Fprintln(w, line)
		}
	}
	if j.Error != nil {
		printHumanError(w, j.Error)
	}
}

// progressLine is the one-line stderr rendering of a job update.
func progressLine(j *app.Job) string {
	line := fmt.Sprintf("[%4s] %s", percent(j.Progress), statusName(j.Status))
	if j.Stage != "" {
		line += " · " + j.Stage
		for _, s := range j.Stages {
			if s.Stage == j.Stage && s.Status == app.StageRunning {
				line += " " + percent(s.Progress)
			}
		}
	}
	return line
}
