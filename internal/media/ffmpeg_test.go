package media

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"watchink/internal/app"
	"watchink/internal/procx"
)

// fakeRunner records the command and plays back a scripted outcome.
type fakeRunner struct {
	cmds   []procx.Cmd
	stdout string
	lines  []string
	res    *procx.Result
	err    error
	// onRun can create output files the way the real tool would.
	onRun func(cmd procx.Cmd)
}

func (f *fakeRunner) Run(ctx context.Context, cmd procx.Cmd) (*procx.Result, error) {
	f.cmds = append(f.cmds, cmd)
	if cmd.Stdout != nil {
		_, _ = cmd.Stdout.Write([]byte(f.stdout))
	}
	if cmd.OnStdoutLine != nil {
		for _, l := range f.lines {
			cmd.OnStdoutLine(l)
		}
	}
	if f.onRun != nil {
		f.onRun(cmd)
	}
	res := f.res
	if res == nil {
		res = &procx.Result{}
	}
	return res, f.err
}

func toolFailed(stderr string) (*procx.Result, error) {
	return &procx.Result{ExitCode: 1, StderrTail: stderr},
		app.E(app.CodeToolFailed, "tool failed").WithDetail("stderr_tail", stderr)
}

const probeMP4 = `{
  "streams": [
    {"index": 0, "codec_name": "h264", "profile": "High", "codec_type": "video", "width": 1280, "height": 720,
     "pix_fmt": "yuv420p", "start_time": "0.000000", "duration": "12.500000", "disposition": {"attached_pic": 0}},
    {"index": 1, "codec_name": "aac", "codec_type": "audio", "sample_rate": "44100", "channels": 2,
     "start_time": "0.023000", "duration": "12.480000", "disposition": {"attached_pic": 0}},
    {"index": 2, "codec_name": "mov_text", "codec_type": "subtitle", "start_time": "0.000000"}
  ],
  "format": {"filename": "x.mp4", "nb_streams": 3, "format_name": "mov,mp4,m4a,3gp,3g2,mj2",
             "start_time": "0.000000", "duration": "12.500000", "size": "123456", "bit_rate": "79000"}
}`

func TestParseProbe(t *testing.T) {
	info, err := ParseProbe([]byte(probeMP4))
	if err != nil {
		t.Fatal(err)
	}
	if info.DurationMs != 12500 || info.SizeBytes != 123456 || info.BitRate != 79000 {
		t.Errorf("format fields: %+v", info)
	}
	if !reflect.DeepEqual(info.FormatNames, []string{"mov", "mp4", "m4a", "3gp", "3g2", "mj2"}) {
		t.Errorf("FormatNames = %v", info.FormatNames)
	}
	if !info.HasAudio || !info.HasVideo || info.AudioStreamIndex != 1 || info.VideoStreamIndex != 0 || len(info.Streams) != 3 {
		t.Errorf("streams: %+v", info)
	}
	v, a := info.VideoStream(), info.AudioStream()
	if v.CodecName != "h264" || v.Width != 1280 || v.Height != 720 || v.PixFmt != "yuv420p" {
		t.Errorf("video = %+v", v)
	}
	if a.CodecName != "aac" || a.SampleRate != 44100 || a.Channels != 2 || !a.HasStartTime || a.StartTime != 0.023 {
		t.Errorf("audio = %+v", a)
	}
	if got := TimelineOffsetMs(info); got != 23 {
		t.Errorf("TimelineOffsetMs = %d, want 23", got)
	}

	// The artifact must survive a JSON round trip unchanged.
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var back ProbeInfo
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(&back, info) {
		t.Errorf("JSON round trip changed the value: %v\n%s", err, b)
	}
}

func TestParseProbeEdgeCases(t *testing.T) {
	t.Run("cover art is not a video stream", func(t *testing.T) {
		info, err := ParseProbe([]byte(`{"streams":[
			{"index":0,"codec_name":"mjpeg","codec_type":"video","width":600,"height":600,"disposition":{"attached_pic":1}},
			{"index":1,"codec_name":"mp3","codec_type":"audio","start_time":"0.025057","duration":"61.0"}],
			"format":{"format_name":"mp3","start_time":"0.025057","duration":"61.0"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if info.HasVideo || info.VideoStream() != nil || !info.HasAudio || info.AudioStreamIndex != 1 {
			t.Errorf("info = %+v", info)
		}
		if got := TimelineOffsetMs(info); got != 0 {
			t.Errorf("offset = %d", got)
		}
	})
	t.Run("no audio", func(t *testing.T) {
		info, err := ParseProbe([]byte(`{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","start_time":"1.5"}],
			"format":{"format_name":"matroska,webm","start_time":"1.5","duration":"N/A"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if info.HasAudio || info.AudioStreamIndex != -1 || info.AudioStream() != nil || TimelineOffsetMs(info) != 0 {
			t.Errorf("info = %+v", info)
		}
	})
	t.Run("duration falls back to the streams", func(t *testing.T) {
		info, err := ParseProbe([]byte(`{"streams":[{"index":0,"codec_name":"opus","codec_type":"audio","duration":"3.2004"}],
			"format":{"format_name":"ogg","duration":"N/A"}}`))
		if err != nil || info.DurationMs != 3200 {
			t.Errorf("info = %+v, err = %v", info, err)
		}
	})
	t.Run("second audio stream is not the first", func(t *testing.T) {
		info, _ := ParseProbe([]byte(`{"streams":[
			{"index":0,"codec_type":"audio","codec_name":"aac","start_time":"0.5"},
			{"index":1,"codec_type":"audio","codec_name":"ac3","start_time":"9"}],
			"format":{"format_name":"mpegts","start_time":"0.2","duration":"4"}}`))
		if info.AudioStreamIndex != 0 || TimelineOffsetMs(info) != 300 {
			t.Errorf("index = %d, offset = %d", info.AudioStreamIndex, TimelineOffsetMs(info))
		}
	})
	t.Run("negative offset", func(t *testing.T) {
		info, _ := ParseProbe([]byte(`{"streams":[
			{"index":0,"codec_type":"video","codec_name":"h264","start_time":"10.0"},
			{"index":1,"codec_type":"audio","codec_name":"aac","start_time":"9.978"}],
			"format":{"format_name":"mpegts","start_time":"10.0","duration":"4"}}`))
		if got := TimelineOffsetMs(info); got != -22 {
			t.Errorf("offset = %d, want -22", got)
		}
	})
	for name, in := range map[string]string{
		"not json":         "Invalid data found",
		"no format":        `{"streams":[{"index":0,"codec_type":"audio"}]}`,
		"only subtitles":   `{"streams":[{"index":0,"codec_type":"subtitle"}],"format":{"format_name":"srt"}}`,
		"only a cover art": `{"streams":[{"index":0,"codec_type":"video","disposition":{"attached_pic":1}}],"format":{"format_name":"image2"}}`,
		"empty":            `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if info, err := ParseProbe([]byte(in)); err == nil {
				t.Errorf("accepted: %+v", info)
			}
		})
	}
}

func TestProbeCommandAndErrors(t *testing.T) {
	fr := &fakeRunner{stdout: probeMP4}
	info, err := Probe(context.Background(), fr, "/t/ffprobe", "/data/source.mp4")
	if err != nil || info.DurationMs != 12500 {
		t.Fatalf("Probe: %+v, %v", info, err)
	}
	want := []string{"-v", "error", "-hide_banner", "-print_format", "json", "-show_format", "-show_streams", "-i", "/data/source.mp4"}
	if c := fr.cmds[0]; c.Path != "/t/ffprobe" || !reflect.DeepEqual(c.Args, want) || c.Stage != app.StageProbe {
		t.Errorf("cmd = %+v", c)
	}

	fr = &fakeRunner{}
	fr.res, fr.err = toolFailed("x.bin: Invalid data found when processing input")
	_, err = Probe(context.Background(), fr, "ffprobe", "/data/x.bin")
	if !app.IsCode(err, app.CodeMediaInvalid) {
		t.Fatalf("err = %v, want MEDIA_INVALID", err)
	}
	if e := app.AsError(err); e.Stage != app.StageProbe || e.Details["stderr_tail"] == "" {
		t.Errorf("error = %+v", e)
	}

	fr = &fakeRunner{stdout: "garbage"}
	if _, err = Probe(context.Background(), fr, "ffprobe", "x"); !app.IsCode(err, app.CodeMediaInvalid) {
		t.Errorf("unparsable output: err = %v", err)
	}

	fr = &fakeRunner{err: app.E(app.CodeToolMissing, "missing")}
	if _, err = Probe(context.Background(), fr, "ffprobe", "x"); !app.IsCode(err, app.CodeToolMissing) {
		t.Errorf("missing tool: err = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fr = &fakeRunner{err: context.Canceled}
	if _, err = Probe(ctx, fr, "ffprobe", "x"); !errors.Is(err, context.Canceled) || app.IsCode(err, app.CodeMediaInvalid) {
		t.Errorf("cancelled: err = %v", err)
	}
}

func probeOf(formats string, video, pix string, height int, audio string) *ProbeInfo {
	info := &ProbeInfo{FormatNames: strings.Split(formats, ","), AudioStreamIndex: -1, VideoStreamIndex: -1}
	if video != "" {
		info.Streams = append(info.Streams, Stream{Index: len(info.Streams), CodecType: "video", CodecName: video, PixFmt: pix, Height: height, Width: height * 16 / 9})
		info.VideoStreamIndex, info.HasVideo = len(info.Streams)-1, true
	}
	if audio != "" {
		info.Streams = append(info.Streams, Stream{Index: len(info.Streams), CodecType: "audio", CodecName: audio})
		info.AudioStreamIndex, info.HasAudio = len(info.Streams)-1, true
	}
	return info
}

func TestPlanPlayback(t *testing.T) {
	const mp4, mkv, ts, flv, webm = "mov,mp4,m4a,3gp,3g2,mj2", "matroska,webm", "mpegts", "flv", "matroska,webm"
	tests := []struct {
		name      string
		info      *ProbeInfo
		maxHeight int
		want      PlaybackKind
		wantScale int
		wantCopyA bool
		wantMime  string
	}{
		// Row 1: MP4 / MOV / M4V, H.264, AAC or MP3 → direct.
		{"mp4 h264 aac", probeOf(mp4, "h264", "yuv420p", 1080, "aac"), 720, PlaybackDirect, 0, true, "video/mp4"},
		{"mp4 h264 mp3", probeOf(mp4, "h264", "yuv420p", 720, "mp3"), 720, PlaybackDirect, 0, true, "video/mp4"},
		{"mp4 h264 full range", probeOf(mp4, "h264", "yuvj420p", 720, "aac"), 720, PlaybackDirect, 0, true, "video/mp4"},
		{"mp4 h264 no audio", probeOf(mp4, "h264", "yuv420p", 720, ""), 720, PlaybackDirect, 0, false, "video/mp4"},
		// Row 2: other container, same codecs → remux.
		{"mkv h264 aac", probeOf(mkv, "h264", "yuv420p", 1080, "aac"), 720, PlaybackRemux, 0, true, "video/mp4"},
		{"flv h264 mp3", probeOf(flv, "h264", "yuv420p", 480, "mp3"), 720, PlaybackRemux, 0, true, "video/mp4"},
		{"ts h264 aac", probeOf(ts, "h264", "yuv420p", 1080, "aac"), 720, PlaybackRemux, 0, true, "video/mp4"},
		// Row 3: H.264 with incompatible audio → audio transcode.
		{"mkv h264 opus", probeOf(mkv, "h264", "yuv420p", 1080, "opus"), 720, PlaybackAudioTranscode, 0, false, "video/mp4"},
		{"mp4 h264 ac3", probeOf(mp4, "h264", "yuv420p", 1080, "ac3"), 720, PlaybackAudioTranscode, 0, false, "video/mp4"},
		{"mov h264 pcm", probeOf(mp4, "h264", "yuv420p", 1080, "pcm_s16le"), 720, PlaybackAudioTranscode, 0, false, "video/mp4"},
		{"ts h264 mp2", probeOf(ts, "h264", "yuv420p", 576, "mp2"), 720, PlaybackAudioTranscode, 0, false, "video/mp4"},
		// Row 4: incompatible video → full transcode, height capped.
		{"mp4 hevc aac 1080", probeOf(mp4, "hevc", "yuv420p", 1080, "aac"), 720, PlaybackFullTranscode, 720, true, "video/mp4"},
		{"mkv hevc aac 720", probeOf(mkv, "hevc", "yuv420p", 720, "aac"), 720, PlaybackFullTranscode, 0, true, "video/mp4"},
		{"webm vp9 opus 2160", probeOf(webm, "vp9", "yuv420p", 2160, "opus"), 720, PlaybackFullTranscode, 720, false, "video/mp4"},
		{"mp4 av1 aac 480", probeOf(mp4, "av1", "yuv420p", 480, "aac"), 720, PlaybackFullTranscode, 0, true, "video/mp4"},
		{"avi mpeg4 mp3", probeOf("avi", "mpeg4", "yuv420p", 480, "mp3"), 720, PlaybackFullTranscode, 0, true, "video/mp4"},
		{"cap from settings", probeOf(mp4, "hevc", "yuv420p", 1080, "aac"), 1080, PlaybackFullTranscode, 0, true, "video/mp4"},
		{"default cap", probeOf(mp4, "hevc", "yuv420p", 1080, "aac"), 0, PlaybackFullTranscode, 720, true, "video/mp4"},
		{"hevc no audio", probeOf(mkv, "hevc", "yuv420p", 2160, ""), 480, PlaybackFullTranscode, 480, false, "video/mp4"},
		// H.264 a WebView cannot decode is treated as incompatible video.
		{"mkv h264 hi10p", probeOf(mkv, "h264", "yuv420p10le", 1080, "aac"), 720, PlaybackFullTranscode, 720, true, "video/mp4"},
		{"mp4 h264 444", probeOf(mp4, "h264", "yuv444p", 720, "aac"), 720, PlaybackFullTranscode, 0, true, "video/mp4"},
		// Audio-only sources.
		{"m4a aac", probeOf(mp4, "", "", 0, "aac"), 720, PlaybackDirect, 0, true, "audio/mp4"},
		{"mp3 file", probeOf("mp3", "", "", 0, "mp3"), 720, PlaybackDirect, 0, true, "audio/mpeg"},
		{"adts aac", probeOf("aac", "", "", 0, "aac"), 720, PlaybackRemux, 0, true, "audio/mp4"},
		{"wav pcm", probeOf("wav", "", "", 0, "pcm_s16le"), 720, PlaybackAudioTranscode, 0, false, "audio/mp4"},
		{"ogg opus", probeOf("ogg", "", "", 0, "opus"), 720, PlaybackAudioTranscode, 0, false, "audio/mp4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlanPlayback(tt.info, tt.maxHeight)
			if got.Kind != tt.want || got.ScaleHeight != tt.wantScale || got.CopyAudio != tt.wantCopyA || got.Mime != tt.wantMime {
				t.Errorf("plan = %+v, want kind %s scale %d copyAudio %v mime %s", got, tt.want, tt.wantScale, tt.wantCopyA, tt.wantMime)
			}
			if got.NeedsBuild() != (tt.want != PlaybackDirect) || got.Reason == "" {
				t.Errorf("NeedsBuild = %v, reason = %q", got.NeedsBuild(), got.Reason)
			}
			if got.HasVideo != tt.info.HasVideo || got.HasAudio != tt.info.HasAudio {
				t.Errorf("stream flags: %+v", got)
			}
		})
	}
}

// timestampShifters are the options forbids in playback commands.
var timestampShifters = []string{"-ss", "-itsoffset", "-copyts", "-start_at_zero", "-avoid_negative_ts", "-output_ts_offset", "-muxdelay", "-muxpreload", "-t", "-to", "-shortest"}

func TestPlaybackArgs(t *testing.T) {
	const pre = "-nostdin -hide_banner -y -i /in/src.mkv "
	const post = " -movflags +faststart -progress pipe:1 -nostats -f mp4 /out/playback.mp4.tmp"
	tests := []struct {
		name string
		plan Plan
		want string
	}{
		{"remux", Plan{Kind: PlaybackRemux, HasVideo: true, HasAudio: true, CopyAudio: true},
			"-map 0:V:0 -map 0:a:0 -sn -dn -c copy"},
		{"remux audio only", Plan{Kind: PlaybackRemux, HasAudio: true, CopyAudio: true},
			"-vn -map 0:a:0 -sn -dn -c copy"},
		{"audio transcode", Plan{Kind: PlaybackAudioTranscode, HasVideo: true, HasAudio: true},
			"-map 0:V:0 -map 0:a:0 -sn -dn -c:v copy -c:a aac -b:a 128k"},
		{"audio transcode audio only", Plan{Kind: PlaybackAudioTranscode, HasAudio: true},
			"-vn -map 0:a:0 -sn -dn -c:a aac -b:a 128k"},
		{"full transcode scaled", Plan{Kind: PlaybackFullTranscode, HasVideo: true, HasAudio: true, ScaleHeight: 720},
			"-map 0:V:0 -map 0:a:0 -sn -dn -c:v libx264 -preset veryfast -crf 26 -pix_fmt yuv420p -vf scale=-2:720 -c:a aac -b:a 128k"},
		{"full transcode keep size copy audio", Plan{Kind: PlaybackFullTranscode, HasVideo: true, HasAudio: true, CopyAudio: true},
			"-map 0:V:0 -map 0:a:0 -sn -dn -c:v libx264 -preset veryfast -crf 26 -pix_fmt yuv420p -c:a copy"},
		{"full transcode no audio", Plan{Kind: PlaybackFullTranscode, HasVideo: true},
			"-map 0:V:0 -sn -dn -c:v libx264 -preset veryfast -crf 26 -pix_fmt yuv420p"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := playbackArgs(tt.plan, "/in/src.mkv", "/out/playback.mp4.tmp")
			if got := strings.Join(args, " "); got != pre+tt.want+post {
				t.Errorf("args:\n got %s\nwant %s", got, pre+tt.want+post)
			}
			for _, a := range args {
				for _, bad := range timestampShifters {
					if a == bad {
						t.Errorf("timestamp-shifting option %s present", bad)
					}
				}
			}
		})
	}
}

func TestExtractAudioArgs(t *testing.T) {
	// The command, plus machine-readable progress and the explicit
	// muxer needed by the .tmp name.
	want := "-nostdin -hide_banner -y -i /in/src.mp4 -map 0:a:0 -vn -ac 1 -ar 16000 -c:a pcm_s16le -progress pipe:1 -nostats -f wav /out/audio.wav.tmp"
	if got := strings.Join(extractAudioArgs("/in/src.mp4", "/out/audio.wav.tmp"), " "); got != want {
		t.Errorf("args:\n got %s\nwant %s", got, want)
	}
}

func TestParseFFmpegProgress(t *testing.T) {
	tests := []struct {
		line   string
		dur    int64
		want   float64
		wantOK bool
	}{
		{"out_time_us=5000000", 10000, 0.5, true},
		{"out_time_ms=2500000", 10000, 0.25, true}, // misnamed: microseconds
		{"out_time_us=20000000", 10000, 1, true},
		{"  out_time_us=0  ", 10000, 0, true},
		{"progress=end", 10000, 1, true},
		{"progress=end", 0, 1, true},
		{"progress=continue", 10000, 0, false},
		{"out_time_us=N/A", 10000, 0, false},
		{"out_time_us=-9223372036854775807", 10000, 0, false},
		{"out_time_us=5000000", 0, 0, false},
		{"out_time=00:00:05.000000", 10000, 0, false},
		{"frame=120", 10000, 0, false},
		{"garbage", 10000, 0, false},
	}
	for _, tt := range tests {
		got, ok := parseFFmpegProgress(tt.line, tt.dur)
		if ok != tt.wantOK || got != tt.want {
			t.Errorf("parseFFmpegProgress(%q, %d) = %v, %v; want %v, %v", tt.line, tt.dur, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestExtractAudioWithFakeRunner(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "audio.wav")
	fr := &fakeRunner{
		lines: []string{"out_time_us=1000000", "progress=continue", "out_time_us=500000", "out_time_us=3000000", "out_time_us=4000000", "progress=end"},
		onRun: func(cmd procx.Cmd) {
			_ = os.WriteFile(cmd.Args[len(cmd.Args)-1], []byte("RIFF"), 0o644)
		},
	}
	var got []float64
	err := ExtractAudio(context.Background(), fr, "/t/ffmpeg", "/in/src.mp4", dst, 4000, func(f float64) { got = append(got, f) })
	if err != nil {
		t.Fatal(err)
	}
	// Monotonic, capped below 1 until the rename, then exactly 1.
	if want := []float64{0.25, 0.75, 0.99, 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("progress = %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(dst); string(b) != "RIFF" {
		t.Errorf("dst content = %q", b)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file still present")
	}
	if c := fr.cmds[0]; c.Path != "/t/ffmpeg" || c.Stage != app.StageExtractAudio || c.Args[len(c.Args)-1] != dst+".tmp" {
		t.Errorf("cmd = %+v", c)
	}
}

func TestFFmpegFailureClassification(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		build  bool
		want   app.Code
	}{
		{"no audio stream", "Stream map '0:a:0' matches no streams.\nTo ignore this, add a trailing '?' to the map.", false, app.CodeNoAudioStream},
		{"disk full", "[out#0/wav] Error writing trailer: No space left on device", false, app.CodeDiskFull},
		{"corrupt input", "/in/x.mp4: Invalid data found when processing input", false, app.CodeMediaInvalid},
		{"truncated mp4", "[mov,mp4,m4a] moov atom not found", true, app.CodeMediaInvalid},
		{"anything else", "Conversion failed!", true, app.CodeToolFailed},
		{"map error during playback stays a tool failure", "Stream map '0:a:0' matches no streams.", true, app.CodeToolFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			fr := &fakeRunner{onRun: func(cmd procx.Cmd) {
				_ = os.WriteFile(cmd.Args[len(cmd.Args)-1], []byte("partial"), 0o644)
			}}
			fr.res, fr.err = toolFailed(tt.stderr)
			var err error
			if tt.build {
				err = BuildPlayback(context.Background(), fr, "ffmpeg", Plan{Kind: PlaybackRemux, HasVideo: true, HasAudio: true}, "/in/x.mp4", filepath.Join(dir, "out.mp4"), 1000, nil)
			} else {
				err = ExtractAudio(context.Background(), fr, "ffmpeg", "/in/x.mp4", filepath.Join(dir, "out.wav"), 1000, nil)
			}
			if !app.IsCode(err, tt.want) {
				t.Fatalf("err = %v, want %s", err, tt.want)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("partial output left behind: %v", entries)
			}
		})
	}

	// Cancellation is passed through untouched and cleans up.
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fr := &fakeRunner{err: context.Canceled, onRun: func(cmd procx.Cmd) {
		_ = os.WriteFile(cmd.Args[len(cmd.Args)-1], []byte("partial"), 0o644)
	}}
	err := ExtractAudio(ctx, fr, "ffmpeg", "/in/x.mp4", filepath.Join(dir, "out.wav"), 1000, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("partial output left after cancel: %v", entries)
	}

	// A "successful" run without an output file is still a failure.
	err = ExtractAudio(context.Background(), &fakeRunner{}, "ffmpeg", "/in/x.mp4", filepath.Join(dir, "out.wav"), 1000, nil)
	if !app.IsCode(err, app.CodeToolFailed) {
		t.Errorf("missing output: err = %v", err)
	}

	// BuildPlayback refuses a direct plan.
	err = BuildPlayback(context.Background(), &fakeRunner{}, "ffmpeg", Plan{Kind: PlaybackDirect}, "a", "b", 1, nil)
	if !app.IsCode(err, app.CodeInternal) {
		t.Errorf("direct plan: err = %v", err)
	}
}
