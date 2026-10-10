package media

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"watchink/internal/app"
	"watchink/internal/procx"
)

// DownloadOptions describes one yt-dlp download.
type DownloadOptions struct {
	// YtDlp is the yt-dlp executable.
	YtDlp string
	// FFmpegDir is the directory holding ffmpeg/ffprobe (--ffmpeg-location).
	FFmpegDir string
	// DenoPath is the deno executable; it is only passed for YouTube.
	DenoPath string
	// Proxy is an optional proxy URL (--proxy).
	Proxy string
	// URL is the video link. It is normalized again here and only the
	// canonical URL reaches yt-dlp.
	URL string
	// SourceType, when set, must match the normalized URL
	// (app.SourceYouTube / app.SourceBilibili).
	SourceType string
	// OutDir receives the file, named source.<ext>.
	OutDir string
	// Progress receives values in [0,1].
	Progress func(float64)
}

// DownloadResult describes the downloaded file.
type DownloadResult struct {
	Path        string  `json:"path"`
	Title       string  `json:"title"`
	PlatformID  string  `json:"platform_id,omitempty"` // id as reported by yt-dlp
	Uploader    string  `json:"uploader,omitempty"`
	DurationSec float64 `json:"duration_sec,omitempty"`
	Ext         string  `json:"ext,omitempty"`
}

const (
	// ytdlpFormat prefers H.264 + AAC up to 1080p so the playback stage can
	// serve the file directly, then degrades step by step.
	ytdlpFormat = "bv*[vcodec^=avc1][height<=1080]+ba[acodec^=mp4a]" +
		"/b[vcodec^=avc1][acodec^=mp4a][height<=1080]" +
		"/bv*[vcodec^=avc1][height<=1080]+ba" +
		"/bv*[height<=1080]+ba" +
		"/b[height<=1080]" +
		"/bv*+ba/b"

	ytdlpProgressPrefix = "WK_PROGRESS"
	ytdlpInfoPrefix     = "WK_INFO_"
	// Fields are separated by spaces; unknown values are printed as "NA".
	ytdlpProgressTemplate = "download:" + ytdlpProgressPrefix +
		" %(progress.status)s %(progress.downloaded_bytes)s %(progress.total_bytes)s %(progress.total_bytes_estimate)s"

	ytdlpUpdateHint = "平台可能已更新接口或限制了访问。请运行 `watchink tools update yt-dlp` 更新后重试；必要时在设置中配置代理。"
)

// ytdlpInfoFields are printed after the final file is in place, JSON-encoded
// one per line so titles with any characters survive.
var ytdlpInfoFields = []string{"id", "title", "uploader", "duration", "ext", "filepath"}

// ytdlpArgs builds the command line. Cookies are never passed, and
// --ignore-config keeps a user-level yt-dlp config (which could enable
// --cookies-from-browser) out of the picture.
func ytdlpArgs(opts DownloadOptions, ref *SourceRef) []string {
	args := []string{
		"--ignore-config",
		"--no-playlist", "--no-mtime", "--newline",
		"--ffmpeg-location", opts.FFmpegDir,
	}
	if ref.Type == app.SourceYouTube && opts.DenoPath != "" {
		args = append(args, "--js-runtimes", "deno:"+opts.DenoPath)
	}
	if opts.Proxy != "" {
		args = append(args, "--proxy", opts.Proxy)
	}
	args = append(args,
		"-f", ytdlpFormat,
		"--merge-output-format", "mp4",
		// --print implies --quiet; --progress keeps the progress lines.
		"--progress", "--progress-template", ytdlpProgressTemplate,
	)
	for _, f := range ytdlpInfoFields {
		args = append(args, "--print", "after_move:"+ytdlpInfoPrefix+f+"=%("+f+")j")
	}
	return append(args,
		"-o", filepath.Join(opts.OutDir, "source.%(ext)s"),
		"--", ref.CanonicalURL,
	)
}

// Download fetches one video with yt-dlp into opts.OutDir, merged to MP4
// where the formats allow it. Failures are classified from yt-dlp's stderr
// into SOURCE_UNAVAILABLE / SOURCE_BLOCKED / NETWORK_ERROR / DISK_FULL /
// TOOL_FAILED (see ClassifyYtDlpError).
func Download(ctx context.Context, runner Runner, opts DownloadOptions) (*DownloadResult, error) {
	ref, err := NormalizeURL(opts.URL)
	if err != nil {
		return nil, err
	}
	if ref.NeedsResolve {
		return nil, app.E(app.CodeInvalidArgument, "短链必须先解析为完整视频链接").WithStage(app.StageAcquire)
	}
	if opts.SourceType != "" && opts.SourceType != ref.Type {
		return nil, app.E(app.CodeInvalidArgument, "链接与来源类型 %s 不一致", opts.SourceType).WithStage(app.StageAcquire)
	}
	if opts.YtDlp == "" || opts.OutDir == "" {
		return nil, app.E(app.CodeInternal, "yt-dlp 路径或输出目录未指定").WithStage(app.StageAcquire)
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, WrapDiskFull(err, "")
	}

	prog := &ytdlpProgress{fn: opts.Progress}
	info := map[string]json.RawMessage{}
	var mu sync.Mutex
	onLine := func(line string) {
		if rest, ok := strings.CutPrefix(line, ytdlpProgressPrefix+" "); ok {
			prog.line(rest)
			return
		}
		if rest, ok := strings.CutPrefix(line, ytdlpInfoPrefix); ok {
			if k, v, ok := strings.Cut(rest, "="); ok {
				mu.Lock()
				info[k] = json.RawMessage(v)
				mu.Unlock()
			}
		}
	}
	env := append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	res, err := runner.Run(ctx, procx.Cmd{
		Path:         opts.YtDlp,
		Args:         ytdlpArgs(opts, ref),
		Dir:          opts.OutDir,
		Env:          env,
		OnStdoutLine: onLine,
		Stage:        app.StageAcquire,
		Secrets:      proxySecrets(opts.Proxy),
	})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, procx.ErrKilled) || !app.IsCode(err, app.CodeToolFailed) {
			return nil, err
		}
		tail := ""
		if res != nil {
			tail = res.StderrTail
		}
		code, hint := ClassifyYtDlpError(tail)
		e := app.E(code, "%s", ytdlpMessage(code)).WithStage(app.StageAcquire).
			WithDetail("stderr_tail", tail).WithCause(err)
		if res != nil {
			e = e.WithDetail("exit_code", res.ExitCode)
		}
		if hint != "" {
			e = e.WithHint(hint)
		}
		return nil, e
	}

	mu.Lock()
	defer mu.Unlock()
	out := &DownloadResult{}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(info[k], &s)
		return s
	}
	out.Title, out.PlatformID, out.Uploader, out.Ext, out.Path = str("title"), str("id"), str("uploader"), str("ext"), str("filepath")
	_ = json.Unmarshal(info["duration"], &out.DurationSec)
	if out.Path == "" || !fileExists(out.Path) {
		out.Path = findDownloaded(opts.OutDir)
	}
	if out.Path == "" {
		return nil, app.E(app.CodeToolFailed, "yt-dlp 没有生成视频文件").WithStage(app.StageAcquire)
	}
	if out.Ext == "" {
		out.Ext = strings.TrimPrefix(filepath.Ext(out.Path), ".")
	}
	if opts.Progress != nil {
		opts.Progress(1)
	}
	return out, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// findDownloaded is the fallback when yt-dlp did not report a file path.
func findDownloaded(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "source.*"))
	for _, m := range matches {
		switch strings.ToLower(filepath.Ext(m)) {
		case ".part", ".ytdl", ".json", ".tmp":
			continue
		}
		if fileExists(m) {
			return m
		}
	}
	return ""
}

// proxySecrets returns the password of a proxy URL so it is masked in stderr.
func proxySecrets(proxy string) []string {
	if i := strings.Index(proxy, "://"); i >= 0 {
		rest := proxy[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			if _, pw, ok := strings.Cut(rest[:at], ":"); ok && pw != "" {
				return []string{pw}
			}
		}
	}
	return nil
}

func ytdlpMessage(code app.Code) string {
	switch code {
	case app.CodeSourceUnavailable:
		return "视频不可用（已删除、设为私密、需要登录或存在地区限制）"
	case app.CodeSourceBlocked:
		return "平台拒绝了下载请求（访问受限或触发了风控）"
	case app.CodeNetworkError:
		return "下载视频时网络出错"
	case app.CodeDiskFull:
		return "磁盘空间不足"
	}
	return "yt-dlp 下载失败"
}

// ytdlpProgress maps per-file download progress to one overall fraction.
// yt-dlp downloads the video and audio formats one after the other, so the
// byte counter restarts; the first file is assumed to be 90 % of the work.
type ytdlpProgress struct {
	fn    func(float64)
	phase int
	prev  float64 // fraction within the current file
	last  float64 // last overall value reported
}

func (p *ytdlpProgress) line(rest string) {
	f, ok := parseYtDlpProgress(rest)
	if !ok || p.fn == nil {
		return
	}
	if f+0.2 < p.prev {
		p.phase++ // the counter restarted: next format
	}
	p.prev = f
	overall := f * 0.9
	if p.phase > 0 {
		overall = 0.9 + f*0.09
	}
	if overall > p.last {
		p.last = overall
		p.fn(overall)
	}
}

// parseYtDlpProgress reads "<status> <downloaded> <total> <estimate>" (the
// part after the WK_PROGRESS prefix) and returns the per-file fraction.
func parseYtDlpProgress(rest string) (float64, bool) {
	f := strings.Fields(rest)
	if len(f) < 4 {
		return 0, false
	}
	if f[0] == "finished" {
		return 1, true
	}
	done, err := strconv.ParseFloat(f[1], 64)
	if err != nil {
		return 0, false
	}
	total, err := strconv.ParseFloat(f[2], 64)
	if err != nil || total <= 0 {
		total, err = strconv.ParseFloat(f[3], 64)
		if err != nil || total <= 0 {
			return 0, false
		}
	}
	frac := done / total
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return frac, true
}

var reHTTP5xx = regexp.MustCompile(`http error 5\d\d`)

var (
	// Checked first: "Sign in to confirm you're not a bot" must not be read
	// as a login requirement.
	ytdlpBlocked = []string{
		"confirm you're not a bot", "confirm you’re not a bot", "confirm you are not a bot",
		"http error 429", "too many requests",
		"http error 412", "precondition failed",
		"http error 403", "forbidden",
		"rate-limit", "rate limit", "captcha", "unusual traffic",
		"blocked by server", "风控", "请求被拦截", "访问频繁",
	}
	ytdlpUnavailable = []string{
		"private video", "video is private", "this video is private",
		"video unavailable", "this video is unavailable", "video is not available", "no longer available",
		"has been removed", "removed by the uploader", "been terminated", "account has been",
		"does not exist", "http error 404", "http error 410",
		"sign in to confirm your age", "age-restricted", "age restricted",
		"login required", "requires login", "need to log in", "log in to", "sign in to", "use --cookies",
		"members-only", "members only", "join this channel", "premium", "requires payment", "purchase",
		"not available in your country", "geo-restricted", "geo restricted", "geo restriction", "in your region", "in your location",
		"copyright", "live event will begin", "premieres in", "is offline",
		"视频不存在", "稿件不可见", "已被删除", "大会员", "地区", "需要登录",
	}
	ytdlpNetwork = []string{
		"timed out", "timeout", "connection refused", "connection reset", "connection aborted",
		"temporary failure in name resolution", "name or service not known", "nodename nor servname",
		"getaddrinfo failed", "failed to resolve", "network is unreachable", "no route to host",
		"remote end closed connection", "incompleteread", "incomplete read", "transporterror", "transport error",
		"ssl:", "sslerror", "certificate verify failed", "eof occurred in violation of protocol",
		"proxyerror", "unable to connect to proxy", "tunnel connection failed",
		"unable to download webpage", "unable to download video data", "unable to download json", "unable to download api page",
		"bad gateway", "service unavailable", "gateway time-out",
	}
	ytdlpExtractor = []string{
		"unable to extract", "please report this issue", "unsupported url", "no video formats found",
		"requested format is not available", "nsig extraction failed", "signature extraction failed",
		"extractorerror", "failed to parse json", "failed to extract",
	}
)

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ClassifyYtDlpError maps yt-dlp's stderr to an error code and a hint. It is
// a pure function. Only "ERROR:" lines are considered when there are any, so
// warnings about unrelated things cannot change the verdict.
//
//	DISK_FULL           no space left on device
//	SOURCE_BLOCKED      429 / 412 / 403, "Sign in to confirm you're not a bot", captcha
//	SOURCE_UNAVAILABLE  private, removed, login or payment required, geo-blocked
//	NETWORK_ERROR       timeouts, DNS, TLS, proxy, connection failures, 5xx
//	TOOL_FAILED         everything else (extractor errors get the update hint)
func ClassifyYtDlpError(stderr string) (app.Code, string) {
	if IsDiskFull(nil, stderr) {
		return app.CodeDiskFull, "请清理磁盘空间后重试。"
	}
	var errLines []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.Contains(l, "ERROR:") {
			errLines = append(errLines, l)
		}
	}
	text := stderr
	if len(errLines) > 0 {
		text = strings.Join(errLines, "\n")
	}
	text = strings.ToLower(text)

	switch {
	case containsAny(text, ytdlpBlocked):
		return app.CodeSourceBlocked, ytdlpUpdateHint
	case containsAny(text, ytdlpUnavailable):
		return app.CodeSourceUnavailable, "请确认视频可以在未登录的浏览器中正常观看。v0 不支持需要登录、付费或受地区限制的视频。"
	case containsAny(text, ytdlpNetwork) || reHTTP5xx.MatchString(text):
		return app.CodeNetworkError, "请检查网络连接或代理设置后重试。"
	case containsAny(text, ytdlpExtractor):
		return app.CodeToolFailed, ytdlpUpdateHint
	}
	return app.CodeToolFailed, "可以尝试运行 `watchink tools update yt-dlp` 更新后重试。"
}
