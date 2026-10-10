package asr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"watchink/internal/infra/procx"
)

// TestIntegrationWhisper runs the real whisper-cli. It is skipped unless
//
//	WATCHINK_WHISPER_BIN    whisper-cli executable
//	WATCHINK_WHISPER_MODEL  ggml model file
//	WATCHINK_WHISPER_AUDIO  16 kHz mono WAV with English speech
//
// are set. Optional: WATCHINK_WHISPER_VAD_MODEL (also runs with --vad)
// and WATCHINK_WHISPER_EXPECT (lower-case words the transcript must
// contain, space separated).
func TestIntegrationWhisper(t *testing.T) {
	bin, model, audio := os.Getenv("WATCHINK_WHISPER_BIN"), os.Getenv("WATCHINK_WHISPER_MODEL"), os.Getenv("WATCHINK_WHISPER_AUDIO")
	if bin == "" || model == "" {
		t.Skip("set WATCHINK_WHISPER_BIN and WATCHINK_WHISPER_MODEL to run the whisper integration test")
	}
	if audio == "" {
		t.Skip("set WATCHINK_WHISPER_AUDIO to a 16 kHz mono WAV with English speech")
	}
	vad := os.Getenv("WATCHINK_WHISPER_VAD_MODEL")
	w := NewWhisperCpp(procx.New(procx.Options{}), bin, model, vad)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	lang, err := w.DetectLanguage(ctx, DetectRequest{Audio: audio})
	if err != nil {
		t.Fatalf("DetectLanguage: %v", err)
	}
	if lang != "en" {
		t.Errorf("detected language = %q, want en", lang)
	}

	run := func(name string, useVAD bool) {
		t.Run(name, func(t *testing.T) {
			var prog []float64
			start := time.Now()
			res, err := w.Transcribe(ctx, Request{
				Audio: audio, Language: lang, Prompt: PromptFor(lang), VAD: useVAD,
				OutPrefix: filepath.Join(t.TempDir(), "whisper"),
				Progress:  func(f float64) { prog = append(prog, f) },
			})
			if err != nil {
				t.Fatalf("Transcribe: %v", err)
			}
			t.Logf("transcribed in %v, %d raw segments, progress %v", time.Since(start).Round(time.Millisecond), len(res.Segments), prog)
			if res.Language != "en" || len(res.Segments) == 0 {
				t.Fatalf("result = %+v", res)
			}
			if _, err := os.Stat(res.RawJSONPath); err != nil {
				t.Errorf("raw JSON missing: %v", err)
			}
			if len(prog) == 0 || prog[len(prog)-1] != 1 {
				t.Errorf("progress = %v", prog)
			}
			segs, meta := PostProcess(res.Segments, 0)
			if len(segs) == 0 {
				t.Fatalf("no segments after post-processing (meta %+v)", meta)
			}
			var text strings.Builder
			var prev int64
			for _, s := range segs {
				if s.StartMs >= s.EndMs || s.StartMs < prev {
					t.Errorf("bad segment times: %+v", s)
				}
				prev = s.StartMs
				text.WriteString(strings.ToLower(s.Text) + " ")
			}
			t.Logf("text: %s", text.String())
			for _, word := range strings.Fields(os.Getenv("WATCHINK_WHISPER_EXPECT")) {
				if !strings.Contains(text.String(), word) {
					t.Errorf("transcript does not contain %q", word)
				}
			}
		})
	}
	run("plain", false)
	if vad != "" {
		run("vad", true)
	}
}
