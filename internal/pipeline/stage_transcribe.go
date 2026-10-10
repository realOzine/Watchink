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
	"watchink/internal/store"
	"watchink/internal/tools"
)

// promptEN gives Whisper a punctuated, cased English context: without it
// large-v3-turbo emits lowercase text with no punctuation
// (third_party/whisper/NOTES.md).
const promptEN = "The following is a lecture transcript, with proper punctuation and capitalization."

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

// asrSetup is what one Whisper run needs from the settings and the
// installed tools.
type asrSetup struct {
	bin       string // whisper-cli
	model     string // manifest ID
	modelPath string
	modelSHA  string
	vad       bool
	vadPath   string // empty unless vad
	threads   int
}

func (p *Pipeline) asrSetup() (asrSetup, error) {
	cfg := p.cfg()
	s := asrSetup{model: ASRModel(cfg, p.tools), vad: cfg.ASR.VAD, threads: cfg.ASR.Threads}
	var err error
	if s.bin, err = p.tools.Path(tools.ToolWhisperCLI); err != nil {
		return s, err
	}
	if s.modelPath, s.modelSHA, err = p.tools.ModelPath(s.model); err != nil {
		return s, err
	}
	if s.vad {
		if s.vadPath, _, err = p.tools.ModelPath(tools.ModelSileroVAD); err != nil {
			return s, err
		}
	}
	if s.threads <= 0 {
		s.threads = asr.DefaultThreads()
	}
	return s, nil
}

// transcribe turns the extracted audio into a transcript version: detect the
// language when asked to, run Whisper over the whole file, clean up
// the segments and store them with the raw output.
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

	setup, err := p.asrSetup()
	if err != nil {
		return nil, err
	}
	w := asr.NewWhisperCpp(p.runner, setup.bin, setup.modelPath, setup.vadPath)
	audioAbs := p.st.Abs(audio.Path)
	requested := p.language(opt)
	lang := requested
	if lang == "auto" {
		if lang, err = w.DetectLanguage(ctx, asr.DetectRequest{Audio: audioAbs, Threads: setup.threads}); err != nil {
			return nil, err
		}
	}
	prompt := asr.PromptFor(lang)
	if lang == "en" {
		prompt = promptEN
	}
	_, duration, err := p.video(ctx, sc)
	if err != nil {
		return nil, err
	}
	trID := app.NewID(app.PrefixTranscript)
	rawRel := path.Join(p.st.VideoDir(videoID), "asr", trID+".json")
	if err := os.MkdirAll(filepath.Dir(p.st.Abs(rawRel)), 0o755); err != nil {
		return nil, err
	}
	res, err := w.Transcribe(ctx, asr.Request{
		Audio: audioAbs, Language: lang, Threads: setup.threads, Prompt: prompt, VAD: setup.vad,
		OutPrefix: strings.TrimSuffix(p.st.Abs(rawRel), ".json"), DurationMs: duration, Progress: sc.Progress,
	})
	if err != nil {
		return nil, err
	}

	params, _ := json.Marshal(map[string]any{"language": lang, "requested_language": requested, "threads": setup.threads, "vad": setup.vad, "prompt": prompt})
	tr := &store.Transcript{
		ID: trID, VideoID: videoID, Language: lang, Engine: "whispercpp@" + p.tools.Version(tools.ToolWhisperCLI),
		Model: setup.model + "@" + setup.modelSHA, Params: params,
	}
	art, err := p.saveTranscript(ctx, tr, res, audio, fp, rawRel)
	if err != nil {
		return nil, err
	}
	if err := p.setTranscript(ctx, sc, tr); err != nil {
		return nil, err
	}
	if !p.cfg().Media.KeepAudio {
		// the WAV is rebuilt by fingerprint when it is needed again.
		if err := p.st.PurgeArtifact(ctx, audio.ID); err != nil {
			p.log.Warn("purge audio failed", "video_id", videoID, "error", err)
		}
	}
	return done(art), nil
}

// saveTranscript post-processes the Whisper segments (step 3), shifts
// them onto the video timeline and stores the transcript together with the
// raw output artifact in one transaction.
func (p *Pipeline) saveTranscript(ctx context.Context, tr *store.Transcript, res *asr.Result, audio *store.Artifact, fp, rawRel string) (*store.Artifact, error) {
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
	tr.TimelineOffsetMs = am.TimelineOffsetMs
	tr.Meta, _ = json.Marshal(meta)
	var art *store.Artifact
	err = p.st.Write(ctx, func(tx *store.Tx) error {
		if art, err = tx.PutArtifact(ctx, &store.Artifact{VideoID: tr.VideoID, Kind: store.KindASRRaw, Fingerprint: fp, Path: rawRel, SHA256: fi.SHA256, SizeBytes: fi.Size}); err != nil {
			return err
		}
		tr.ArtifactID = art.ID
		return tx.InsertTranscript(ctx, tr, rows)
	})
	return art, err
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
