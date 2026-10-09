package media

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"

	"watchink/internal/app"
	"watchink/internal/procx"
)

// PlaybackKind is the §10.1 decision for a source file.
type PlaybackKind string

const (
	// PlaybackDirect: serve the source file as is; nothing is generated.
	PlaybackDirect PlaybackKind = "direct"
	// PlaybackRemux: lossless re-mux into MP4 (`-c copy -movflags +faststart`).
	PlaybackRemux PlaybackKind = "remux"
	// PlaybackAudioTranscode: copy the video, encode the audio to AAC 128 kbps.
	PlaybackAudioTranscode PlaybackKind = "audio-transcode"
	// PlaybackFullTranscode: encode to H.264 (height-capped) plus AAC.
	PlaybackFullTranscode PlaybackKind = "full-transcode"

	// DefaultPlaybackMaxHeight is the D-11 height cap.
	DefaultPlaybackMaxHeight = 720
)

// Plan is the playback decision plus everything BuildPlayback needs. It is
// plain data so it can be stored with the stage output.
type Plan struct {
	Kind PlaybackKind `json:"kind"`
	// Reason says, for logs and the stage record, why this kind was chosen.
	Reason   string `json:"reason"`
	HasVideo bool   `json:"has_video"`
	HasAudio bool   `json:"has_audio"`
	// CopyAudio is true when the audio stream is kept as is.
	CopyAudio bool `json:"copy_audio"`
	// ScaleHeight is the target height of a full transcode, or 0 to keep the
	// source height (it is only set when the source is taller than the cap).
	ScaleHeight int `json:"scale_height,omitempty"`
	// Mime is the content type of the playback file: the source for a direct
	// plan, otherwise the generated MP4.
	Mime string `json:"mime"`
}

// NeedsBuild reports whether BuildPlayback has to produce a file.
func (p Plan) NeedsBuild() bool { return p.Kind != PlaybackDirect }

func hasFormat(info *ProbeInfo, names ...string) bool {
	for _, f := range info.FormatNames {
		for _, n := range names {
			if f == n {
				return true
			}
		}
	}
	return false
}

// PlanPlayback applies the §10.1 table:
//
//	MP4/MOV/M4V + H.264 + AAC/MP3   → direct
//	other container, same codecs    → remux
//	H.264 + incompatible audio      → audio-transcode
//	incompatible video              → full-transcode (height ≤ maxHeight)
//
// Two cases the table leaves implicit are decided here: H.264 that is not
// 8-bit 4:2:0 (Hi10P, 4:2:2, 4:4:4) is treated as incompatible because
// WebViews cannot decode it, and audio-only sources follow the same rules
// with the audio codec alone (an MP3 file is also played directly).
// maxHeight <= 0 means DefaultPlaybackMaxHeight.
func PlanPlayback(info *ProbeInfo, maxHeight int) Plan {
	if maxHeight <= 0 {
		maxHeight = DefaultPlaybackMaxHeight
	}
	v, a := info.VideoStream(), info.AudioStream()
	plan := Plan{HasVideo: v != nil, HasAudio: a != nil, Mime: "video/mp4"}

	mp4Family := hasFormat(info, "mov", "mp4", "m4a")
	audioOK := a == nil || a.CodecName == "aac" || a.CodecName == "mp3"
	plan.CopyAudio = a != nil && audioOK

	if v == nil {
		plan.Mime = "audio/mp4"
		switch {
		case audioOK && mp4Family:
			plan.Kind, plan.Reason = PlaybackDirect, "audio-only MP4 container with AAC/MP3"
		case a != nil && a.CodecName == "mp3" && hasFormat(info, "mp3"):
			plan.Kind, plan.Reason, plan.Mime = PlaybackDirect, "MP3 file", "audio/mpeg"
		case audioOK:
			plan.Kind, plan.Reason = PlaybackRemux, "audio-only, compatible codec in another container"
		default:
			plan.Kind, plan.Reason = PlaybackAudioTranscode, "audio-only, incompatible audio codec "+a.CodecName
		}
		return plan
	}

	videoOK := v.CodecName == "h264" && (v.PixFmt == "" || v.PixFmt == "yuv420p" || v.PixFmt == "yuvj420p")
	switch {
	case !videoOK:
		plan.Kind = PlaybackFullTranscode
		plan.Reason = "incompatible video codec " + v.CodecName
		if v.CodecName == "h264" {
			plan.Reason = "H.264 with unsupported pixel format " + v.PixFmt
		}
		if v.Height > maxHeight {
			plan.ScaleHeight = maxHeight
		}
	case !audioOK:
		plan.Kind, plan.Reason = PlaybackAudioTranscode, "H.264 video, incompatible audio codec "+a.CodecName
	case !mp4Family:
		plan.Kind, plan.Reason = PlaybackRemux, "compatible codecs in a non-MP4 container"
	default:
		plan.Kind, plan.Reason = PlaybackDirect, "MP4/MOV container with H.264 and AAC/MP3"
	}
	return plan
}

// playbackArgs builds the ffmpeg arguments for a plan. No option that shifts
// timestamps (-ss, -itsoffset, -copyts, -start_at_zero, -avoid_negative_ts,
// setpts…) may ever be added here: citations rely on the playback file
// keeping the source timeline (§10.1, S-11).
func playbackArgs(plan Plan, src, tmp string) []string {
	args := []string{"-nostdin", "-hide_banner", "-y", "-i", src}
	// The same streams the rest of the pipeline uses: the first real video
	// stream (V excludes cover art) and the first audio stream (§8.1).
	if plan.HasVideo {
		args = append(args, "-map", "0:V:0")
	} else {
		args = append(args, "-vn")
	}
	if plan.HasAudio {
		args = append(args, "-map", "0:a:0")
	}
	args = append(args, "-sn", "-dn")

	audioEnc := []string{"-c:a", "aac", "-b:a", "128k"}
	switch plan.Kind {
	case PlaybackRemux:
		args = append(args, "-c", "copy")
	case PlaybackAudioTranscode:
		if plan.HasVideo {
			args = append(args, "-c:v", "copy")
		}
		args = append(args, audioEnc...)
	case PlaybackFullTranscode:
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-pix_fmt", "yuv420p")
		if plan.ScaleHeight > 0 {
			args = append(args, "-vf", "scale=-2:"+strconv.Itoa(plan.ScaleHeight))
		}
		if plan.HasAudio {
			if plan.CopyAudio {
				args = append(args, "-c:a", "copy")
			} else {
				args = append(args, audioEnc...)
			}
		}
	}
	return append(args, "-movflags", "+faststart",
		"-progress", "pipe:1", "-nostats", "-f", "mp4", tmp)
}

// BuildPlayback produces the playback MP4 for a remux / transcode plan at
// dst (written to dst+".tmp" and renamed). It is an error to call it with a
// direct plan. progress receives values in [0,1].
func BuildPlayback(ctx context.Context, runner Runner, ffmpegPath string, plan Plan, src, dst string, durationMs int64, progress func(float64)) error {
	if !plan.NeedsBuild() {
		return app.E(app.CodeInternal, "direct 播放方案不需要生成文件").WithStage(app.StagePlayback)
	}
	tmp := dst + ".tmp"
	return runFFmpeg(ctx, runner, ffmpegPath, playbackArgs(plan, src, tmp), tmp, dst, app.StagePlayback, durationMs, progress)
}

// extractAudioArgs is the §8.1 command; -progress/-nostats only add machine
// readable progress and -f wav is needed because the temp name ends in .tmp.
func extractAudioArgs(src, tmp string) []string {
	return []string{
		"-nostdin", "-hide_banner", "-y", "-i", src,
		"-map", "0:a:0", "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le",
		"-progress", "pipe:1", "-nostats", "-f", "wav", tmp,
	}
}

// ExtractAudio writes the first audio stream of src as 16 kHz mono PCM WAV to
// dst (§8.1), via dst+".tmp" and a rename. progress receives values in [0,1].
func ExtractAudio(ctx context.Context, runner Runner, ffmpegPath, src, dst string, durationMs int64, progress func(float64)) error {
	tmp := dst + ".tmp"
	return runFFmpeg(ctx, runner, ffmpegPath, extractAudioArgs(src, tmp), tmp, dst, app.StageExtractAudio, durationMs, progress)
}

func runFFmpeg(ctx context.Context, runner Runner, ffmpegPath string, args []string, tmp, dst, stage string, durationMs int64, progress func(float64)) error {
	pp := newFFmpegProgress(durationMs, progress)
	res, err := runner.Run(ctx, procx.Cmd{Path: ffmpegPath, Args: args, OnStdoutLine: pp.line, Stage: stage})
	if err != nil {
		_ = os.Remove(tmp)
		return classifyFFmpegError(ctx, err, res, stage)
	}
	if st, serr := os.Stat(tmp); serr != nil || st.Size() == 0 {
		_ = os.Remove(tmp)
		return app.E(app.CodeToolFailed, "ffmpeg 没有生成输出文件").WithStage(stage)
	}
	if f, oerr := os.Open(tmp); oerr == nil {
		_ = f.Sync()
		f.Close()
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return WrapDiskFull(err, "")
	}
	if progress != nil {
		progress(1)
	}
	return nil
}

func classifyFFmpegError(ctx context.Context, err error, res *procx.Result, stage string) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, procx.ErrKilled) || !app.IsCode(err, app.CodeToolFailed) {
		return err
	}
	tail := ""
	if res != nil {
		tail = res.StderrTail
	}
	low := strings.ToLower(tail)
	var e *app.Error
	switch {
	case IsDiskFull(nil, tail):
		e = app.E(app.CodeDiskFull, "磁盘空间不足").WithHint("请清理磁盘空间后重试。")
	case strings.Contains(low, "matches no streams") && stage == app.StageExtractAudio:
		e = app.E(app.CodeNoAudioStream, "视频中没有音轨").WithHint("没有音轨的视频无法转写。")
	case strings.Contains(low, "invalid data found when processing input") ||
		strings.Contains(low, "moov atom not found"):
		e = app.E(app.CodeMediaInvalid, "媒体文件无法解码").WithHint("文件可能已损坏，或不是受支持的音视频格式。")
	default:
		return err
	}
	e = e.WithStage(stage).WithDetail("stderr_tail", tail).WithCause(err)
	if res != nil {
		e = e.WithDetail("exit_code", res.ExitCode)
	}
	return e
}

// ffmpegProgress turns `-progress pipe:1` key=value lines into a fraction.
type ffmpegProgress struct {
	durationMs int64
	fn         func(float64)
	last       float64
}

func newFFmpegProgress(durationMs int64, fn func(float64)) *ffmpegProgress {
	return &ffmpegProgress{durationMs: durationMs, fn: fn}
}

func (p *ffmpegProgress) line(line string) {
	if p.fn == nil {
		return
	}
	if f, ok := parseFFmpegProgress(line, p.durationMs); ok && f > p.last {
		// 1.0 is reported by the caller once the file has been renamed.
		if f > 0.99 {
			f = 0.99
		}
		if f > p.last {
			p.last = f
			p.fn(f)
		}
	}
}

// parseFFmpegProgress reads one progress line. out_time_us (and the
// misnamed out_time_ms, also microseconds) carry the output position.
func parseFFmpegProgress(line string, durationMs int64) (float64, bool) {
	key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
	if !ok {
		return 0, false
	}
	switch key {
	case "progress":
		if val == "end" {
			return 1, true
		}
	case "out_time_us", "out_time_ms":
		us, err := strconv.ParseInt(val, 10, 64)
		if err != nil || us < 0 || durationMs <= 0 {
			return 0, false
		}
		f := float64(us) / 1000 / float64(durationMs)
		if f > 1 {
			f = 1
		}
		return f, true
	}
	return 0, false
}
