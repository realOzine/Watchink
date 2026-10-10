package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"os"

	"watchink/internal/app"
	"watchink/internal/jobs"
	"watchink/internal/store"
)

func options(sc *jobs.StageContext) (JobOptions, error) {
	var opt JobOptions
	err := sc.DecodeOptions(&opt)
	return opt, err
}

// cached returns the registered artifact for (kind, fp) when the stage may
// reuse it. hash re-verifies the sha256; large media files only get the
// cheap existence + size check.
func (p *Pipeline) cached(ctx context.Context, sc *jobs.StageContext, kind, fp string, hash bool) *store.Artifact {
	if sc.IgnoreCache {
		return nil
	}
	a, err := p.st.FindArtifact(ctx, kind, fp)
	if err != nil || a == nil || a.Purged {
		return nil
	}
	if hash {
		if !p.st.VerifyArtifact(a) {
			return nil
		}
	} else if !p.st.ArtifactPresent(a) {
		return nil
	}
	return a
}

func reused(a *store.Artifact) *jobs.StageResult {
	return &jobs.StageResult{ArtifactID: a.ID, Reused: true, Fingerprint: a.Fingerprint}
}

func done(a *store.Artifact) *jobs.StageResult {
	return &jobs.StageResult{ArtifactID: a.ID, Fingerprint: a.Fingerprint}
}

// depArtifact loads the artifact a dependency stage produced.
func (p *Pipeline) depArtifact(ctx context.Context, sc *jobs.StageContext, stage string) (*store.Artifact, error) {
	d, ok := sc.Dep(stage)
	if !ok || d.ArtifactID == "" {
		return nil, app.E(app.CodeInternal, "阶段 %s 没有登记产物", stage)
	}
	return p.st.GetArtifact(ctx, d.ArtifactID)
}

func (p *Pipeline) readJSON(a *store.Artifact, v any) error {
	b, err := os.ReadFile(p.st.Abs(a.Path))
	if err != nil {
		return app.E(app.CodeInternal, "读取产物 %s 失败：%v", a.Path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return app.E(app.CodeInternal, "解析产物 %s 失败：%v", a.Path, err)
	}
	return nil
}

// putJSON writes v under rel and registers it as an artifact.
func (p *Pipeline) putJSON(ctx context.Context, videoID, kind, fp, rel string, v any) (*store.Artifact, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	fi, err := p.st.WriteFileAtomic(rel, buf.Bytes())
	if err != nil {
		return nil, err
	}
	return p.st.PutArtifact(ctx, &store.Artifact{VideoID: videoID, Kind: kind, Fingerprint: fp, Path: rel, SHA256: fi.SHA256, SizeBytes: fi.Size})
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

// video loads the job's video and its duration (0 until probe has run).
func (p *Pipeline) video(ctx context.Context, sc *jobs.StageContext) (*store.Video, int64, error) {
	v, err := p.st.GetVideo(ctx, sc.VideoID())
	if err != nil {
		return nil, 0, err
	}
	var duration int64
	if v.DurationMs != nil {
		duration = *v.DurationMs
	}
	return v, duration, nil
}
