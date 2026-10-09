package pipeline

import (
	"context"
	"encoding/json"
	"path"
	"path/filepath"
	"sync"
	"time"

	"watchink/internal/app"
	"watchink/internal/config"
	"watchink/internal/jobs"
	"watchink/internal/llm"
	"watchink/internal/notes"
	"watchink/internal/store"
)

const (
	noteMapVersion    = 1
	noteReduceVersion = 1
	noteRenderVersion = 1
)

// mapBundle is the stage-level artifact of note_map: every chunk result in
// order. note_reduce reads it, so it never has to redo the chunking.
type mapBundle struct {
	TranscriptID string               `json:"transcript_id"`
	Chunks       []*notes.ChunkResult `json:"chunks"`
}

// modelID identifies the online model in fingerprints: a different endpoint
// or model must not reuse another one's output.
func modelID(cfg app.Settings) map[string]any {
	return map[string]any{"base_url": cfg.LLM.BaseURL, "model": cfg.LLM.Model, "temperature": cfg.LLM.Temperature}
}

// transcriptOf loads the transcript produced (or reused) by the transcribe
// stage of this job, with its segments.
func (p *Pipeline) transcriptOf(ctx context.Context, sc *jobs.StageContext) (*store.Transcript, []app.Segment, error) {
	raw, err := p.depArtifact(ctx, sc, app.StageTranscribe)
	if err != nil {
		return nil, nil, err
	}
	tr, err := p.transcriptFor(ctx, sc.VideoID(), raw)
	if err != nil {
		return nil, nil, err
	}
	if tr == nil {
		return nil, nil, app.E(app.CodeInternal, "转写产物 %s 没有对应的字幕版本", raw.ID)
	}
	segs, err := p.segments(ctx, tr.ID)
	return tr, segs, err
}

func (p *Pipeline) segments(ctx context.Context, transcriptID string) ([]app.Segment, error) {
	rows, err := p.st.Segments(ctx, transcriptID)
	if err != nil {
		return nil, err
	}
	out := make([]app.Segment, len(rows))
	for i, r := range rows {
		out[i] = r.DTO()
	}
	return out, nil
}

func (p *Pipeline) llmClient(sc *jobs.StageContext) (llm.Client, error) {
	// D-18: every model request takes a slot of the global llm semaphore.
	cfg := LLMConfig(p.cfg(), func(ctx context.Context) (func(), error) {
		return sc.Acquire(ctx, jobs.ResourceLLM)
	})
	// A healed request is not an error, but it must leave a trace: the
	// engine log and the job's event history both record what was changed.
	jobID, stage := sc.JobID(), sc.Stage
	cfg.Warn = func(msg string) {
		p.log.Warn("llm request adjusted", "job_id", jobID, "stage", stage, "model", cfg.Model, "detail", msg)
		_ = p.st.AddJobEvent(context.Background(), jobID, store.EventInput{
			Type: "stage.warning", Data: map[string]string{"stage": stage, "message": msg},
		})
	}
	return llm.New(cfg)
}

func (p *Pipeline) chunkOptions() notes.Options {
	cfg := p.cfg()
	d, err := config.ParseDuration(cfg.Note.ChunkMaxDuration)
	if err != nil || d <= 0 {
		d = 10 * time.Minute
	}
	return notes.Options{MaxTokens: cfg.Note.ChunkMaxTokens, MaxDuration: d}
}

func (p *Pipeline) noteMap(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	tr, segs, err := p.transcriptOf(ctx, sc)
	if err != nil {
		return nil, err
	}
	cfg := p.cfg()
	chunks := notes.ChunkSegments(segs, p.chunkOptions())
	chunkFP := make([]string, len(chunks))
	for i, c := range chunks {
		chunkFP[i] = jobs.Fingerprint("note_map_chunk", noteMapVersion, map[string]any{
			"transcript_id": tr.ID, "chunk": c.Key(), "prompt_version": notes.PromptVersion, "model": modelID(cfg),
		})
	}
	fp := jobs.Fingerprint(app.StageNoteMap, noteMapVersion, map[string]any{"chunks": chunkFP})
	if a := p.cached(ctx, sc, store.KindMapChunk, fp, true); a != nil {
		return reused(a), nil
	}
	// §7.5: without a configured model the job fails here at once instead
	// of sitting in running.
	client, err := p.llmClient(sc)
	if err != nil {
		return nil, err
	}
	v, err := p.st.GetVideo(ctx, sc.VideoID())
	if err != nil {
		return nil, err
	}
	var duration int64
	if v.DurationMs != nil {
		duration = *v.DurationMs
	}
	dir := path.Join(p.st.VideoDir(v.ID), "work")
	var mu sync.Mutex
	finished := 0
	tick := func() {
		mu.Lock()
		finished++
		n := finished
		mu.Unlock()
		sc.Progress(float64(n) / float64(len(chunks)))
	}
	results, err := notes.MapAll(ctx, client, chunks, notes.MapAllOptions{
		VideoTitle: v.Title, DurationMs: duration, Concurrency: cfg.LLM.MaxConcurrency,
		Cached: func(i int, _ notes.Chunk) *notes.ChunkResult {
			a := p.cached(ctx, sc, store.KindMapChunk, chunkFP[i], true)
			if a == nil {
				return nil
			}
			r := new(notes.ChunkResult)
			if p.readJSON(a, r) != nil {
				return nil
			}
			tick()
			return r
		},
		// Each finished chunk is registered right away so an interrupted
		// or failed job resumes from the chunks it already has (§7.6).
		OnResult: func(i int, r *notes.ChunkResult) error {
			_, err := p.putJSON(ctx, v.ID, store.KindMapChunk, chunkFP[i], path.Join(dir, chunkFP[i]+".json"), r)
			tick()
			return err
		},
	})
	if err != nil {
		return nil, err
	}
	a, err := p.putJSON(ctx, v.ID, store.KindMapChunk, fp, path.Join(dir, "map-"+fp+".json"), mapBundle{TranscriptID: tr.ID, Chunks: results})
	if err != nil {
		return nil, err
	}
	return done(a), nil
}

func (p *Pipeline) noteReduce(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	bundleArt, err := p.depArtifact(ctx, sc, app.StageNoteMap)
	if err != nil {
		return nil, err
	}
	cfg := p.cfg()
	fp := jobs.Fingerprint(app.StageNoteReduce, noteReduceVersion, map[string]any{
		"map_sha256": bundleArt.SHA256, "prompt_version": notes.PromptVersion, "model": modelID(cfg),
	})
	if a := p.cached(ctx, sc, store.KindReduce, fp, true); a != nil {
		return reused(a), nil
	}
	var bundle mapBundle
	if err := p.readJSON(bundleArt, &bundle); err != nil {
		return nil, err
	}
	client, err := p.llmClient(sc)
	if err != nil {
		return nil, err
	}
	v, err := p.st.GetVideo(ctx, sc.VideoID())
	if err != nil {
		return nil, err
	}
	var duration int64
	if v.DurationMs != nil {
		duration = *v.DurationMs
	}
	res, err := notes.Reduce(ctx, client, notes.ReduceInput{VideoTitle: v.Title, DurationMs: duration, Chunks: bundle.Chunks})
	if err != nil {
		return nil, err
	}
	a, err := p.putJSON(ctx, v.ID, store.KindReduce, fp, path.Join(p.st.VideoDir(v.ID), "work", "reduce-"+fp+".json"), res)
	if err != nil {
		return nil, err
	}
	return done(a), nil
}

func (p *Pipeline) noteRender(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	reduceArt, err := p.depArtifact(ctx, sc, app.StageNoteReduce)
	if err != nil {
		return nil, err
	}
	videoID := sc.VideoID()
	fp := jobs.Fingerprint(app.StageNoteRender, noteRenderVersion, map[string]any{
		"reduce_sha256": reduceArt.SHA256, "renderer": notes.RendererVersion,
	})
	if a := p.cached(ctx, sc, store.KindNoteMD, fp, true); a != nil {
		if n := p.noteByArtifact(ctx, videoID, a.ID); n != nil {
			if err := p.setNote(ctx, sc, n.ID); err != nil {
				return nil, err
			}
			return reused(a), nil
		}
	}
	var red notes.ReduceResult
	if err := p.readJSON(reduceArt, &red); err != nil {
		return nil, err
	}
	// The map bundle is the note_map output this reduce was built from.
	var bundle mapBundle
	mapArt, err := p.mapBundleOf(ctx, sc)
	if err != nil {
		return nil, err
	}
	if err := p.readJSON(mapArt, &bundle); err != nil {
		return nil, err
	}
	tr, err := p.st.GetTranscript(ctx, bundle.TranscriptID)
	if err != nil {
		return nil, err
	}
	segs, err := p.segments(ctx, tr.ID)
	if err != nil {
		return nil, err
	}
	v, err := p.st.GetVideo(ctx, videoID)
	if err != nil {
		return nil, err
	}
	var duration int64
	if v.DurationMs != nil {
		duration = *v.DurationMs
	}
	cfg := p.cfg()
	noteID := app.NewID(app.PrefixNote)
	out, err := notes.Render(notes.RenderInput{
		VideoID: videoID, NoteID: noteID, TranscriptID: tr.ID, Source: VideoSource(v), VideoTitle: v.Title,
		DurationMs: duration, Language: tr.Language, GeneratedAt: time.Now().UTC(),
		Model: cfg.LLM.Model, Segments: segs, Chunks: bundle.Chunks, Reduce: &red,
	})
	if err != nil {
		return nil, err
	}
	dir := path.Join(p.st.VideoDir(videoID), "notes", noteID)
	mdInfo, err := p.st.WriteFileAtomic(path.Join(dir, "note.md"), []byte(out.Markdown))
	if err != nil {
		return nil, err
	}
	docJSON, err := json.MarshalIndent(out.Document, "", "  ")
	if err != nil {
		return nil, err
	}
	jsonInfo, err := p.st.WriteFileAtomic(path.Join(dir, "note.json"), docJSON)
	if err != nil {
		return nil, err
	}
	coverage, _ := json.Marshal(out.Coverage)
	cites := make([]store.Citation, len(out.Citations))
	for i, c := range out.Citations {
		cites[i] = store.Citation{RefNo: c.RefNo, PointID: c.PointID, SegmentID: c.SegmentID, StartMs: c.StartMs, EndMs: c.EndMs}
	}
	var md *store.Artifact
	err = p.st.Write(ctx, func(tx *store.Tx) error {
		if md, err = tx.PutArtifact(ctx, &store.Artifact{VideoID: videoID, Kind: store.KindNoteMD, Fingerprint: fp, Path: path.Join(dir, "note.md"), SHA256: mdInfo.SHA256, SizeBytes: mdInfo.Size}); err != nil {
			return err
		}
		js, err := tx.PutArtifact(ctx, &store.Artifact{VideoID: videoID, Kind: store.KindNoteJSON, Fingerprint: fp, Path: path.Join(dir, "note.json"), SHA256: jsonInfo.SHA256, SizeBytes: jsonInfo.Size})
		if err != nil {
			return err
		}
		if err := tx.InsertNote(ctx, &store.Note{
			ID: noteID, VideoID: videoID, TranscriptID: tr.ID, Title: out.Title, MDArtifactID: md.ID, JSONArtifactID: js.ID,
			LLMProvider: llm.Provider, LLMModel: cfg.LLM.Model, PromptVersion: notes.PromptVersion, Coverage: coverage, CreatedByJob: sc.JobID(),
		}, cites); err != nil {
			return err
		}
		_, err = tx.UpdateVideo(ctx, videoID, store.VideoPatch{CurrentNoteID: &noteID})
		return err
	})
	if err != nil {
		return nil, err
	}
	p.changed(videoID)
	if err := sc.SetResult(func(r *app.JobResult) { r.NoteID = &noteID }); err != nil {
		return nil, err
	}
	return done(md), nil
}

// mapBundleOf finds the note_map bundle of this job. note_render depends on
// note_reduce only, so it reads the note_map stage row through the job.
func (p *Pipeline) mapBundleOf(ctx context.Context, sc *jobs.StageContext) (*store.Artifact, error) {
	j, err := p.st.GetJob(ctx, sc.JobID())
	if err != nil {
		return nil, err
	}
	st := j.Stage(app.StageNoteMap)
	if st == nil || st.ArtifactID == "" {
		return nil, app.E(app.CodeInternal, "找不到 note_map 的产物")
	}
	return p.st.GetArtifact(ctx, st.ArtifactID)
}

func (p *Pipeline) noteByArtifact(ctx context.Context, videoID, mdArtifactID string) *store.Note {
	list, err := p.st.ListNotes(ctx, videoID)
	if err != nil {
		return nil
	}
	for _, n := range list {
		if n.MDArtifactID == mdArtifactID {
			return n
		}
	}
	return nil
}

func (p *Pipeline) setNote(ctx context.Context, sc *jobs.StageContext, noteID string) error {
	if _, err := p.st.UpdateVideo(ctx, sc.VideoID(), store.VideoPatch{CurrentNoteID: &noteID}); err != nil {
		return err
	}
	p.changed(sc.VideoID())
	return sc.SetResult(func(r *app.JobResult) { r.NoteID = &noteID })
}

// VideoSource converts a videos row to the source DTO. A local file shows
// only its base name: absolute paths of the importing machine do not belong
// in notes or exports.
func VideoSource(v *store.Video) app.VideoSource {
	src := app.VideoSource{Type: v.SourceType, URI: v.SourceURI}
	switch v.SourceType {
	case app.SourceYouTube, app.SourceBilibili:
		if _, id, ok := cutKey(v.SourceKey); ok {
			src.PlatformID = id
		}
	default:
		src.URI = filepath.Base(v.SourceURI)
	}
	return src
}

// cutKey splits "youtube:<id>" / "bilibili:<BV>:p<n>" into platform and id.
func cutKey(key string) (platform, id string, ok bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}
