package media

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"watchink/internal/app"
	"watchink/internal/infra/procx"
)

// The tests in this file run the real ffmpeg / ffprobe. They are skipped
// unless both variables point at the binaries:
//
//	WATCHINK_FFMPEG=/path/ffmpeg WATCHINK_FFPROBE=/path/ffprobe go test ./internal/capability/media/
type ffEnv struct {
	t       *testing.T
	ffmpeg  string
	ffprobe string
	runner  *procx.Runner
	dir     string
	enc     string
}

func newFFEnv(t *testing.T) *ffEnv {
	t.Helper()
	ffmpeg, ffprobe := os.Getenv("WATCHINK_FFMPEG"), os.Getenv("WATCHINK_FFPROBE")
	if ffmpeg == "" || ffprobe == "" {
		t.Skip("set WATCHINK_FFMPEG and WATCHINK_FFPROBE to run the ffmpeg integration tests")
	}
	e := &ffEnv{t: t, ffmpeg: ffmpeg, ffprobe: ffprobe, runner: procx.New(procx.Options{}), dir: t.TempDir()}
	var out strings.Builder
	if _, err := e.runner.Run(context.Background(), procx.Cmd{Path: ffmpeg, Args: []string{"-hide_banner", "-encoders"}, Stdout: &out}); err != nil {
		t.Fatalf("ffmpeg -encoders: %v", err)
	}
	e.enc = out.String()
	return e
}

func (e *ffEnv) hasEncoder(name string) bool { return strings.Contains(e.enc, " "+name+" ") }

// gen creates a fixture with ffmpeg; args follow the standard inputs.
func (e *ffEnv) gen(name string, args ...string) string {
	e.t.Helper()
	out := filepath.Join(e.dir, name)
	full := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, args...)
	full = append(full, out)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if res, err := e.runner.Run(ctx, procx.Cmd{Path: e.ffmpeg, Args: full}); err != nil {
		e.t.Fatalf("generate %s: %v\n%s", name, err, res.StderrTail)
	}
	return out
}

func video(size string, sec string) []string {
	return []string{"-f", "lavfi", "-i", "testsrc=size=" + size + ":rate=25:duration=" + sec}
}

func tone(sec string) []string {
	return []string{"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=" + sec}
}

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func (e *ffEnv) probe(file string) *ProbeInfo {
	e.t.Helper()
	info, err := Probe(context.Background(), e.runner, e.ffprobe, file)
	if err != nil {
		e.t.Fatalf("Probe(%s): %v", filepath.Base(file), err)
	}
	return info
}

func near(a, b, tol int64) bool { return a-b <= tol && b-a <= tol }

var (
	h264 = []string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p"}
	aac  = []string{"-c:a", "aac", "-b:a", "64k"}
)

func TestIntegrationProbe(t *testing.T) {
	e := newFFEnv(t)
	mp4 := e.gen("a.mp4", cat(video("640x360", "3"), tone("3"), h264, aac)...)
	info := e.probe(mp4)
	if !near(info.DurationMs, 3000, 150) || !hasFormat(info, "mp4") || !info.HasAudio || !info.HasVideo {
		t.Errorf("mp4 probe = %+v", info)
	}
	v, a := info.VideoStream(), info.AudioStream()
	if v.CodecName != "h264" || v.Width != 640 || v.Height != 360 || v.PixFmt != "yuv420p" {
		t.Errorf("video = %+v", v)
	}
	if a.CodecName != "aac" || a.SampleRate != 44100 || a.Channels != 1 || a.Index != info.AudioStreamIndex {
		t.Errorf("audio = %+v", a)
	}

	silent := e.gen("silent.mp4", cat(video("320x240", "1"), h264)...)
	if info := e.probe(silent); info.HasAudio || info.AudioStreamIndex != -1 || !info.HasVideo {
		t.Errorf("silent probe = %+v", info)
	}

	// Not media at all → MEDIA_INVALID.
	junk := filepath.Join(e.dir, "junk.mp4")
	if err := os.WriteFile(junk, []byte(strings.Repeat("this is not a video\n", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Probe(context.Background(), e.runner, e.ffprobe, junk); !app.IsCode(err, app.CodeMediaInvalid) {
		t.Errorf("junk: err = %v, want MEDIA_INVALID", err)
	}
	if _, err := Probe(context.Background(), e.runner, e.ffprobe, filepath.Join(e.dir, "missing.mp4")); !app.IsCode(err, app.CodeMediaInvalid) {
		t.Errorf("missing: err = %v, want MEDIA_INVALID", err)
	}
	if _, err := Probe(context.Background(), e.runner, filepath.Join(e.dir, "no-ffprobe"), mp4); !app.IsCode(err, app.CodeToolMissing) {
		t.Errorf("no tool: err = %v, want TOOL_MISSING", err)
	}
}

func TestIntegrationExtractAudioAndOffset(t *testing.T) {
	e := newFFEnv(t)

	// Audio delayed by 0.5 s against the video: the offset must report.
	delayed := e.gen("delayed.mkv", cat(video("320x240", "4"), []string{"-itsoffset", "0.5"}, tone("3"), h264, aac)...)
	info := e.probe(delayed)
	if off := TimelineOffsetMs(info); !near(off, 500, 30) {
		t.Errorf("delayed.mkv: TimelineOffsetMs = %d, want ~500 (%+v)", off, info.Streams)
	}

	// MPEG-TS starts at a non-zero container time (1.4 s by default); audio
	// and container start together, so the offset stays near zero.
	ts := e.gen("b.ts", cat(video("320x240", "3"), tone("3"), h264, aac, []string{"-f", "mpegts"})...)
	tsInfo := e.probe(ts)
	if tsInfo.StartTime < 1 {
		t.Errorf("b.ts: container start_time = %v, want non-zero", tsInfo.StartTime)
	}
	if off := TimelineOffsetMs(tsInfo); !near(off, 0, 60) {
		t.Errorf("b.ts: TimelineOffsetMs = %d, want ~0", off)
	}

	wav := filepath.Join(e.dir, "audio.wav")
	var prog []float64
	if err := ExtractAudio(context.Background(), e.runner, e.ffmpeg, delayed, wav, info.DurationMs, func(f float64) { prog = append(prog, f) }); err != nil {
		t.Fatalf("ExtractAudio: %v", err)
	}
	w := e.probe(wav)
	a := w.AudioStream()
	if a == nil || a.CodecName != "pcm_s16le" || a.SampleRate != 16000 || a.Channels != 1 || !hasFormat(w, "wav") {
		t.Errorf("wav audio = %+v, formats %v", a, w.FormatNames)
	}
	// The WAV holds only the audio samples (3 s), not the 0.5 s lead-in:
	// that is exactly why the offset has to be added back to segment times.
	if !near(w.DurationMs, 3000, 120) {
		t.Errorf("wav duration = %d ms, want ~3000", w.DurationMs)
	}
	if len(prog) == 0 || prog[len(prog)-1] != 1 {
		t.Errorf("progress = %v", prog)
	}
	for i := 1; i < len(prog); i++ {
		if prog[i] < prog[i-1] {
			t.Errorf("progress went backwards: %v", prog)
		}
	}
	if _, err := os.Stat(wav + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}

	// No audio stream → NO_AUDIO_STREAM, and no output.
	silent := e.gen("silent.mp4", cat(video("320x240", "1"), h264)...)
	out := filepath.Join(e.dir, "none.wav")
	if err := ExtractAudio(context.Background(), e.runner, e.ffmpeg, silent, out, 1000, nil); !app.IsCode(err, app.CodeNoAudioStream) {
		t.Errorf("silent: err = %v, want NO_AUDIO_STREAM", err)
	}
	if entries, _ := filepath.Glob(out + "*"); len(entries) != 0 {
		t.Errorf("output left for a failed extraction: %v", entries)
	}

	// Cancellation stops ffmpeg and removes the partial file.
	long := e.gen("long.mkv", cat(tone("1200"), []string{"-c:a", "flac"})...)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once bool
	go func() {
		<-started
		cancel()
	}()
	cut := filepath.Join(e.dir, "cut.wav")
	// Cancel on the first progress callback, i.e. while ffmpeg is running.
	err := ExtractAudio(ctx, e.runner, e.ffmpeg, long, cut, 1200000, func(float64) {
		if !once {
			once = true
			close(started)
		}
	})
	if err == nil {
		// A very fast machine may finish 20 minutes of audio before the
		// cancel lands; that is not a failure of the code under test.
		t.Log("extraction finished before the cancel took effect")
	} else if ctx.Err() == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("cancelled extract: err = %v", err)
	} else if entries, _ := filepath.Glob(cut + "*"); len(entries) != 0 {
		t.Errorf("partial output left after cancel: %v", entries)
	}
	cancel()
}

func TestIntegrationPlaybackPlans(t *testing.T) {
	e := newFFEnv(t)

	hevc := []string{"-c:v", "mpeg4", "-q:v", "8"} // always-available incompatible codec
	hevcName := "mpeg4"
	if e.hasEncoder("libx265") {
		hevc, hevcName = []string{"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-x265-params", "log-level=error"}, "hevc"
	}
	badAudio, badAudioName := []string{"-c:a", "flac"}, "flac"
	if e.hasEncoder("libopus") {
		badAudio, badAudioName = []string{"-c:a", "libopus", "-b:a", "48k"}, "opus"
	}

	cases := []struct {
		name       string
		file       string
		maxHeight  int
		want       PlaybackKind
		wantHeight int
		srcVideo   string
		srcAudio   string
	}{
		{"mp4 h264 aac", e.gen("direct.mp4", cat(video("640x360", "3"), tone("3"), h264, aac)...), 720, PlaybackDirect, 360, "h264", "aac"},
		{"mov h264 aac", e.gen("direct.mov", cat(video("640x360", "2"), tone("2"), h264, aac)...), 720, PlaybackDirect, 360, "h264", "aac"},
		{"mkv h264 aac", e.gen("remux.mkv", cat(video("640x360", "3"), tone("3"), h264, aac)...), 720, PlaybackRemux, 360, "h264", "aac"},
		{"ts h264 aac", e.gen("remux.ts", cat(video("640x360", "3"), tone("3"), h264, aac, []string{"-f", "mpegts"})...), 720, PlaybackRemux, 360, "h264", "aac"},
		{"flv h264 mp3", e.gen("remux.flv", cat(video("640x360", "3"), tone("3"), h264, []string{"-c:a", "libmp3lame", "-b:a", "64k"})...), 720, PlaybackRemux, 360, "h264", "mp3"},
		{"mkv h264 " + badAudioName, e.gen("atrans.mkv", cat(video("640x360", "3"), tone("3"), h264, badAudio)...), 720, PlaybackAudioTranscode, 360, "h264", badAudioName},
		{"mkv " + hevcName + " 1080p", e.gen("full.mkv", cat(video("1920x1080", "2"), tone("2"), hevc, aac)...), 720, PlaybackFullTranscode, 720, hevcName, "aac"},
		{"mp4 " + hevcName + " 360p keeps size", e.gen("full-small.mp4", cat(video("640x360", "2"), tone("2"), hevc, aac)...), 720, PlaybackFullTranscode, 360, hevcName, ""},
		{"mkv delayed audio", e.gen("delayed.mkv", cat(video("640x360", "4"), []string{"-itsoffset", "0.5"}, tone("3"), hevc, badAudio)...), 480, PlaybackFullTranscode, 360, hevcName, badAudioName},
	}
	if !e.hasEncoder("libmp3lame") {
		t.Fatal("this ffmpeg build has no libmp3lame; adjust the fixture")
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := e.probe(tc.file)
			if v := src.VideoStream(); v == nil || v.CodecName != tc.srcVideo {
				t.Fatalf("fixture video codec = %+v, want %s", v, tc.srcVideo)
			}
			if a := src.AudioStream(); tc.srcAudio != "" && (a == nil || a.CodecName != tc.srcAudio) {
				t.Fatalf("fixture audio codec = %+v, want %s", a, tc.srcAudio)
			}
			plan := PlanPlayback(src, tc.maxHeight)
			if plan.Kind != tc.want {
				t.Fatalf("plan = %+v, want %s", plan, tc.want)
			}
			dst := filepath.Join(e.dir, "playback-"+strings.ReplaceAll(tc.name, " ", "_")+".mp4")
			if !plan.NeedsBuild() {
				if err := BuildPlayback(context.Background(), e.runner, e.ffmpeg, plan, tc.file, dst, src.DurationMs, nil); err == nil {
					t.Error("BuildPlayback accepted a direct plan")
				}
				return
			}
			var last float64
			if err := BuildPlayback(context.Background(), e.runner, e.ffmpeg, plan, tc.file, dst, src.DurationMs, func(f float64) { last = f }); err != nil {
				t.Fatalf("BuildPlayback: %v", err)
			}
			if last != 1 {
				t.Errorf("final progress = %v", last)
			}
			out := e.probe(dst)
			v, a := out.VideoStream(), out.AudioStream()
			if !hasFormat(out, "mp4") || v == nil || a == nil {
				t.Fatalf("output probe = %+v", out)
			}
			if v.CodecName != "h264" || v.PixFmt != "yuv420p" || v.Height != tc.wantHeight || v.Width%2 != 0 {
				t.Errorf("output video = %+v, want h264 yuv420p height %d", v, tc.wantHeight)
			}
			if a.CodecName != "aac" && a.CodecName != "mp3" {
				t.Errorf("output audio codec = %s", a.CodecName)
			}
			if plan.CopyAudio && a.CodecName != src.AudioStream().CodecName {
				t.Errorf("audio was re-encoded (%s → %s) although the plan copies it", src.AudioStream().CodecName, a.CodecName)
			}
			if !near(out.DurationMs, src.DurationMs, 200) {
				t.Errorf("duration %d ms, source %d ms", out.DurationMs, src.DurationMs)
			}
			// The output is directly playable: planning it again says so.
			if again := PlanPlayback(out, tc.maxHeight); again.Kind != PlaybackDirect {
				t.Errorf("generated file is not directly playable: %+v", again)
			}
			// Timeline preserved: the audio/video start relation of
			// the source survives, so a transcript offset computed from the
			// source also fits the playback file.
			srcRel := relStartMs(src)
			outRel := relStartMs(out)
			if math.Abs(float64(srcRel-outRel)) > 60 {
				t.Errorf("audio-vs-video start changed: source %d ms, playback %d ms", srcRel, outRel)
			}
			if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
				t.Error("temp file left behind")
			}
		})
	}
}

func relStartMs(info *ProbeInfo) int64 {
	a, v := info.AudioStream(), info.VideoStream()
	if a == nil || v == nil {
		return 0
	}
	return secToMs(a.StartTime - v.StartTime)
}

func TestIntegrationAudioOnlyPlayback(t *testing.T) {
	e := newFFEnv(t)
	wav := e.gen("talk.wav", tone("3")...)
	src := e.probe(wav)
	plan := PlanPlayback(src, 720)
	if plan.Kind != PlaybackAudioTranscode || plan.HasVideo {
		t.Fatalf("plan = %+v", plan)
	}
	dst := filepath.Join(e.dir, "talk.m4a")
	if err := BuildPlayback(context.Background(), e.runner, e.ffmpeg, plan, wav, dst, src.DurationMs, nil); err != nil {
		t.Fatalf("BuildPlayback: %v", err)
	}
	out := e.probe(dst)
	if out.HasVideo || out.AudioStream().CodecName != "aac" || !near(out.DurationMs, 3000, 150) {
		t.Errorf("output = %+v", out)
	}
	if again := PlanPlayback(out, 720); again.Kind != PlaybackDirect || again.Mime != "audio/mp4" {
		t.Errorf("replan = %+v", again)
	}
}
