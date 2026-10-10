// Package asr turns a 16 kHz mono WAV into timed transcript segments.
// The only engine in v0 is whisper.cpp's whisper-cli,
// run as one child process per transcription through an injected procx
// runner. Post-processing of the raw segments is a pure function.
package asr

import (
	"context"
	"strings"

	"watchink/internal/infra/procx"
)

// PromptZH is the initial prompt used for Chinese audio. English, and
// every other language, gets no prompt.
const PromptZH = "以下是普通话课程的转写，使用简体中文和标点。"

// MaxThreads caps the default thread count.
const MaxThreads = 8

// Runner is the part of *procx.Runner this package needs.
type Runner interface {
	Run(ctx context.Context, cmd procx.Cmd) (*procx.Result, error)
}

// Transcriber is a speech-to-text engine.
type Transcriber interface {
	// DetectLanguage returns the language code detected in the audio
	// ("zh", "en", "ja", …). A language outside zh/en is not an error.
	DetectLanguage(ctx context.Context, req DetectRequest) (string, error)
	// Transcribe transcribes the whole file and returns the raw segments.
	Transcribe(ctx context.Context, req Request) (*Result, error)
}

// DetectRequest is the input of DetectLanguage.
type DetectRequest struct {
	// Audio is the 16 kHz mono WAV.
	Audio string
	// Threads <= 0 means DefaultThreads().
	Threads int
}

// Request is the input of Transcribe.
type Request struct {
	// Audio is the 16 kHz mono WAV.
	Audio string
	// Language is the spoken language code, e.g. "zh" or "en" (
	// resolved by configuration or DetectLanguage before transcribing).
	Language string
	// Threads <= 0 means DefaultThreads().
	Threads int
	// Prompt is the initial prompt; use PromptFor(Language) for the default.
	Prompt string
	// VAD enables voice-activity detection; it needs a VAD model.
	VAD bool
	// OutPrefix is the output path without extension: whisper writes
	// <OutPrefix>.json, which is kept untouched as the raw artifact.
	OutPrefix string
	// DurationMs is the audio duration (informational; whisper reports
	// progress in percent on its own).
	DurationMs int64
	// Progress receives values in [0,1].
	Progress func(float64)
}

// RawSegment is one segment exactly as the engine produced it; times are
// relative to the start of the audio file.
type RawSegment struct {
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
	Text    string `json:"text"`
}

// Result is the outcome of Transcribe.
type Result struct {
	// Language is the language the engine reports having used.
	Language string `json:"language"`
	// Segments are the unprocessed segments; feed them to PostProcess.
	Segments []RawSegment `json:"segments"`
	// RawJSONPath is the engine's own output file, kept as is.
	RawJSONPath string `json:"raw_json_path"`
}

// PromptFor returns the default initial prompt for a language: the
// fixed Mandarin prompt for Chinese, nothing otherwise.
func PromptFor(lang string) string {
	if strings.EqualFold(strings.TrimSpace(lang), "zh") {
		return PromptZH
	}
	return ""
}

// DefaultThreads is the default: the number of physical cores, at most 8.
func DefaultThreads() int {
	n := physicalCores()
	if n < 1 {
		n = 1
	}
	if n > MaxThreads {
		n = MaxThreads
	}
	return n
}
