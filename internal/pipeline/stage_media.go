package pipeline

import (
	"context"
	"encoding/json"
	"path"

	"watchink/internal/app"
	"watchink/internal/jobs"
	"watchink/internal/media"
	"watchink/internal/store"
	"watchink/internal/tools"
)

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
	// The WAV is deleted after a successful transcription. When the
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
		// a directly playable source is served as is.
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
