package asr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"watchink/internal/app"
	"watchink/internal/procx"
)

// WhisperCpp drives whisper.cpp's whisper-cli (D-07). The flag names and the
// output formats below were checked against examples/cli/cli.cpp upstream:
// `-oj` JSON, `-pp` progress lines and the `-dl` language line.
type WhisperCpp struct {
	// Runner starts the child process.
	Runner Runner
	// Bin is the whisper-cli executable.
	Bin string
	// Model is the ggml model file.
	Model string
	// VADModel is the silero VAD model; required when Request.VAD is set.
	VADModel string
}

var _ Transcriber = (*WhisperCpp)(nil)

// NewWhisperCpp returns a whisper.cpp transcriber.
func NewWhisperCpp(runner Runner, bin, model, vadModel string) *WhisperCpp {
	return &WhisperCpp{Runner: runner, Bin: bin, Model: model, VADModel: vadModel}
}

var (
	// "whisper_full_with_state: auto-detected language: en (p = 0.977899)"
	reDetected = regexp.MustCompile(`auto-detected language:\s*([A-Za-z]{2,8})\b`)
	// "whisper_print_progress_callback: progress =  45%"
	reProgress = regexp.MustCompile(`progress\s*=\s*(\d{1,3})\s*%`)
	// "whisper.cpp version: 1.8.2"
	reVersion = regexp.MustCompile(`(?i)version:?\s*v?([0-9][0-9A-Za-z.\-+]*)`)
	reLang    = regexp.MustCompile(`^[a-z]{2,8}$`)
)

func asrFailed(format string, args ...any) *app.Error {
	return app.E(app.CodeASRFailed, format, args...).WithStage(app.StageTranscribe)
}

func (w *WhisperCpp) check(audio string) error {
	switch {
	case w.Runner == nil || w.Bin == "":
		return app.E(app.CodeToolMissing, "未配置 whisper-cli").WithStage(app.StageTranscribe)
	case w.Model == "":
		return app.E(app.CodeToolMissing, "未配置语音模型").WithStage(app.StageTranscribe)
	case audio == "":
		return asrFailed("未指定音频文件")
	}
	return nil
}

func threadArg(n int) string {
	if n <= 0 {
		n = DefaultThreads()
	}
	return strconv.Itoa(n)
}

// wrap converts a runner error into the package's error contract: context
// cancellation, force-kills and TOOL_MISSING pass through; a full disk is
// DISK_FULL; every other failure is ASR_FAILED with the stderr tail.
func wrap(ctx context.Context, err error, res *procx.Result) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, procx.ErrKilled) || app.IsCode(err, app.CodeToolMissing) {
		return err
	}
	tail := ""
	if res != nil {
		tail = res.StderrTail
	}
	var e *app.Error
	if strings.Contains(strings.ToLower(tail), "no space left on device") {
		e = app.E(app.CodeDiskFull, "磁盘空间不足").WithStage(app.StageTranscribe).WithHint("请清理磁盘空间后重试。")
	} else {
		e = asrFailed("语音转写失败")
	}
	e = e.WithDetail("stderr_tail", tail).WithCause(err)
	if res != nil {
		e = e.WithDetail("exit_code", res.ExitCode)
	}
	return e
}

// detectArgs builds the language-detection command.
func (w *WhisperCpp) detectArgs(req DetectRequest) []string {
	return []string{"-m", w.Model, "-f", req.Audio, "-t", threadArg(req.Threads), "-dl"}
}

// DetectLanguage runs `whisper-cli -dl`, which exits right after detecting
// the language from the first 30 s, and parses the "auto-detected language"
// line. Any language is returned as is; deciding what to do with a language
// other than zh/en is the caller's business.
func (w *WhisperCpp) DetectLanguage(ctx context.Context, req DetectRequest) (string, error) {
	if err := w.check(req.Audio); err != nil {
		return "", err
	}
	var mu sync.Mutex
	lang := ""
	scan := func(line string) {
		if m := reDetected.FindStringSubmatch(line); m != nil {
			mu.Lock()
			lang = strings.ToLower(m[1])
			mu.Unlock()
		}
	}
	res, err := w.Runner.Run(ctx, procx.Cmd{
		Path: w.Bin, Args: w.detectArgs(req),
		OnStdoutLine: scan, OnStderrLine: scan,
		Stage: app.StageTranscribe,
	})
	if err != nil {
		return "", wrap(ctx, err, res)
	}
	mu.Lock()
	defer mu.Unlock()
	if lang == "" {
		e := asrFailed("未能检测出音频语言").WithHint("可以在设置中把转写语言固定为 zh 或 en 后重试。")
		if res != nil {
			e = e.WithDetail("stderr_tail", res.StderrTail)
		}
		return "", e
	}
	return lang, nil
}

// transcribeArgs builds the §8.2 command:
//
//	whisper-cli -m <model> -f audio.wav -l <lang> -t <threads> -oj -of <out> -pp
//	  [--prompt <prompt>] [--vad --vad-model <silero model>]
func (w *WhisperCpp) transcribeArgs(req Request) []string {
	args := []string{
		"-m", w.Model, "-f", req.Audio, "-l", req.Language, "-t", threadArg(req.Threads),
		"-oj", "-of", req.OutPrefix, "-pp",
	}
	if req.Prompt != "" {
		args = append(args, "--prompt", req.Prompt)
	}
	if req.VAD {
		args = append(args, "--vad", "--vad-model", w.VADModel)
	}
	return args
}

// Transcribe runs one whisper-cli process over the whole file and parses the
// JSON it writes to <OutPrefix>.json. The file is left in place untouched.
func (w *WhisperCpp) Transcribe(ctx context.Context, req Request) (*Result, error) {
	if err := w.check(req.Audio); err != nil {
		return nil, err
	}
	req.Language = strings.ToLower(strings.TrimSpace(req.Language))
	if !reLang.MatchString(req.Language) {
		return nil, asrFailed("无效的转写语言：%q", req.Language)
	}
	if req.OutPrefix == "" {
		return nil, asrFailed("未指定转写输出路径")
	}
	if req.VAD && w.VADModel == "" {
		return nil, app.E(app.CodeToolMissing, "启用了 VAD 但未配置 VAD 模型").WithStage(app.StageTranscribe)
	}
	jsonPath := req.OutPrefix + ".json"
	// A stale file from an interrupted run must never be read as this run's
	// result.
	if err := os.Remove(jsonPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, asrFailed("无法清理旧的转写输出").WithCause(err)
	}

	var mu sync.Mutex
	last := 0.0
	onLine := func(line string) {
		if req.Progress == nil {
			return
		}
		f, ok := parseProgress(line)
		if !ok {
			return
		}
		// 1.0 is reported once the JSON has been read successfully.
		if f > 0.99 {
			f = 0.99
		}
		mu.Lock()
		advance := f > last
		if advance {
			last = f
		}
		mu.Unlock()
		if advance {
			req.Progress(f)
		}
	}
	res, err := w.Runner.Run(ctx, procx.Cmd{
		Path: w.Bin, Args: w.transcribeArgs(req),
		OnStdoutLine: onLine, OnStderrLine: onLine,
		Stage: app.StageTranscribe,
	})
	if err != nil {
		_ = os.Remove(jsonPath) // possibly half-written
		return nil, wrap(ctx, err, res)
	}

	data, err := os.ReadFile(jsonPath)
	if err != nil {
		e := asrFailed("whisper-cli 没有生成转写结果").WithCause(err)
		if res != nil {
			e = e.WithDetail("stderr_tail", res.StderrTail)
		}
		return nil, e
	}
	out, err := ParseWhisperJSON(data)
	if err != nil {
		return nil, asrFailed("无法解析 whisper-cli 的输出").WithCause(err).WithDetail("error", err.Error())
	}
	out.RawJSONPath = jsonPath
	if out.Language == "" || out.Language == "auto" {
		out.Language = req.Language
	}
	if req.Progress != nil {
		req.Progress(1)
	}
	return out, nil
}

// parseProgress reads a `-pp` line and returns the fraction done.
func parseProgress(line string) (float64, bool) {
	if !strings.Contains(line, "progress") {
		return 0, false
	}
	m := reProgress.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	pct, err := strconv.Atoi(m[1])
	if err != nil || pct < 0 {
		return 0, false
	}
	if pct > 100 {
		pct = 100
	}
	return float64(pct) / 100, true
}

// whisperJSON is the part of whisper-cli's `-oj` output that is used. The
// full shape (examples/cli/cli.cpp, output_json) is:
//
//	{"systeminfo": "...", "model": {...},
//	 "params": {"model": "...", "language": "zh", "translate": false},
//	 "result": {"language": "zh"},
//	 "transcription": [
//	   {"timestamps": {"from": "00:00:00,000", "to": "00:00:04,200"},
//	    "offsets": {"from": 0, "to": 4200}, "text": " ..."}]}
type whisperJSON struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Params struct {
		Language string `json:"language"`
	} `json:"params"`
	Transcription *[]struct {
		Offsets *struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		Text string `json:"text"`
	} `json:"transcription"`
}

// ParseWhisperJSON parses whisper-cli's `-oj` output. Offsets are already in
// milliseconds. Text is returned verbatim; PostProcess does the cleaning.
func ParseWhisperJSON(data []byte) (*Result, error) {
	var raw whisperJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw.Transcription == nil {
		return nil, errors.New("missing \"transcription\" array")
	}
	out := &Result{Language: raw.Result.Language, Segments: make([]RawSegment, 0, len(*raw.Transcription))}
	if out.Language == "" {
		out.Language = raw.Params.Language
	}
	for i, s := range *raw.Transcription {
		if s.Offsets == nil {
			return nil, fmt.Errorf("transcription[%d] has no offsets", i)
		}
		out.Segments = append(out.Segments, RawSegment{StartMs: s.Offsets.From, EndMs: s.Offsets.To, Text: s.Text})
	}
	return out, nil
}

// Version returns the whisper.cpp version reported by `whisper-cli
// --version`, or "unknown" when the build does not support the flag. It is
// meant for the transcribe stage fingerprint, together with the model hash.
func (w *WhisperCpp) Version(ctx context.Context) string {
	if w.Runner == nil || w.Bin == "" {
		return "unknown"
	}
	var out strings.Builder
	res, err := w.Runner.Run(ctx, procx.Cmd{Path: w.Bin, Args: []string{"--version"}, Stdout: &out, Stage: app.StageTranscribe})
	if err != nil {
		return "unknown"
	}
	text := out.String()
	if res != nil {
		text += "\n" + res.StderrTail
	}
	return parseVersion(text)
}

func parseVersion(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(strings.ToLower(line), "version") {
			continue
		}
		if m := reVersion.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return "unknown"
}
