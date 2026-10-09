package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"

	"watchink/internal/app"
	"watchink/internal/asr"
	"watchink/internal/jobs"
	"watchink/internal/media"
	"watchink/internal/store"
	"watchink/internal/tools"
)

// Stage versions: bump when a stage's logic changes so old artifacts are not
// reused (§7.4).
const (
	acquireVersion  = 1
	probeVersion    = 1
	audioVersion    = 1
	asrVersion      = 1
	playbackVersion = 1

	// downloadPolicy names the yt-dlp format policy of media.Download.
	downloadPolicy = "h264-aac-1080-v1"
	// promptEN gives Whisper a punctuated, cased English context: without
	// it large-v3-turbo emits lowercase text with no punctuation
	// (third_party/whisper/NOTES.md).
	promptEN = "The following is a lecture transcript, with proper punctuation and capitalization."
)

func readFile(abs string) ([]byte, error) { return os.ReadFile(abs) }

// --- acquire ---

func (p *Pipeline) acquire(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	opt, err := options(sc)
	if err != nil {
		return nil, err
	}
	v, err := p.st.GetVideo(ctx, sc.VideoID())
	if err != nil {
		return nil, err
	}
	fp := jobs.Fingerprint(app.StageAcquire, acquireVersion, map[string]any{"source_key": v.SourceKey, "policy": downloadPolicy})
	if a := p.cached(ctx, sc, store.KindSource, fp, false); a != nil {
		return reused(a), nil
	}
	dir := p.st.VideoDir(v.ID)
	var rel, sha string
	var size int64
	switch v.SourceType {
	case app.SourceFile:
		if opt.Path == "" {
			return nil, app.E(app.CodeInvalidArgument, "任务缺少本地文件路径")
		}
		rel = path.Join(dir, "source"+safeExt(opt.Path))
		if err := os.MkdirAll(p.st.Abs(dir), 0o755); err != nil {
			return nil, err
		}
		// D-13: copy into the library, hashing on the way.
		sha, size, err = media.CopyWithSHA256(ctx, opt.Path, p.st.Abs(rel))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, app.E(app.CodeNotFound, "源文件不存在：%s", opt.Path)
			}
			return nil, err
		}
		sc.Progress(1)
	case app.SourceUpload:
		up, err := p.st.GetUpload(ctx, opt.UploadID)
		if err != nil {
			return nil, err
		}
		rel = path.Join(dir, "source"+safeExt(up.Filename))
		fi, err := p.st.CommitFile(p.st.Abs(up.Path), rel)
		if err != nil {
			return nil, app.E(app.CodeNotFound, "上传文件已不存在，请重新上传：%v", err)
		}
		sha, size = fi.SHA256, fi.Size
	case app.SourceYouTube, app.SourceBilibili:
		rel, sha, size, err = p.download(ctx, sc, v)
		if err != nil {
			return nil, err
		}
	default:
		return nil, app.E(app.CodeUnsupportedSource, "不支持的来源类型：%s", v.SourceType)
	}
	a, err := p.st.PutArtifact(ctx, &store.Artifact{VideoID: v.ID, Kind: store.KindSource, Fingerprint: fp, Path: rel, SHA256: sha, SizeBytes: size})
	if err != nil {
		return nil, err
	}
	return done(a), nil
}

func (p *Pipeline) download(ctx context.Context, sc *jobs.StageContext, v *store.Video) (rel, sha string, size int64, err error) {
	ytdlp, err := p.tools.Path(tools.ToolYtDlp)
	if err != nil {
		return "", "", 0, err
	}
	deno := ""
	if v.SourceType == app.SourceYouTube {
		// yt-dlp needs an external JS runtime for YouTube only (§11.5).
		if deno, err = p.tools.Path(tools.ToolDeno); err != nil {
			return "", "", 0, err
		}
	}
	dir := p.st.VideoDir(v.ID)
	work := p.st.Abs(path.Join(dir, "download.tmp"))
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", "", 0, err
	}
	defer os.RemoveAll(work)
	res, err := media.Download(ctx, p.runner, media.DownloadOptions{
		YtDlp: ytdlp, FFmpegDir: p.tools.BinDir(), DenoPath: deno, Proxy: p.cfg().Network.Proxy,
		URL: v.SourceURI, SourceType: v.SourceType, OutDir: work, Progress: sc.Progress,
	})
	if err != nil {
		return "", "", 0, err
	}
	rel = path.Join(dir, "source"+safeExt(res.Path))
	fi, err := p.st.CommitFile(res.Path, rel)
	if err != nil {
		return "", "", 0, err
	}
	if t := strings.TrimSpace(res.Title); t != "" {
		if _, err := p.st.UpdateVideo(ctx, v.ID, store.VideoPatch{Title: &t}); err == nil {
			p.changed(v.ID)
		}
	}
	return rel, fi.SHA256, fi.Size, nil
}

// safeExt returns a lowercase, short, alphanumeric extension (with the dot)
// or ".bin": file names inside the library never carry user-controlled text.
func safeExt(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if len(ext) < 2 || len(ext) > 8 {
		return ".bin"
	}
	for _, c := range ext[1:] {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return ".bin"
		}
	}
	return ext
}

// --- probe ---

func (p *Pipeline) probe(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	src, err := p.depArtifact(ctx, sc, app.StageAcquire)
	if err != nil {
		return nil, err
	}
	fp := jobs.Fingerprint(app.StageProbe, probeVersion, map[string]any{"source_sha256": src.SHA256})
	a := p.cached(ctx, sc, store.KindProbe, fp, true)
	var info *media.ProbeInfo
	if a != nil {
		info = new(media.ProbeInfo)
		if err := p.readJSON(a, info); err != nil {
			a = nil
		}
	}
	hit := a != nil
	if !hit {
		ffprobe, err := p.tools.Path(tools.ToolFFprobe)
		if err != nil {
			return nil, err
		}
		if info, err = media.Probe(ctx, p.runner, ffprobe, p.st.Abs(src.Path)); err != nil {
			return nil, err
		}
		if a, err = p.putJSON(ctx, sc.VideoID(), store.KindProbe, fp, path.Join(p.st.VideoDir(sc.VideoID()), "probe.json"), info); err != nil {
			return nil, err
		}
	}
	if !info.HasAudio {
		return nil, app.E(app.CodeNoAudioStream, "视频没有音轨，v0 不支持纯画面视频")
	}
	if _, err := p.st.UpdateVideo(ctx, sc.VideoID(), store.VideoPatch{DurationMs: &info.DurationMs}); err != nil {
		return nil, err
	}
	p.changed(sc.VideoID())
	return &jobs.StageResult{ArtifactID: a.ID, Fingerprint: fp, Reused: hit}, nil
}

func (p *Pipeline) probeInfo(ctx context.Context, sc *jobs.StageContext) (*media.ProbeInfo, error) {
	a, err := p.depArtifact(ctx, sc, app.StageProbe)
	if err != nil {
		return nil, err
	}
	info := new(media.ProbeInfo)
	return info, p.readJSON(a, info)
}

// sourceOf returns the source artifact of the job's video (the acquire
// output), for stages that depend on probe only.
func (p *Pipeline) sourceOf(ctx context.Context, videoID string) (*store.Artifact, error) {
	arts, err := p.st.ListArtifacts(ctx, videoID, store.KindSource)
	if err != nil {
		return nil, err
	}
	for i := len(arts) - 1; i >= 0; i-- {
		if p.st.ArtifactPresent(arts[i]) {
			return arts[i], nil
		}
	}
	return nil, app.E(app.CodeInternal, "找不到视频 %s 的源文件", videoID)
}

// --- extract_audio ---

type audioMeta struct {
	TimelineOffsetMs int64 `json:"timeline_offset_ms"`
}

func (p *Pipeline) extractAudio(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	opt, err := options(sc)
	if err != nil {
		return nil, err
	}
	info, err := p.probeInfo(ctx, sc)
	if err != nil {
		return nil, err
	}
	src, err := p.sourceOf(ctx, sc.VideoID())
	if err != nil {
		return nil, err
	}
	fp := jobs.Fingerprint(app.StageExtractAudio, audioVersion, map[string]any{
		"source_sha256": src.SHA256, "ffmpeg": p.tools.Version(tools.ToolFFmpeg), "params": "ac1-ar16000-pcm_s16le",
	})
	if a := p.cached(ctx, sc, store.KindAudio, fp, false); a != nil {
		return reused(a), nil
	}
	// The WAV is deleted after a successful transcription (§6.2). When the
	// transcript it produced is still valid, the purged row is enough: the
	// transcribe stage will hit its own cache and never open the file.
	if !sc.IgnoreCache && opt.ReprocessFrom != jobs.ReprocessTranscribe {
		if a, _ := p.st.FindArtifact(ctx, store.KindAudio, fp); a != nil && a.Purged {
			if tfp, err := p.transcribeFingerprint(a.SHA256, opt); err == nil {
				if raw, _ := p.st.FindArtifact(ctx, store.KindASRRaw, tfp); raw != nil && p.st.VerifyArtifact(raw) {
					return reused(a), nil
				}
			}
		}
	}
	ffmpeg, err := p.tools.Path(tools.ToolFFmpeg)
	if err != nil {
		return nil, err
	}
	rel := path.Join(p.st.VideoDir(sc.VideoID()), "audio.wav")
	if err := media.ExtractAudio(ctx, p.runner, ffmpeg, p.st.Abs(src.Path), p.st.Abs(rel), info.DurationMs, sc.Progress); err != nil {
		return nil, err
	}
	fi, err := store.SHA256File(p.st.Abs(rel))
	if err != nil {
		return nil, err
	}
	meta, _ := json.Marshal(audioMeta{TimelineOffsetMs: media.TimelineOffsetMs(info)})
	a, err := p.st.PutArtifact(ctx, &store.Artifact{VideoID: sc.VideoID(), Kind: store.KindAudio, Fingerprint: fp, Path: rel, SHA256: fi.SHA256, SizeBytes: fi.Size, Meta: meta})
	if err != nil {
		return nil, err
	}
	return done(a), nil
}

// --- transcribe ---

// language returns the language requested for the job: the import option,
// else the configured default.
func (p *Pipeline) language(opt JobOptions) string {
	if opt.Language != "" {
		return opt.Language
	}
	if l := p.cfg().ASR.Language; l != "" {
		return l
	}
	return "auto"
}

func (p *Pipeline) transcribeFingerprint(audioSHA string, opt JobOptions) (string, error) {
	cfg := p.cfg()
	model := ASRModel(cfg, p.tools)
	_, modelSHA, err := p.tools.ModelPath(model)
	if err != nil {
		return "", err
	}
	return jobs.Fingerprint(app.StageTranscribe, asrVersion, map[string]any{
		"audio_sha256": audioSHA, "whisper": p.tools.Version(tools.ToolWhisperCLI), "model": model, "model_sha256": modelSHA,
		"language": p.language(opt), "prompt_zh": asr.PromptZH, "prompt_en": promptEN, "vad": cfg.ASR.VAD,
	}), nil
}

func (p *Pipeline) transcribe(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	opt, err := options(sc)
	if err != nil {
		return nil, err
	}
	audio, err := p.depArtifact(ctx, sc, app.StageExtractAudio)
	if err != nil {
		return nil, err
	}
	fp, err := p.transcribeFingerprint(audio.SHA256, opt)
	if err != nil {
		return nil, err
	}
	videoID := sc.VideoID()
	if a := p.cached(ctx, sc, store.KindASRRaw, fp, true); a != nil {
		if tr, err := p.transcriptFor(ctx, videoID, a); err != nil {
			return nil, err
		} else if tr != nil {
			if err := p.setTranscript(ctx, sc, tr); err != nil {
				return nil, err
			}
			return reused(a), nil
		}
	}
	if !p.st.ArtifactPresent(audio) {
		return nil, app.E(app.CodeInternal, "音频产物已被清理，请重试任务以重新提取")
	}
	cfg := p.cfg()
	model := ASRModel(cfg, p.tools)
	bin, err := p.tools.Path(tools.ToolWhisperCLI)
	if err != nil {
		return nil, err
	}
	modelPath, modelSHA, err := p.tools.ModelPath(model)
	if err != nil {
		return nil, err
	}
	vadPath := ""
	if cfg.ASR.VAD {
		if vadPath, _, err = p.tools.ModelPath(tools.ModelSileroVAD); err != nil {
			return nil, err
		}
	}
	threads := cfg.ASR.Threads
	if threads <= 0 {
		threads = asr.DefaultThreads()
	}
	w := asr.NewWhisperCpp(p.runner, bin, modelPath, vadPath)
	audioAbs := p.st.Abs(audio.Path)
	lang := p.language(opt)
	if lang == "auto" {
		if lang, err = w.DetectLanguage(ctx, asr.DetectRequest{Audio: audioAbs, Threads: threads}); err != nil {
			return nil, err
		}
	}
	prompt := asr.PromptFor(lang)
	if lang == "en" {
		prompt = promptEN
	}
	v, err := p.st.GetVideo(ctx, videoID)
	if err != nil {
		return nil, err
	}
	var duration int64
	if v.DurationMs != nil {
		duration = *v.DurationMs
	}
	trID := app.NewID(app.PrefixTranscript)
	rel := path.Join(p.st.VideoDir(videoID), "asr", trID+".json")
	if err := os.MkdirAll(filepath.Dir(p.st.Abs(rel)), 0o755); err != nil {
		return nil, err
	}
	res, err := w.Transcribe(ctx, asr.Request{
		Audio: audioAbs, Language: lang, Threads: threads, Prompt: prompt, VAD: cfg.ASR.VAD,
		OutPrefix: strings.TrimSuffix(p.st.Abs(rel), ".json"), DurationMs: duration, Progress: sc.Progress,
	})
	if err != nil {
		return nil, err
	}
	var am audioMeta
	_ = json.Unmarshal(audio.Meta, &am)
	segs, meta := asr.PostProcess(res.Segments, am.TimelineOffsetMs)
	if len(segs) == 0 {
		return nil, app.E(app.CodeASRFailed, "没有识别到任何语音内容")
	}
	fi, err := store.SHA256File(res.RawJSONPath)
	if err != nil {
		return nil, err
	}
	rows := make([]store.Segment, len(segs))
	for i, s := range segs {
		rows[i] = store.Segment{StartMs: s.StartMs, EndMs: s.EndMs, Text: s.Text}
	}
	params, _ := json.Marshal(map[string]any{"language": lang, "requested_language": p.language(opt), "threads": threads, "vad": cfg.ASR.VAD, "prompt": prompt})
	metaJSON, _ := json.Marshal(meta)
	tr := &store.Transcript{
		ID: trID, VideoID: videoID, Language: lang, Engine: "whispercpp@" + p.tools.Version(tools.ToolWhisperCLI),
		Model: model + "@" + modelSHA, Params: params, TimelineOffsetMs: am.TimelineOffsetMs, Meta: metaJSON,
	}
	var art *store.Artifact
	err = p.st.Write(ctx, func(tx *store.Tx) error {
		if art, err = tx.PutArtifact(ctx, &store.Artifact{VideoID: videoID, Kind: store.KindASRRaw, Fingerprint: fp, Path: rel, SHA256: fi.SHA256, SizeBytes: fi.Size}); err != nil {
			return err
		}
		tr.ArtifactID = art.ID
		return tx.InsertTranscript(ctx, tr, rows)
	})
	if err != nil {
		return nil, err
	}
	if err := p.setTranscript(ctx, sc, tr); err != nil {
		return nil, err
	}
	if !cfg.Media.KeepAudio {
		// §6.2: the WAV is rebuilt by fingerprint when it is needed again.
		if err := p.st.PurgeArtifact(ctx, audio.ID); err != nil {
			p.log.Warn("purge audio failed", "video_id", videoID, "error", err)
		}
	}
	return done(art), nil
}

// transcriptFor returns the transcript of videoID built from the raw ASR
// artifact a, or nil when no video has one yet. Two videos with identical
// audio share the artifact (same fingerprint) but each needs its own
// transcript version, because segment IDs belong to one video: the other
// video's transcript is cloned instead of running Whisper again.
func (p *Pipeline) transcriptFor(ctx context.Context, videoID string, a *store.Artifact) (*store.Transcript, error) {
	own, err := p.st.ListTranscripts(ctx, videoID)
	if err != nil {
		return nil, err
	}
	for _, tr := range own {
		if tr.ArtifactID == a.ID {
			return tr, nil
		}
	}
	other, err := p.st.FindTranscriptByArtifact(ctx, a.ID)
	if err != nil || other == nil {
		return nil, err
	}
	segs, err := p.st.Segments(ctx, other.ID)
	if err != nil {
		return nil, err
	}
	rows := make([]store.Segment, len(segs))
	for i, s := range segs {
		rows[i] = store.Segment{StartMs: s.StartMs, EndMs: s.EndMs, Text: s.Text}
	}
	tr := &store.Transcript{
		VideoID: videoID, ArtifactID: a.ID, Language: other.Language, Engine: other.Engine, Model: other.Model,
		Params: other.Params, TimelineOffsetMs: other.TimelineOffsetMs, Meta: other.Meta,
	}
	if err := p.st.InsertTranscript(ctx, tr, rows); err != nil {
		return nil, err
	}
	return tr, nil
}

// setTranscript makes tr the current transcript of the video and records it
// on the job.
func (p *Pipeline) setTranscript(ctx context.Context, sc *jobs.StageContext, tr *store.Transcript) error {
	if _, err := p.st.UpdateVideo(ctx, tr.VideoID, store.VideoPatch{Language: &tr.Language, CurrentTranscriptID: &tr.ID}); err != nil {
		return err
	}
	p.changed(tr.VideoID)
	return sc.SetResult(func(r *app.JobResult) { r.TranscriptID = &tr.ID })
}

// --- playback ---

// PlaybackMeta is the meta JSON of a playback artifact.
type PlaybackMeta struct {
	Kind string `json:"kind"`
	Mime string `json:"mime"`
}

func (p *Pipeline) playback(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	info, err := p.probeInfo(ctx, sc)
	if err != nil {
		return nil, err
	}
	src, err := p.sourceOf(ctx, sc.VideoID())
	if err != nil {
		return nil, err
	}
	maxHeight := p.cfg().Media.PlaybackMaxHeight
	fp := jobs.Fingerprint(app.StagePlayback, playbackVersion, map[string]any{
		"source_sha256": src.SHA256, "ffmpeg": p.tools.Version(tools.ToolFFmpeg), "max_height": maxHeight,
	})
	if a := p.cached(ctx, sc, store.KindPlayback, fp, false); a != nil {
		p.changed(sc.VideoID())
		return reused(a), nil
	}
	plan := media.PlanPlayback(info, maxHeight)
	meta, _ := json.Marshal(PlaybackMeta{Kind: string(plan.Kind), Mime: plan.Mime})
	art := &store.Artifact{VideoID: sc.VideoID(), Kind: store.KindPlayback, Fingerprint: fp, Meta: meta}
	if !plan.NeedsBuild() {
		// §10.1: a directly playable source is served as is.
		art.Path, art.SHA256, art.SizeBytes = src.Path, src.SHA256, src.SizeBytes
	} else {
		ffmpeg, err := p.tools.Path(tools.ToolFFmpeg)
		if err != nil {
			return nil, err
		}
		rel := path.Join(p.st.VideoDir(sc.VideoID()), "playback.mp4")
		if err := media.BuildPlayback(ctx, p.runner, ffmpeg, plan, p.st.Abs(src.Path), p.st.Abs(rel), info.DurationMs, sc.Progress); err != nil {
			return nil, err
		}
		fi, err := store.SHA256File(p.st.Abs(rel))
		if err != nil {
			return nil, err
		}
		art.Path, art.SHA256, art.SizeBytes = rel, fi.SHA256, fi.Size
	}
	a, err := p.st.PutArtifact(ctx, art)
	if err != nil {
		return nil, err
	}
	p.changed(sc.VideoID())
	return done(a), nil
}
