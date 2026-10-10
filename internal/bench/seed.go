// Package bench holds the performance benchmarks of the engine and the data
// generator they share. It measures the paths a user can feel: the read API
// behind the library, note and transcript views, the scheduler's per-job
// overhead, and the pure text processing of long lectures.
//
// Run with:
//
//	go test -run '^$' -bench . -benchmem ./internal/bench
//	go run ./internal/bench/cmd/loadtest          # concurrent HTTP load
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"watchink/internal/app"
	"watchink/internal/infra/store"
)

// Shape describes a synthetic library.
type Shape struct {
	Videos   int // number of processed videos
	Segments int // transcript segments per video (a 1 h lecture ≈ 600–900)
	Points   int // cited points per note
}

// Library is what Seed created.
type Library struct {
	VideoIDs []string
	JobIDs   []string
}

// sentence is one transcript line; the mix of CJK and Latin text matches
// what Whisper produces for a Chinese lecture with English terms.
const sentence = "这一节我们讨论 gradient descent 的收敛条件，以及学习率对训练稳定性的影响。"

// Segments returns n synthetic segments of about 4 s each.
func Segments(n int) []store.Segment {
	segs := make([]store.Segment, n)
	for i := range segs {
		segs[i] = store.Segment{StartMs: int64(i) * 4000, EndMs: int64(i)*4000 + 3800, Text: fmt.Sprintf("%s（第 %d 句）", sentence, i+1)}
	}
	return segs
}

// Seed fills the store with shape.Videos fully processed videos: a
// transcript with segments, a note with citations and both note files on
// disk, and one succeeded job each.
func Seed(ctx context.Context, st *store.Store, shape Shape) (*Library, error) {
	lib := &Library{}
	for i := 0; i < shape.Videos; i++ {
		videoID, jobID, err := seedVideo(ctx, st, i, shape)
		if err != nil {
			return nil, err
		}
		lib.VideoIDs = append(lib.VideoIDs, videoID)
		lib.JobIDs = append(lib.JobIDs, jobID)
	}
	return lib, nil
}

func seedVideo(ctx context.Context, st *store.Store, i int, shape Shape) (videoID, jobID string, err error) {
	videoID = app.NewID(app.PrefixVideo)
	duration := int64(shape.Segments) * 4000
	dir := st.VideoDir(videoID)
	segs := Segments(shape.Segments)

	noteID := app.NewID(app.PrefixNote)
	var md strings.Builder
	fmt.Fprintf(&md, "# 机器学习导论 第 %d 讲\n\n## 概览\n\n本讲介绍优化方法。\n\n## 1. 梯度下降\n\n", i+1)
	cites := make([]store.Citation, shape.Points)

	err = st.Write(ctx, func(tx *store.Tx) error {
		v := &store.Video{ID: videoID, SourceType: app.SourceFile, SourceKey: fmt.Sprintf("file:%d:bench%06d", duration, i),
			SourceURI: fmt.Sprintf("/lectures/ml-%03d.mp4", i+1), Title: fmt.Sprintf("机器学习导论 第 %d 讲", i+1), DurationMs: &duration}
		if err := tx.CreateVideo(ctx, v); err != nil {
			return err
		}
		raw, err := tx.PutArtifact(ctx, &store.Artifact{VideoID: videoID, Kind: store.KindASRRaw, Fingerprint: "bench-asr-" + videoID})
		if err != nil {
			return err
		}
		tr := &store.Transcript{VideoID: videoID, ArtifactID: raw.ID, Language: "zh", Engine: "whispercpp@bench", Model: "bench"}
		if err := tx.InsertTranscript(ctx, tr, segs); err != nil {
			return err
		}
		for p := range cites {
			seg := segs[(p*len(segs))/max(shape.Points, 1)]
			cites[p] = store.Citation{RefNo: p + 1, PointID: fmt.Sprintf("P1.%d", p+1), SegmentID: seg.ID, StartMs: seg.StartMs, EndMs: seg.EndMs}
			fmt.Fprintf(&md, "- **要点 %d**：学习率过大时损失会发散。 [%02d:%02d](%s%s)\n", p+1, seg.StartMs/60000, seg.StartMs/1000%60, app.SegmentLinkScheme, seg.ID)
		}
		mdRel, jsonRel := path.Join(dir, "notes", noteID, "note.md"), path.Join(dir, "notes", noteID, "note.json")
		mdInfo, err := st.WriteFileAtomic(mdRel, []byte(md.String()))
		if err != nil {
			return err
		}
		doc, _ := json.Marshal(map[string]any{"note_id": noteID, "title": v.Title, "markdown_chars": md.Len()})
		jsonInfo, err := st.WriteFileAtomic(jsonRel, doc)
		if err != nil {
			return err
		}
		mdArt, err := tx.PutArtifact(ctx, &store.Artifact{VideoID: videoID, Kind: store.KindNoteMD, Fingerprint: "bench-md-" + videoID, Path: mdRel, SHA256: mdInfo.SHA256, SizeBytes: mdInfo.Size})
		if err != nil {
			return err
		}
		jsArt, err := tx.PutArtifact(ctx, &store.Artifact{VideoID: videoID, Kind: store.KindNoteJSON, Fingerprint: "bench-json-" + videoID, Path: jsonRel, SHA256: jsonInfo.SHA256, SizeBytes: jsonInfo.Size})
		if err != nil {
			return err
		}
		job := &store.Job{Kind: app.JobKindProcessVideo, VideoID: videoID, Status: app.JobSucceeded, RequestedBy: "bench",
			Result: app.JobResult{TranscriptID: &tr.ID, NoteID: &noteID}}
		created, err := tx.CreateJob(ctx, job, []string{app.StageAcquire, app.StageTranscribe, app.StageNoteRender})
		if err != nil {
			return err
		}
		jobID = created.ID
		if err := tx.InsertNote(ctx, &store.Note{ID: noteID, VideoID: videoID, TranscriptID: tr.ID, Title: v.Title,
			MDArtifactID: mdArt.ID, JSONArtifactID: jsArt.ID, LLMProvider: "openai-compatible", LLMModel: "bench", PromptVersion: "bench", CreatedByJob: jobID}, cites); err != nil {
			return err
		}
		lang := "zh"
		_, err = tx.UpdateVideo(ctx, videoID, store.VideoPatch{Language: &lang, CurrentTranscriptID: &tr.ID, CurrentNoteID: &noteID})
		return err
	})
	return videoID, jobID, err
}
