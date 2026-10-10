package asr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"watchink/internal/app"
	"watchink/internal/infra/procx"
)

func rs(start, end int64, text string) RawSegment { return RawSegment{start, end, text} }
func sg(start, end int64, text string) Segment    { return Segment{start, end, text} }

func TestPostProcess(t *testing.T) {
	tests := []struct {
		name     string
		raw      []RawSegment
		offset   int64
		want     []Segment
		wantMeta Meta
	}{
		{
			name: "nil input",
			want: []Segment{},
		},
		{
			name: "trim and drop empty",
			raw:  []RawSegment{rs(0, 1000, "  hello \n"), rs(1000, 2000, "   "), rs(2000, 3000, ""), rs(3000, 4000, "\t世界。")},
			want: []Segment{sg(0, 1000, "hello"), sg(3000, 4000, "世界。")},
			wantMeta: Meta{
				RawSegments: 4, DroppedEmpty: 2,
			},
		},
		{
			name:     "two identical segments are kept",
			raw:      []RawSegment{rs(0, 1000, "好的"), rs(1000, 2000, "好的"), rs(2000, 3000, "下一题")},
			want:     []Segment{sg(0, 1000, "好的"), sg(1000, 2000, "好的"), sg(2000, 3000, "下一题")},
			wantMeta: Meta{RawSegments: 3},
		},
		{
			name:     "three identical segments collapse into the first",
			raw:      []RawSegment{rs(0, 1000, "a"), rs(1000, 2000, "谢谢"), rs(2000, 3000, "谢谢"), rs(3000, 4000, "谢谢"), rs(4000, 5000, "b")},
			want:     []Segment{sg(0, 1000, "a"), sg(1000, 2000, "谢谢"), sg(4000, 5000, "b")},
			wantMeta: Meta{RawSegments: 5, CollapsedRuns: 1, CollapsedSegments: 2},
		},
		{
			name: "identity is judged after trimming, and empties do not break a run",
			raw:  []RawSegment{rs(0, 1, " x"), rs(1, 2, "x "), rs(2, 3, " "), rs(3, 4, "x"), rs(4, 5, "x")},
			want: []Segment{sg(0, 1, "x")},
			wantMeta: Meta{
				RawSegments: 5, DroppedEmpty: 1, CollapsedRuns: 1, CollapsedSegments: 3,
			},
		},
		{
			name: "several runs",
			raw: []RawSegment{
				rs(0, 10, "a"), rs(10, 20, "a"), rs(20, 30, "a"),
				rs(30, 40, "b"), rs(40, 50, "b"),
				rs(50, 60, "c"), rs(60, 70, "c"), rs(70, 80, "c"), rs(80, 90, "c"),
				rs(90, 100, "a"),
			},
			want:     []Segment{sg(0, 10, "a"), sg(30, 40, "b"), sg(40, 50, "b"), sg(50, 60, "c"), sg(90, 100, "a")},
			wantMeta: Meta{RawSegments: 10, CollapsedRuns: 2, CollapsedSegments: 5},
		},
		{
			name:     "case differences are not identical",
			raw:      []RawSegment{rs(0, 1, "Ok"), rs(1, 2, "ok"), rs(2, 3, "OK")},
			want:     []Segment{sg(0, 1, "Ok"), sg(1, 2, "ok"), sg(2, 3, "OK")},
			wantMeta: Meta{RawSegments: 3},
		},
		{
			name:     "positive offset shifts everything",
			raw:      []RawSegment{rs(0, 1000, "a"), rs(1000, 2500, "b")},
			offset:   500,
			want:     []Segment{sg(500, 1500, "a"), sg(1500, 3000, "b")},
			wantMeta: Meta{TimelineOffsetMs: 500, RawSegments: 2},
		},
		{
			name:     "negative offset clamps at zero",
			raw:      []RawSegment{rs(0, 20, "a"), rs(20, 1000, "b"), rs(1000, 2000, "c")},
			offset:   -40,
			want:     []Segment{sg(0, 1, "a"), sg(0, 960, "b"), sg(960, 1960, "c")},
			wantMeta: Meta{TimelineOffsetMs: -40, RawSegments: 3, AdjustedTimes: 1},
		},
		{
			name:     "zero-length and inverted segments get start < end",
			raw:      []RawSegment{rs(1000, 1000, "a"), rs(2000, 1500, "b"), rs(3000, 4000, "c")},
			want:     []Segment{sg(1000, 1001, "a"), sg(2000, 2001, "b"), sg(3000, 4000, "c")},
			wantMeta: Meta{RawSegments: 3, AdjustedTimes: 2},
		},
		{
			name:     "start times are made non-decreasing",
			raw:      []RawSegment{rs(0, 2000, "a"), rs(5000, 6000, "b"), rs(4000, 7000, "c"), rs(3000, 3500, "d"), rs(8000, 9000, "e")},
			want:     []Segment{sg(0, 2000, "a"), sg(5000, 6000, "b"), sg(5000, 7000, "c"), sg(5000, 5001, "d"), sg(8000, 9000, "e")},
			wantMeta: Meta{RawSegments: 5, AdjustedTimes: 2},
		},
		{
			name:     "overlapping ends are left alone",
			raw:      []RawSegment{rs(0, 3000, "a"), rs(2000, 2500, "b")},
			want:     []Segment{sg(0, 3000, "a"), sg(2000, 2500, "b")},
			wantMeta: Meta{RawSegments: 2},
		},
		{
			name:     "everything empty",
			raw:      []RawSegment{rs(0, 1, " "), rs(1, 2, "\n")},
			want:     []Segment{},
			wantMeta: Meta{RawSegments: 2, DroppedEmpty: 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]RawSegment(nil), tt.raw...)
			got, meta := PostProcess(tt.raw, tt.offset)
			if got == nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("segments:\n got %+v\nwant %+v", got, tt.want)
			}
			if meta != tt.wantMeta {
				t.Errorf("meta:\n got %+v\nwant %+v", meta, tt.wantMeta)
			}
			if !reflect.DeepEqual(in, tt.raw) {
				t.Error("input slice was modified")
			}
			// Invariants, whatever the input.
			var prev int64
			for i, s := range got {
				if s.StartMs >= s.EndMs || s.StartMs < prev || s.StartMs < 0 || s.Text == "" || s.Text != strings.TrimSpace(s.Text) {
					t.Errorf("segment %d violates the invariants: %+v", i, s)
				}
				prev = s.StartMs
			}
		})
	}
}

// sampleJSON mirrors what whisper-cli -oj writes (examples/cli/cli.cpp).
const sampleJSON = `{
	"systeminfo": "WHISPER : COREML = 0 | OPENVINO = 0 | Metal : EMBED_LIBRARY = 1 | CPU : NEON = 1 | ARM_FMA = 1 |",
	"model": {
		"type": "large",
		"multilingual": true,
		"vocab": 51866,
		"audio": {"ctx": 1500, "state": 1280, "head": 20, "layer": 32},
		"text": {"ctx": 448, "state": 1280, "head": 20, "layer": 4},
		"mels": 128,
		"ftype": 8
	},
	"params": {
		"model": "/models/ggml-large-v3-turbo-q5_0.bin",
		"language": "zh",
		"translate": false
	},
	"result": {
		"language": "zh"
	},
	"transcription": [
		{
			"timestamps": {"from": "00:00:00,000", "to": "00:00:04,200"},
			"offsets": {"from": 0, "to": 4200},
			"text": " 大家好，今天我们讲\"梯度下降\"。"
		},
		{
			"timestamps": {"from": "00:00:04,200", "to": "00:00:09,960"},
			"offsets": {"from": 4200, "to": 9960},
			"text": " The loss is L = (y - ŷ)²\n"
		},
		{
			"timestamps": {"from": "01:02:03,450", "to": "01:02:05,000"},
			"offsets": {"from": 3723450, "to": 3725000},
			"text": ""
		}
	]
}`

func TestParseWhisperJSON(t *testing.T) {
	res, err := ParseWhisperJSON([]byte(sampleJSON))
	if err != nil {
		t.Fatal(err)
	}
	want := []RawSegment{
		rs(0, 4200, ` 大家好，今天我们讲"梯度下降"。`),
		rs(4200, 9960, " The loss is L = (y - ŷ)²\n"),
		rs(3723450, 3725000, ""),
	}
	if res.Language != "zh" || !reflect.DeepEqual(res.Segments, want) {
		t.Errorf("result = %+v", res)
	}

	// -ojf adds per-token data; unknown fields are ignored.
	full := `{"result":{"language":"en"},"transcription":[{"timestamps":{"from":"00:00:00,000","to":"00:00:01,000"},
		"offsets":{"from":0,"to":1000},"text":" Hi","tokens":[{"text":" Hi","id":1,"p":0.9,"t_dtw":-1}],"speaker":"0"}]}`
	if res, err = ParseWhisperJSON([]byte(full)); err != nil || len(res.Segments) != 1 || res.Segments[0].EndMs != 1000 || res.Language != "en" {
		t.Errorf("full json: %+v, %v", res, err)
	}

	// Silence: an empty transcription array is a valid result.
	if res, err = ParseWhisperJSON([]byte(`{"params":{"language":"en"},"transcription":[]}`)); err != nil || res.Segments == nil || len(res.Segments) != 0 || res.Language != "en" {
		t.Errorf("empty transcription: %+v, %v", res, err)
	}

	for name, in := range map[string]string{
		"truncated":        sampleJSON[:len(sampleJSON)/2],
		"no transcription": `{"result":{"language":"en"}}`,
		"no offsets":       `{"transcription":[{"text":"x"}]}`,
		"not json":         "whisper_init_from_file_with_params_no_state: failed",
		"empty":            "",
	} {
		t.Run(name, func(t *testing.T) {
			if res, err := ParseWhisperJSON([]byte(in)); err == nil {
				t.Errorf("accepted: %+v", res)
			}
		})
	}
}

func TestParseProgressAndPatterns(t *testing.T) {
	progress := []struct {
		line   string
		want   float64
		wantOK bool
	}{
		{"whisper_print_progress_callback: progress =   5%", 0.05, true},
		{"whisper_print_progress_callback: progress =  45%", 0.45, true},
		{"whisper_print_progress_callback: progress = 100%", 1, true},
		{"progress = 250%", 1, true},
		{"[00:00:00.000 --> 00:00:04.000]   we made 50% progress today", 0, false},
		{"whisper_full_with_state: auto-detected language: en (p = 0.97)", 0, false},
		{"", 0, false},
	}
	for _, tt := range progress {
		if got, ok := parseProgress(tt.line); ok != tt.wantOK || got != tt.want {
			t.Errorf("parseProgress(%q) = %v, %v; want %v, %v", tt.line, got, ok, tt.want, tt.wantOK)
		}
	}

	langs := map[string]string{
		"whisper_full_with_state: auto-detected language: en (p = 0.977899)":       "en",
		"whisper_full_with_state: auto-detected language: zh (p = 0.5)":            "zh",
		"whisper_full_with_state: auto-detected language: haw (p = 0.1)":           "haw",
		"main: processing 'a.wav' (48000 samples), lang = auto, task = transcribe": "",
	}
	for line, want := range langs {
		got := ""
		if m := reDetected.FindStringSubmatch(line); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("detected language in %q = %q, want %q", line, got, want)
		}
	}
}

func TestPromptAndThreads(t *testing.T) {
	if PromptZH != "以下是普通话课程的转写，使用简体中文和标点。" {
		t.Errorf("PromptZH = %q", PromptZH)
	}
	for lang, want := range map[string]string{"zh": PromptZH, "ZH": PromptZH, " zh ": PromptZH, "en": "", "ja": "", "": "", "auto": ""} {
		if got := PromptFor(lang); got != want {
			t.Errorf("PromptFor(%q) = %q", lang, got)
		}
	}
	if n := DefaultThreads(); n < 1 || n > MaxThreads {
		t.Errorf("DefaultThreads() = %d", n)
	}
	if n := physicalCores(); n < 1 {
		t.Errorf("physicalCores() = %d", n)
	}
}

func TestCommandLines(t *testing.T) {
	w := NewWhisperCpp(nil, "/t/whisper-cli", "/m/ggml.bin", "/m/silero.bin")
	tests := []struct {
		name string
		req  Request
		want string
	}{
		{"zh with prompt and vad", Request{Audio: "/d/audio.wav", Language: "zh", Threads: 6, Prompt: PromptZH, VAD: true, OutPrefix: "/d/whisper"},
			"-m /m/ggml.bin -f /d/audio.wav -l zh -t 6 -oj -of /d/whisper -pp --prompt " + PromptZH + " --vad --vad-model /m/silero.bin"},
		{"en plain", Request{Audio: "/d/audio.wav", Language: "en", Threads: 4, OutPrefix: "/d/whisper"},
			"-m /m/ggml.bin -f /d/audio.wav -l en -t 4 -oj -of /d/whisper -pp"},
		{"vad only", Request{Audio: "a.wav", Language: "en", Threads: 1, VAD: true, OutPrefix: "o"},
			"-m /m/ggml.bin -f a.wav -l en -t 1 -oj -of o -pp --vad --vad-model /m/silero.bin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strings.Join(w.transcribeArgs(tt.req), " "); got != tt.want {
				t.Errorf("args:\n got %s\nwant %s", got, tt.want)
			}
		})
	}
	// The prompt is one argv element, never split by a shell.
	args := w.transcribeArgs(Request{Audio: "a", Language: "zh", Threads: 1, Prompt: "a b; rm -rf $HOME", OutPrefix: "o"})
	if args[len(args)-1] != "a b; rm -rf $HOME" {
		t.Errorf("prompt argument = %q", args[len(args)-1])
	}
	if got := strings.Join(w.detectArgs(DetectRequest{Audio: "/d/audio.wav", Threads: 3}), " "); got != "-m /m/ggml.bin -f /d/audio.wav -t 3 -dl" {
		t.Errorf("detect args = %s", got)
	}
	// Default thread count.
	if args := w.detectArgs(DetectRequest{Audio: "a"}); args[5] != threadArg(0) || args[5] == "0" {
		t.Errorf("default threads arg = %q", args[5])
	}
}

// fakeWhisper imitates whisper-cli closely enough to exercise the real code
// path through procx: argument handling, stderr progress, the -dl line and
// the JSON file. FAKE_MODE selects the failure to simulate.
const fakeWhisper = `#!/bin/sh
out=""; lang=""; detect=0; prompt=""; vad=0; prev=""
for a in "$@"; do
  case "$prev" in
    -of) out="$a" ;;
    -l) lang="$a" ;;
    --prompt) prompt="$a" ;;
  esac
  case "$a" in
    -dl) detect=1 ;;
    --vad) vad=1 ;;
    --version) echo "whisper.cpp version: 1.8.2"; exit 0 ;;
  esac
  prev="$a"
done
echo "whisper_init_from_file_with_params_no_state: loading model" 1>&2
case "$FAKE_MODE" in
  crash) echo "error: failed to read audio file" 1>&2; exit 3 ;;
  hang) sleep 300 ;;
  nodisk) echo "failed to open '$out.json' for writing: No space left on device" 1>&2; exit 1 ;;
esac
if [ "$detect" = 1 ]; then
  if [ "$FAKE_MODE" = nolang ]; then exit 0; fi
  echo "whisper_full_with_state: auto-detected language: ${FAKE_LANG:-zh} (p = 0.987654)" 1>&2
  exit 0
fi
for p in 10 5 40 80 100; do
  echo "whisper_print_progress_callback: progress = $p%" 1>&2
done
echo "[00:00:00.000 --> 00:00:02.000]   progress = 99% is just transcript text on stdout? no: stdout"
if [ "$FAKE_MODE" = nojson ]; then exit 0; fi
if [ "$FAKE_MODE" = badjson ]; then printf '{"transcription": [' > "$out.json"; exit 0; fi
cat > "$out.json" <<EOF
{
	"params": {"model": "m", "language": "$lang", "translate": false},
	"result": {"language": "$lang"},
	"transcription": [
		{"timestamps": {"from": "00:00:00,000", "to": "00:00:02,000"}, "offsets": {"from": 0, "to": 2000}, "text": " prompt=[$prompt] vad=$vad"},
		{"timestamps": {"from": "00:00:02,000", "to": "00:00:02,000"}, "offsets": {"from": 2000, "to": 2000}, "text": "  "},
		{"timestamps": {"from": "00:00:02,000", "to": "00:00:03,000"}, "offsets": {"from": 2000, "to": 3000}, "text": " 谢谢"},
		{"timestamps": {"from": "00:00:03,000", "to": "00:00:04,000"}, "offsets": {"from": 3000, "to": 4000}, "text": " 谢谢"},
		{"timestamps": {"from": "00:00:04,000", "to": "00:00:05,000"}, "offsets": {"from": 4000, "to": 5000}, "text": " 谢谢"}
	]
}
EOF
`

func newFake(t *testing.T, mode string, extraEnv ...string) (*WhisperCpp, string) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "whisper-cli")
	if err := os.WriteFile(bin, []byte(fakeWhisper), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kv := range append(extraEnv, "FAKE_MODE="+mode) {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	return NewWhisperCpp(procx.New(procx.Options{TermGrace: 300 * time.Millisecond}), bin, "/m/ggml.bin", "/m/silero.bin"), dir
}

func TestFakeWhisperTranscribe(t *testing.T) {
	w, dir := newFake(t, "")
	var prog []float64
	res, err := w.Transcribe(context.Background(), Request{
		Audio: "/d/audio.wav", Language: "ZH", Prompt: PromptFor("zh"), VAD: true,
		OutPrefix: filepath.Join(dir, "whisper"), DurationMs: 5000,
		Progress: func(f float64) { prog = append(prog, f) },
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if res.Language != "zh" || res.RawJSONPath != filepath.Join(dir, "whisper.json") || len(res.Segments) != 5 {
		t.Fatalf("result = %+v", res)
	}
	// The prompt reached the tool as a single, unmangled argument.
	if want := " prompt=[" + PromptZH + "] vad=1"; res.Segments[0].Text != want {
		t.Errorf("segment 0 = %q, want %q", res.Segments[0].Text, want)
	}
	// Monotonic progress from stderr only, 1.0 exactly once at the end.
	if want := []float64{0.1, 0.4, 0.8, 0.99, 1}; !reflect.DeepEqual(prog, want) {
		t.Errorf("progress = %v, want %v", prog, want)
	}
	// The raw JSON stays on disk untouched and parses to the same result.
	data, err := os.ReadFile(res.RawJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseWhisperJSON(data)
	if err != nil || !reflect.DeepEqual(again.Segments, res.Segments) {
		t.Errorf("raw JSON does not match the result: %v", err)
	}

	segs, meta := PostProcess(res.Segments, 120)
	wantSegs := []Segment{sg(120, 2120, "prompt=["+PromptZH+"] vad=1"), sg(2120, 3120, "谢谢")}
	wantMeta := Meta{TimelineOffsetMs: 120, RawSegments: 5, DroppedEmpty: 1, CollapsedRuns: 1, CollapsedSegments: 2}
	if !reflect.DeepEqual(segs, wantSegs) || meta != wantMeta {
		t.Errorf("PostProcess = %+v, %+v", segs, meta)
	}
}

func TestFakeWhisperDetectAndVersion(t *testing.T) {
	w, _ := newFake(t, "", "FAKE_LANG=ja")
	lang, err := w.DetectLanguage(context.Background(), DetectRequest{Audio: "/d/audio.wav"})
	if err != nil || lang != "ja" {
		t.Errorf("DetectLanguage = %q, %v; want ja (a non zh/en language is not an error)", lang, err)
	}
}

func TestFakeWhisperFailures(t *testing.T) {
	req := func(dir string) Request {
		return Request{Audio: "/d/audio.wav", Language: "en", OutPrefix: filepath.Join(dir, "whisper")}
	}
	for mode, want := range map[string]app.Code{
		"crash":   app.CodeASRFailed,
		"nojson":  app.CodeASRFailed,
		"badjson": app.CodeASRFailed,
		"nodisk":  app.CodeDiskFull,
	} {
		t.Run(mode, func(t *testing.T) {
			w, dir := newFake(t, mode)
			_, err := w.Transcribe(context.Background(), req(dir))
			if !app.IsCode(err, want) {
				t.Fatalf("err = %v, want %s", err, want)
			}
			if e := app.AsError(err); e.Stage != app.StageTranscribe {
				t.Errorf("stage = %q", e.Stage)
			}
			if mode == "crash" {
				e := app.AsError(err)
				if tail, _ := e.Details["stderr_tail"].(string); !strings.Contains(tail, "failed to read audio file") || e.Details["exit_code"] != 3 {
					t.Errorf("details = %v", e.Details)
				}
				if !e.Retryable {
					t.Error("ASR_FAILED must be retryable")
				}
			}
		})
	}

	t.Run("detect crash", func(t *testing.T) {
		w, _ := newFake(t, "crash")
		if _, err := w.DetectLanguage(context.Background(), DetectRequest{Audio: "a.wav"}); !app.IsCode(err, app.CodeASRFailed) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("detect without a language line", func(t *testing.T) {
		w, _ := newFake(t, "nolang")
		if _, err := w.DetectLanguage(context.Background(), DetectRequest{Audio: "a.wav"}); !app.IsCode(err, app.CodeASRFailed) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("stale json is never reused", func(t *testing.T) {
		w, dir := newFake(t, "nojson")
		stale := filepath.Join(dir, "whisper.json")
		if err := os.WriteFile(stale, []byte(sampleJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		if res, err := w.Transcribe(context.Background(), req(dir)); !app.IsCode(err, app.CodeASRFailed) {
			t.Errorf("stale output was accepted: %+v, %v", res, err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		w, dir := newFake(t, "hang")
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := w.Transcribe(ctx, req(dir))
		if !errors.Is(err, context.DeadlineExceeded) || app.IsCode(err, app.CodeASRFailed) {
			t.Errorf("err = %v, want the context error", err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("cancel took %v", took)
		}
	})
	t.Run("missing binary", func(t *testing.T) {
		w := NewWhisperCpp(procx.New(procx.Options{}), filepath.Join(t.TempDir(), "nope"), "/m/ggml.bin", "")
		if _, err := w.Transcribe(context.Background(), req(t.TempDir())); !app.IsCode(err, app.CodeToolMissing) {
			t.Errorf("err = %v, want TOOL_MISSING", err)
		}
	})
}

func TestTranscribeValidation(t *testing.T) {
	w := NewWhisperCpp(procx.New(procx.Options{}), "/bin/echo", "/m/ggml.bin", "")
	dir := t.TempDir()
	tests := []struct {
		name string
		w    *WhisperCpp
		req  Request
		want app.Code
	}{
		{"no language", w, Request{Audio: "a.wav", OutPrefix: dir + "/o"}, app.CodeASRFailed},
		{"language is not a flag", w, Request{Audio: "a.wav", Language: "--help", OutPrefix: dir + "/o"}, app.CodeASRFailed},
		{"no output prefix", w, Request{Audio: "a.wav", Language: "en"}, app.CodeASRFailed},
		{"no audio", w, Request{Language: "en", OutPrefix: dir + "/o"}, app.CodeASRFailed},
		{"vad without model", w, Request{Audio: "a.wav", Language: "en", VAD: true, OutPrefix: dir + "/o"}, app.CodeToolMissing},
		{"no binary", &WhisperCpp{Runner: w.Runner, Model: "m"}, Request{Audio: "a.wav", Language: "en", OutPrefix: dir + "/o"}, app.CodeToolMissing},
		{"no model", &WhisperCpp{Runner: w.Runner, Bin: "/bin/echo"}, Request{Audio: "a.wav", Language: "en", OutPrefix: dir + "/o"}, app.CodeToolMissing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.w.Transcribe(context.Background(), tt.req); !app.IsCode(err, tt.want) {
				t.Errorf("err = %v, want %s", err, tt.want)
			}
		})
	}
}
