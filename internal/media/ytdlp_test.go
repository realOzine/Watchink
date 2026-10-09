package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"watchink/internal/app"
	"watchink/internal/procx"
)

func TestClassifyYtDlpError(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		want     app.Code
		wantHint string // substring
	}{
		// SOURCE_BLOCKED
		{"bot check", "ERROR: [youtube] dQw4w9WgXcQ: Sign in to confirm you're not a bot. Use --cookies-from-browser or --cookies for the authentication.", app.CodeSourceBlocked, "tools update yt-dlp"},
		{"bot check curly quote", "ERROR: [youtube] x: Sign in to confirm you’re not a bot.", app.CodeSourceBlocked, "tools update yt-dlp"},
		{"429", "ERROR: unable to download video data: HTTP Error 429: Too Many Requests", app.CodeSourceBlocked, "tools update yt-dlp"},
		{"bilibili 412", "ERROR: [BiliBili] BV17x411w7KC: Unable to download webpage: HTTP Error 412: Precondition Failed (caused by <HTTPError 412: Precondition Failed>)", app.CodeSourceBlocked, "tools update yt-dlp"},
		{"403", "ERROR: unable to download video data: HTTP Error 403: Forbidden", app.CodeSourceBlocked, ""},
		{"rate limited", "ERROR: [youtube] x: This content isn't available, try again later. Your account has been rate-limited by YouTube", app.CodeSourceBlocked, ""},
		// SOURCE_UNAVAILABLE
		{"private", "ERROR: [youtube] abc: Private video. Sign in if you've been granted access to this video", app.CodeSourceUnavailable, "未登录"},
		{"removed", "ERROR: [youtube] abc: Video unavailable. This video has been removed by the uploader", app.CodeSourceUnavailable, ""},
		{"terminated", "ERROR: [youtube] abc: Video unavailable. This video is no longer available because the YouTube account associated with this video has been terminated.", app.CodeSourceUnavailable, ""},
		{"age gate", "ERROR: [youtube] abc: Sign in to confirm your age. This video may be inappropriate for some users.", app.CodeSourceUnavailable, ""},
		{"members only", "ERROR: [youtube] abc: Join this channel to get access to members-only content like this video", app.CodeSourceUnavailable, ""},
		{"geo", "ERROR: [youtube] abc: Video unavailable. The uploader has not made this video available in your country", app.CodeSourceUnavailable, ""},
		{"geo generic", "ERROR: [BiliBili] 1: This video is not available in your country due to geo restriction", app.CodeSourceUnavailable, ""},
		{"404", "ERROR: [BiliBili] BV1xx: Unable to download webpage: HTTP Error 404: Not Found", app.CodeSourceUnavailable, ""},
		{"bilibili deleted", "ERROR: [BiliBili] BV1xx: 视频不存在或已被删除", app.CodeSourceUnavailable, ""},
		{"bilibili premium", "ERROR: [BiliBili] 1: This video is for premium members only", app.CodeSourceUnavailable, ""},
		{"upcoming live", "ERROR: [youtube] abc: This live event will begin in 3 hours.", app.CodeSourceUnavailable, ""},
		// NETWORK_ERROR
		{"timeout", "ERROR: [youtube] abc: Unable to download API page: The read operation timed out", app.CodeNetworkError, "代理"},
		{"dns", "ERROR: Unable to download webpage: <urlopen error [Errno 8] nodename nor servname provided, or not known>", app.CodeNetworkError, ""},
		{"refused", "ERROR: unable to download video data: [Errno 61] Connection refused", app.CodeNetworkError, ""},
		{"reset", "ERROR: [download] Got error: [Errno 54] Connection reset by peer", app.CodeNetworkError, ""},
		{"tls", "ERROR: Unable to download webpage: [SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed", app.CodeNetworkError, ""},
		{"proxy", "ERROR: Unable to download webpage: ('Unable to connect to proxy', OSError('Tunnel connection failed: 503'))", app.CodeNetworkError, ""},
		{"502", "ERROR: Unable to download JSON metadata: HTTP Error 502: Bad Gateway", app.CodeNetworkError, ""},
		{"incomplete", "ERROR: unable to download video data: IncompleteRead(1024 bytes read, 2048 more expected)", app.CodeNetworkError, ""},
		// DISK_FULL
		{"disk full", "ERROR: unable to write data: [Errno 28] No space left on device", app.CodeDiskFull, "磁盘"},
		// TOOL_FAILED
		{"extractor broke", "ERROR: [youtube] abc: Unable to extract initial player response; please report this issue on https://github.com/yt-dlp/yt-dlp/issues", app.CodeToolFailed, "tools update yt-dlp"},
		{"format", "ERROR: [youtube] abc: Requested format is not available. Use --list-formats for a list of available formats", app.CodeToolFailed, "tools update yt-dlp"},
		{"ffmpeg merge", "ERROR: Postprocessing: Conversion failed!", app.CodeToolFailed, ""},
		{"empty", "", app.CodeToolFailed, ""},
		{"traceback", "Traceback (most recent call last):\n  File \"yt_dlp/__main__.py\"\nKeyError: 'x'", app.CodeToolFailed, ""},
		// Warnings must not decide when an ERROR line exists.
		{"warning ignored", "WARNING: [youtube] HTTP Error 429 while fetching subtitles, retrying\nERROR: [youtube] abc: Private video", app.CodeSourceUnavailable, ""},
		{"warning used without error line", "WARNING: [youtube] abc: HTTP Error 429: Too Many Requests", app.CodeSourceBlocked, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, hint := ClassifyYtDlpError(tt.stderr)
			if code != tt.want {
				t.Errorf("code = %s, want %s (hint %q)", code, tt.want, hint)
			}
			if hint == "" || !strings.Contains(hint, tt.wantHint) {
				t.Errorf("hint = %q, want it to contain %q", hint, tt.wantHint)
			}
		})
	}
}

func TestYtDlpArgs(t *testing.T) {
	opts := DownloadOptions{YtDlp: "/t/yt-dlp", FFmpegDir: "/t/ff", DenoPath: "/t/deno", OutDir: "/data/v1"}
	yt, _ := NormalizeURL("https://youtu.be/dQw4w9WgXcQ?list=PL1")
	args := ytdlpArgs(opts, yt)
	joined := strings.Join(args, " ")

	// §11.5 flag shape, in order.
	wantOrder := []string{
		"--no-playlist", "--no-mtime", "--newline", "--ffmpeg-location /t/ff",
		"--js-runtimes deno:/t/deno", "-f " + ytdlpFormat, "--merge-output-format mp4",
		"--progress-template download:WK_PROGRESS", "-o /data/v1/source.%(ext)s",
		"-- https://www.youtube.com/watch?v=dQw4w9WgXcQ",
	}
	pos := 0
	for _, w := range wantOrder {
		i := strings.Index(joined[pos:], w)
		if i < 0 {
			t.Fatalf("missing or out of order: %q in\n%s", w, joined)
		}
		pos += i + len(w)
	}
	// The URL is the last argument and directly follows "--".
	if n := len(args); args[n-2] != "--" || args[n-1] != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
		t.Errorf("tail = %v", args[len(args)-2:])
	}
	for _, a := range args {
		if strings.Contains(strings.ToLower(a), "cookie") {
			t.Errorf("cookie option present: %s", a)
		}
	}
	if !strings.Contains(joined, "--print after_move:WK_INFO_title=%(title)j") || !strings.Contains(joined, "--ignore-config") {
		t.Errorf("args = %s", joined)
	}
	if strings.Contains(joined, "--proxy") {
		t.Error("--proxy passed without a proxy")
	}

	// Bilibili: no JS runtime; proxy when configured.
	opts.Proxy = "http://127.0.0.1:7890"
	bili, _ := NormalizeURL("https://www.bilibili.com/video/av170001?p=2")
	joined = strings.Join(ytdlpArgs(opts, bili), " ")
	if strings.Contains(joined, "--js-runtimes") {
		t.Error("--js-runtimes passed for Bilibili")
	}
	if !strings.Contains(joined, "--proxy http://127.0.0.1:7890") || !strings.HasSuffix(joined, "-- https://www.bilibili.com/video/BV17x411w7KC?p=2") {
		t.Errorf("args = %s", joined)
	}

	// YouTube without deno configured.
	opts.DenoPath = ""
	if strings.Contains(strings.Join(ytdlpArgs(opts, yt), " "), "--js-runtimes") {
		t.Error("--js-runtimes passed without a deno path")
	}
}

func TestYtDlpFormatPolicy(t *testing.T) {
	alts := strings.Split(ytdlpFormat, "/")
	if !strings.Contains(alts[0], "vcodec^=avc1") || !strings.Contains(alts[0], "acodec^=mp4a") || !strings.Contains(alts[0], "height<=1080") {
		t.Errorf("first choice is not H.264 + AAC <= 1080p: %s", alts[0])
	}
	if last := alts[len(alts)-1]; last != "b" {
		t.Errorf("last fallback = %q, want the unconditional best", last)
	}
}

func TestParseYtDlpProgress(t *testing.T) {
	tests := []struct {
		in     string
		want   float64
		wantOK bool
	}{
		{"downloading 500 1000 NA", 0.5, true},
		{"downloading 250 NA 1000.0", 0.25, true},
		{"downloading 2000 1000 NA", 1, true},
		{"finished 1000 1000 NA", 1, true},
		{"finished NA NA NA", 1, true},
		{"downloading NA NA NA", 0, false},
		{"downloading 10 NA NA", 0, false},
		{"downloading 10 0 0", 0, false},
		{"downloading", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseYtDlpProgress(tt.in)
		if ok != tt.wantOK || got != tt.want {
			t.Errorf("parseYtDlpProgress(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}

	// Two formats downloaded one after the other map to one rising value.
	var got []float64
	p := &ytdlpProgress{fn: func(f float64) { got = append(got, f) }}
	for _, l := range []string{
		"downloading 0 1000 NA", "downloading 500 1000 NA", "downloading 400 1000 NA", "finished 1000 1000 NA",
		"downloading 0 100 NA", "downloading 50 100 NA", "finished 100 100 NA",
	} {
		p.line(l)
	}
	want := []float64{0.45, 0.9, 0.945, 0.99}
	if len(got) != len(want) {
		t.Fatalf("progress = %v, want %v", got, want)
	}
	for i := range want {
		if d := got[i] - want[i]; d > 1e-9 || d < -1e-9 {
			t.Fatalf("progress = %v, want %v", got, want)
		}
	}
}

func TestProxySecrets(t *testing.T) {
	if got := proxySecrets("http://user:p%40ss@10.0.0.1:8080"); !reflect.DeepEqual(got, []string{"p%40ss"}) {
		t.Errorf("got %v", got)
	}
	for _, p := range []string{"", "http://127.0.0.1:7890", "socks5://user@host:1080", "not a url"} {
		if got := proxySecrets(p); got != nil {
			t.Errorf("proxySecrets(%q) = %v", p, got)
		}
	}
}

func TestDownloadValidation(t *testing.T) {
	fr := &fakeRunner{}
	tests := []struct {
		name string
		opts DownloadOptions
		want app.Code
	}{
		{"unsupported host", DownloadOptions{YtDlp: "y", OutDir: t.TempDir(), URL: "https://vimeo.com/1"}, app.CodeUnsupportedSource},
		{"option injection", DownloadOptions{YtDlp: "y", OutDir: t.TempDir(), URL: "--exec=rm"}, app.CodeUnsupportedSource},
		{"unresolved short link", DownloadOptions{YtDlp: "y", OutDir: t.TempDir(), URL: "https://b23.tv/AbCd123"}, app.CodeInvalidArgument},
		{"type mismatch", DownloadOptions{YtDlp: "y", OutDir: t.TempDir(), URL: "https://youtu.be/dQw4w9WgXcQ", SourceType: app.SourceBilibili}, app.CodeInvalidArgument},
		{"no tool", DownloadOptions{OutDir: t.TempDir(), URL: "https://youtu.be/dQw4w9WgXcQ"}, app.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Download(context.Background(), fr, tt.opts); !app.IsCode(err, tt.want) {
				t.Errorf("err = %v, want %s", err, tt.want)
			}
		})
	}
	if len(fr.cmds) != 0 {
		t.Errorf("yt-dlp was started for an invalid request: %+v", fr.cmds)
	}
}

func TestDownloadFailureMapping(t *testing.T) {
	fr := &fakeRunner{}
	fr.res, fr.err = toolFailed("WARNING: something\nERROR: [youtube] dQw4w9WgXcQ: Sign in to confirm you're not a bot.")
	_, err := Download(context.Background(), fr, DownloadOptions{YtDlp: "/t/yt-dlp", OutDir: t.TempDir(), URL: "https://youtu.be/dQw4w9WgXcQ"})
	if !app.IsCode(err, app.CodeSourceBlocked) {
		t.Fatalf("err = %v", err)
	}
	e := app.AsError(err)
	if e.Stage != app.StageAcquire || !strings.Contains(e.Hint, "watchink tools update yt-dlp") || !e.Retryable {
		t.Errorf("error = %+v", e)
	}
	if tail, _ := e.Details["stderr_tail"].(string); !strings.Contains(tail, "not a bot") {
		t.Errorf("details = %v", e.Details)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fr = &fakeRunner{err: context.Canceled}
	if _, err = Download(ctx, fr, DownloadOptions{YtDlp: "y", OutDir: t.TempDir(), URL: "https://youtu.be/dQw4w9WgXcQ"}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v", err)
	}

	fr = &fakeRunner{err: app.E(app.CodeToolMissing, "missing")}
	if _, err = Download(context.Background(), fr, DownloadOptions{YtDlp: "y", OutDir: t.TempDir(), URL: "https://youtu.be/dQw4w9WgXcQ"}); !app.IsCode(err, app.CodeToolMissing) {
		t.Errorf("missing tool: err = %v", err)
	}
}

// fakeYtDlp is a stand-in for yt-dlp driven through the real procx.Runner: it
// checks that the URL follows "--", then behaves like a successful download.
const fakeYtDlp = `#!/bin/sh
out=""; url=""; prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  if [ "$prev" = "--" ]; then url="$a"; fi
  prev="$a"
done
if [ -z "$url" ]; then echo "ERROR: no url after --" 1>&2; exit 2; fi
case "$url" in
  *FAILFAILFAI) echo "ERROR: [youtube] FAILFAILFAI: Private video" 1>&2; exit 1 ;;
esac
dir=$(dirname "$out")
echo "WK_PROGRESS downloading 0 1000 NA"
echo "WK_PROGRESS downloading 500 1000 NA"
echo "WK_PROGRESS finished 1000 1000 NA"
echo "WK_PROGRESS downloading 50 100 NA"
echo "WK_PROGRESS finished 100 100 NA"
printf 'fake video for %s' "$url" > "$dir/source.mp4"
echo 'WK_INFO_id="dQw4w9WgXcQ"'
echo 'WK_INFO_title="课程 \"第一讲\": intro = basics"'
echo 'WK_INFO_uploader="Some Channel"'
echo 'WK_INFO_duration=212.5'
echo 'WK_INFO_ext="mp4"'
echo "WK_INFO_filepath=\"$dir/source.mp4\""
echo "WARNING: harmless" 1>&2
`

func TestDownloadWithFakeYtDlp(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	bin := filepath.Join(t.TempDir(), "yt-dlp")
	if err := os.WriteFile(bin, []byte(fakeYtDlp), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := procx.New(procx.Options{})
	outDir := filepath.Join(t.TempDir(), "nested", "out")

	var prog []float64
	res, err := Download(context.Background(), runner, DownloadOptions{
		YtDlp: bin, FFmpegDir: "/t/ff", DenoPath: "/t/deno", OutDir: outDir,
		URL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PL123", SourceType: app.SourceYouTube,
		Progress: func(f float64) { prog = append(prog, f) },
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	want := &DownloadResult{
		Path: filepath.Join(outDir, "source.mp4"), Title: `课程 "第一讲": intro = basics`,
		PlatformID: "dQw4w9WgXcQ", Uploader: "Some Channel", DurationSec: 212.5, Ext: "mp4",
	}
	if !reflect.DeepEqual(res, want) {
		t.Errorf("result = %+v\n   want %+v", res, want)
	}
	b, _ := os.ReadFile(res.Path)
	if string(b) != "fake video for https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
		t.Errorf("yt-dlp did not receive the canonical URL: %q", b)
	}
	if len(prog) < 3 || prog[len(prog)-1] != 1 {
		t.Errorf("progress = %v", prog)
	}
	for i := 1; i < len(prog); i++ {
		if prog[i] <= prog[i-1] {
			t.Errorf("progress not increasing: %v", prog)
		}
	}

	_, err = Download(context.Background(), runner, DownloadOptions{
		YtDlp: bin, OutDir: t.TempDir(), URL: "https://youtu.be/FAILFAILFAI",
	})
	if !app.IsCode(err, app.CodeSourceUnavailable) {
		t.Errorf("err = %v, want SOURCE_UNAVAILABLE", err)
	}
}
