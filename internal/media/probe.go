// Package media wraps the media tools (ffprobe, ffmpeg, yt-dlp) and the
// source-identification rules (URL normalization and dedup keys).
// It never creates processes itself: every tool runs through an injected
// Runner (procx), and tool paths are plain strings supplied by the caller.
package media

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"watchink/internal/app"
	"watchink/internal/procx"
)

// Runner is the part of *procx.Runner this package needs.
type Runner interface {
	Run(ctx context.Context, cmd procx.Cmd) (*procx.Result, error)
}

// ProbeInfo is the normalized ffprobe result. It is stored as an artifact, so
// its JSON shape must stay stable.
type ProbeInfo struct {
	DurationMs int64 `json:"duration_ms"`
	// FormatNames is ffprobe's comma-separated demuxer name list, split:
	// e.g. ["mov","mp4","m4a","3gp","3g2","mj2"] or ["matroska","webm"].
	FormatNames []string `json:"format_names"`
	// StartTime is the container start time in seconds.
	StartTime float64  `json:"start_time"`
	SizeBytes int64    `json:"size_bytes,omitempty"`
	BitRate   int64    `json:"bit_rate,omitempty"`
	Streams   []Stream `json:"streams"`
	// HasAudio / HasVideo ignore attached pictures (cover art).
	HasAudio bool `json:"has_audio"`
	HasVideo bool `json:"has_video"`
	// AudioStreamIndex is the ffprobe index of the first audio stream (the
	// one `-map 0:a:0` selects), or -1.
	AudioStreamIndex int `json:"audio_stream_index"`
	// VideoStreamIndex is the index of the first real video stream, or -1.
	VideoStreamIndex int `json:"video_stream_index"`
}

// Stream is one stream of a probed file.
type Stream struct {
	Index      int    `json:"index"`
	CodecType  string `json:"codec_type"` // video / audio / subtitle / data / attachment
	CodecName  string `json:"codec_name"`
	Profile    string `json:"profile,omitempty"`
	PixFmt     string `json:"pix_fmt,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
	// StartTime is the stream start time in seconds; HasStartTime is false
	// when ffprobe reported none.
	StartTime    float64 `json:"start_time"`
	HasStartTime bool    `json:"has_start_time"`
	DurationMs   int64   `json:"duration_ms,omitempty"`
	// AttachedPic marks cover art carried as a video stream.
	AttachedPic bool `json:"attached_pic,omitempty"`
}

// AudioStream returns the first audio stream, or nil.
func (p *ProbeInfo) AudioStream() *Stream { return p.streamAt(p.AudioStreamIndex) }

// VideoStream returns the first real (non cover-art) video stream, or nil.
func (p *ProbeInfo) VideoStream() *Stream { return p.streamAt(p.VideoStreamIndex) }

func (p *ProbeInfo) streamAt(index int) *Stream {
	if p == nil || index < 0 {
		return nil
	}
	for i := range p.Streams {
		if p.Streams[i].Index == index {
			return &p.Streams[i]
		}
	}
	return nil
}

// ffprobe prints numbers as strings and "N/A" for unknown values.
type ffprobeOutput struct {
	Format *struct {
		FormatName string `json:"format_name"`
		StartTime  string `json:"start_time"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		Index       int    `json:"index"`
		CodecType   string `json:"codec_type"`
		CodecName   string `json:"codec_name"`
		Profile     string `json:"profile"`
		PixFmt      string `json:"pix_fmt"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		Channels    int    `json:"channels"`
		SampleRate  string `json:"sample_rate"`
		StartTime   string `json:"start_time"`
		Duration    string `json:"duration"`
		Disposition struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
	} `json:"streams"`
}

func mediaInvalid(file string) *app.Error {
	return app.E(app.CodeMediaInvalid, "无法识别媒体文件：%s", filepath.Base(file)).
		WithStage(app.StageProbe).
		WithHint("文件可能已损坏，或不是受支持的音视频格式。")
}

// Probe runs ffprobe on file. A file ffprobe cannot read, or one without any
// audio or video stream, is MEDIA_INVALID. A missing audio stream is not an
// error here: check HasAudio (the pipeline maps it to NO_AUDIO_STREAM).
func Probe(ctx context.Context, runner Runner, ffprobePath, file string) (*ProbeInfo, error) {
	var out strings.Builder
	res, err := runner.Run(ctx, procx.Cmd{
		Path: ffprobePath,
		Args: []string{
			"-v", "error", "-hide_banner",
			"-print_format", "json", "-show_format", "-show_streams",
			"-i", file,
		},
		Stdout: &out,
		Stage:  app.StageProbe,
	})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if app.IsCode(err, app.CodeToolMissing) {
			return nil, err
		}
		if errors.Is(err, procx.ErrKilled) {
			return nil, err
		}
		e := mediaInvalid(file).WithCause(err)
		if res != nil {
			e = e.WithDetail("exit_code", res.ExitCode).WithDetail("stderr_tail", res.StderrTail)
		}
		return nil, e
	}
	info, perr := ParseProbe([]byte(out.String()))
	if perr != nil {
		return nil, mediaInvalid(file).WithCause(perr)
	}
	return info, nil
}

// ParseProbe converts ffprobe's `-print_format json -show_format
// -show_streams` output into a ProbeInfo.
func ParseProbe(data []byte) (*ProbeInfo, error) {
	var raw ffprobeOutput
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw.Format == nil {
		return nil, errors.New("ffprobe output has no format section")
	}
	info := &ProbeInfo{AudioStreamIndex: -1, VideoStreamIndex: -1, Streams: []Stream{}}
	for _, n := range strings.Split(raw.Format.FormatName, ",") {
		if n = strings.TrimSpace(n); n != "" {
			info.FormatNames = append(info.FormatNames, n)
		}
	}
	info.StartTime, _ = parseSeconds(raw.Format.StartTime)
	if d, ok := parseSeconds(raw.Format.Duration); ok {
		info.DurationMs = secToMs(d)
	}
	info.SizeBytes, _ = strconv.ParseInt(raw.Format.Size, 10, 64)
	info.BitRate, _ = strconv.ParseInt(raw.Format.BitRate, 10, 64)

	var longest int64
	for _, s := range raw.Streams {
		st := Stream{
			Index:       s.Index,
			CodecType:   s.CodecType,
			CodecName:   s.CodecName,
			Profile:     s.Profile,
			PixFmt:      s.PixFmt,
			Width:       s.Width,
			Height:      s.Height,
			Channels:    s.Channels,
			AttachedPic: s.Disposition.AttachedPic != 0,
		}
		st.SampleRate, _ = strconv.Atoi(s.SampleRate)
		st.StartTime, st.HasStartTime = parseSeconds(s.StartTime)
		if d, ok := parseSeconds(s.Duration); ok {
			st.DurationMs = secToMs(d)
			longest = max(longest, st.DurationMs)
		}
		switch {
		case st.CodecType == "audio" && info.AudioStreamIndex < 0:
			info.AudioStreamIndex, info.HasAudio = st.Index, true
		case st.CodecType == "video" && !st.AttachedPic && info.VideoStreamIndex < 0:
			info.VideoStreamIndex, info.HasVideo = st.Index, true
		}
		info.Streams = append(info.Streams, st)
	}
	if info.DurationMs <= 0 {
		info.DurationMs = longest // some containers only report per-stream durations
	}
	if !info.HasAudio && !info.HasVideo {
		return nil, errors.New("no audio or video stream")
	}
	return info, nil
}

func parseSeconds(s string) (float64, bool) {
	if s == "" || s == "N/A" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func secToMs(sec float64) int64 { return int64(math.Round(sec * 1000)) }

// TimelineOffsetMs is how far the audio track starts after the container
// does; transcript times are shifted by it so they match the player:
//
//	timeline_offset_ms = round((audio stream start_time − container start_time) × 1000)
//
// Whisper timestamps are relative to the first audio sample, while the player
// clock starts at the container start time; adding this offset to every
// segment puts transcript and player on the same timeline. It is 0 when there
// is no audio stream or the stream has no start time.
func TimelineOffsetMs(info *ProbeInfo) int64 {
	a := info.AudioStream()
	if a == nil || !a.HasStartTime {
		return 0
	}
	return secToMs(a.StartTime - info.StartTime)
}
