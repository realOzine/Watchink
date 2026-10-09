package store

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"watchink/internal/app"
)

// maxSegments is the largest idx the 5-digit segment ID format can carry (§6.3).
const maxSegments = 99999

// Transcript is a row of the transcripts table. A transcript version is
// immutable once inserted, which keeps segment IDs stable (§6.3).
type Transcript struct {
	ID               string
	VideoID          string
	ArtifactID       string          // asr_raw artifact
	Language         string          // detected or requested language
	Engine           string          // whispercpp@<version>
	Model            string          // manifest ID + sha256
	Params           json.RawMessage // JSON object, "{}" when empty
	TimelineOffsetMs int64
	SegmentCount     int
	Meta             json.RawMessage // JSON object, "{}" when empty
	CreatedAt        string
}

// Segment is a row of the segments table. Idx is 1-based.
type Segment struct {
	ID           string
	TranscriptID string
	Idx          int
	StartMs      int64
	EndMs        int64
	Text         string
}

// DTO converts the row to the wire type.
func (s Segment) DTO() app.Segment {
	return app.Segment{SegmentID: s.ID, Idx: s.Idx, StartMs: s.StartMs, EndMs: s.EndMs, Text: s.Text}
}

const (
	transcriptCols = `id, video_id, artifact_id, language, engine, model, params, timeline_offset_ms,
		segment_count, meta, created_at`
	segmentCols = `id, transcript_id, idx, start_ms, end_ms, text`
)

func scanTranscript(sc scanner) (*Transcript, error) {
	var (
		t            Transcript
		params, meta string
	)
	if err := sc.Scan(&t.ID, &t.VideoID, &t.ArtifactID, &t.Language, &t.Engine, &t.Model, &params,
		&t.TimelineOffsetMs, &t.SegmentCount, &meta, &t.CreatedAt); err != nil {
		return nil, err
	}
	t.Params, t.Meta = json.RawMessage(params), json.RawMessage(meta)
	return &t, nil
}

func scanSegment(sc scanner) (Segment, error) {
	var s Segment
	err := sc.Scan(&s.ID, &s.TranscriptID, &s.Idx, &s.StartMs, &s.EndMs, &s.Text)
	return s, err
}

// InsertTranscript stores the transcript and all of its segments in one
// transaction. It fills t.ID / t.CreatedAt when empty, sets t.SegmentCount,
// and normalizes segs in place: Idx becomes the 1-based position, ID becomes
// app.SegmentID(t.ID, idx) and TranscriptID becomes t.ID.
//
// It does not touch videos.current_transcript_id; do that with UpdateVideo
// in the same Store.Write transaction.
func (q *Queries) InsertTranscript(ctx context.Context, t *Transcript, segs []Segment) error {
	if len(segs) > maxSegments {
		return app.E(app.CodeInternal, "字幕片段数量（%d）超过上限 %d", len(segs), maxSegments)
	}
	if t.ID == "" {
		t.ID = app.NewID(app.PrefixTranscript)
	}
	if t.CreatedAt == "" {
		t.CreatedAt = app.Now()
	}
	t.SegmentCount = len(segs)
	for i := range segs {
		segs[i].Idx = i + 1
		segs[i].TranscriptID = t.ID
		segs[i].ID = app.SegmentID(t.ID, i+1)
	}
	return q.inWrite(ctx, func(q *Queries) error {
		if _, err := q.exec(ctx, `INSERT INTO transcripts (`+transcriptCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			t.ID, t.VideoID, t.ArtifactID, t.Language, t.Engine, t.Model, rawJSON(t.Params),
			t.TimelineOffsetMs, t.SegmentCount, rawJSON(t.Meta), t.CreatedAt); err != nil {
			return err
		}
		// Multi-row inserts: 400 rows × 6 columns stays far below SQLite's
		// bound-variable limit while cutting statement overhead for long
		// lectures (thousands of segments).
		for batch := range slices.Chunk(segs, 400) {
			var sb strings.Builder
			sb.WriteString(`INSERT INTO segments (` + segmentCols + `) VALUES `)
			args := make([]any, 0, len(batch)*6)
			for i, s := range batch {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString("(?,?,?,?,?,?)")
				args = append(args, s.ID, s.TranscriptID, s.Idx, s.StartMs, s.EndMs, s.Text)
			}
			if _, err := q.exec(ctx, sb.String(), args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetTranscript returns the transcript or NOT_FOUND.
func (q *Queries) GetTranscript(ctx context.Context, id string) (*Transcript, error) {
	t, ok, err := queryOne(ctx, q, scanTranscript, `SELECT `+transcriptCols+` FROM transcripts WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("字幕版本", id)
	}
	return t, nil
}

// FindTranscriptByArtifact returns the newest transcript built from the
// asr_raw artifact, or (nil, nil). It lets a reused transcribe stage find the
// transcript version that belongs to the cached artifact.
func (q *Queries) FindTranscriptByArtifact(ctx context.Context, artifactID string) (*Transcript, error) {
	t, ok, err := queryOne(ctx, q, scanTranscript,
		`SELECT `+transcriptCols+` FROM transcripts WHERE artifact_id = ? ORDER BY id DESC LIMIT 1`, artifactID)
	if err != nil || !ok {
		return nil, err
	}
	return t, nil
}

// ListTranscripts returns the transcript versions of a video, newest first.
func (q *Queries) ListTranscripts(ctx context.Context, videoID string) ([]*Transcript, error) {
	return queryAll(ctx, q, scanTranscript,
		`SELECT `+transcriptCols+` FROM transcripts WHERE video_id = ? ORDER BY id DESC`, videoID)
}

// Segments returns every segment of the transcript ordered by idx.
func (q *Queries) Segments(ctx context.Context, transcriptID string) ([]Segment, error) {
	return queryAll(ctx, q, scanSegment,
		`SELECT `+segmentCols+` FROM segments WHERE transcript_id = ? ORDER BY idx`, transcriptID)
}

// SegmentsInRange returns the segments overlapping the half-open time range
// [startMs, endMs), ordered by idx. A nil bound is open. limit <= 0 means no
// limit.
func (q *Queries) SegmentsInRange(ctx context.Context, transcriptID string, startMs, endMs *int64, limit int) ([]Segment, error) {
	query := `SELECT ` + segmentCols + ` FROM segments WHERE transcript_id = ?`
	args := []any{transcriptID}
	if startMs != nil {
		query += ` AND end_ms > ?`
		args = append(args, *startMs)
	}
	if endMs != nil {
		query += ` AND start_ms < ?`
		args = append(args, *endMs)
	}
	query += ` ORDER BY idx`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	return queryAll(ctx, q, scanSegment, query, args...)
}

// SegmentsByIDs returns the existing segments among ids, ordered by
// transcript and idx. Unknown IDs are simply absent from the result, so
// callers validating citations can compare lengths or build a set.
func (q *Queries) SegmentsByIDs(ctx context.Context, ids []string) ([]Segment, error) {
	var out []Segment
	for chunk := range slices.Chunk(ids, 500) {
		segs, err := queryAll(ctx, q, scanSegment,
			`SELECT `+segmentCols+` FROM segments WHERE id IN (`+placeholders(len(chunk))+`)`, anySlice(chunk)...)
		if err != nil {
			return nil, err
		}
		out = append(out, segs...)
	}
	slices.SortFunc(out, func(a, b Segment) int {
		if c := strings.Compare(a.TranscriptID, b.TranscriptID); c != 0 {
			return c
		}
		return a.Idx - b.Idx
	})
	return slices.CompactFunc(out, func(a, b Segment) bool { return a.ID == b.ID }), nil
}
