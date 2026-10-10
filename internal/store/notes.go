package store

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"watchink/internal/app"
)

// Note is a row of the notes table: one immutable note version of a video.
type Note struct {
	ID             string
	VideoID        string
	TranscriptID   string
	Version        int // 1-based, unique per video
	Title          string
	MDArtifactID   string
	JSONArtifactID string
	LLMProvider    string
	LLMModel       string
	PromptVersion  string
	Coverage       json.RawMessage // §9.6, "{}" when empty
	CreatedByJob   string
	CreatedAt      string
}

// Citation is a row of the citations table.
type Citation struct {
	NoteID    string
	RefNo     int
	PointID   string
	SegmentID string
	StartMs   int64
	EndMs     int64
}

const noteCols = `id, video_id, transcript_id, version, title, md_artifact_id, json_artifact_id,
	llm_provider, llm_model, prompt_version, coverage, created_by_job, created_at`

func scanNote(sc scanner) (*Note, error) {
	var (
		n        Note
		coverage string
	)
	if err := sc.Scan(&n.ID, &n.VideoID, &n.TranscriptID, &n.Version, &n.Title, &n.MDArtifactID,
		&n.JSONArtifactID, &n.LLMProvider, &n.LLMModel, &n.PromptVersion, &coverage, &n.CreatedByJob,
		&n.CreatedAt); err != nil {
		return nil, err
	}
	n.Coverage = json.RawMessage(coverage)
	return &n, nil
}

// NextNoteVersion returns the version number the next note of the video
// gets (1 for the first). Inside Store.Write the value stays valid until the
// transaction ends.
func (q *Queries) NextNoteVersion(ctx context.Context, videoID string) (int, error) {
	v, _, err := queryOne(ctx, q, func(sc scanner) (int, error) {
		var v int
		return v, sc.Scan(&v)
	}, `SELECT COALESCE(MAX(version), 0) + 1 FROM notes WHERE video_id = ?`, videoID)
	return v, err
}

// InsertNote stores the note and its citations in one transaction. It fills
// n.ID / n.CreatedAt when empty and assigns n.Version (the next version of
// the video) when it is 0. Citations get NoteID = n.ID.
//
// It does not touch videos.current_note_id; do that with UpdateVideo in the
// same Store.Write transaction.
func (q *Queries) InsertNote(ctx context.Context, n *Note, citations []Citation) error {
	if n.ID == "" {
		n.ID = app.NewID(app.PrefixNote)
	}
	if n.CreatedAt == "" {
		n.CreatedAt = app.Now()
	}
	for i := range citations {
		citations[i].NoteID = n.ID
	}
	return q.inWrite(ctx, func(q *Queries) error {
		if n.Version == 0 {
			v, err := q.NextNoteVersion(ctx, n.VideoID)
			if err != nil {
				return err
			}
			n.Version = v
		}
		if _, err := q.exec(ctx, `INSERT INTO notes (`+noteCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			n.ID, n.VideoID, n.TranscriptID, n.Version, n.Title, n.MDArtifactID, n.JSONArtifactID,
			n.LLMProvider, n.LLMModel, n.PromptVersion, rawJSON(n.Coverage), n.CreatedByJob, n.CreatedAt); err != nil {
			return err
		}
		for batch := range slices.Chunk(citations, 400) {
			var sb strings.Builder
			sb.WriteString(`INSERT INTO citations (note_id, ref_no, point_id, segment_id, start_ms, end_ms) VALUES `)
			args := make([]any, 0, len(batch)*6)
			for i, c := range batch {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString("(?,?,?,?,?,?)")
				args = append(args, c.NoteID, c.RefNo, c.PointID, c.SegmentID, c.StartMs, c.EndMs)
			}
			if _, err := q.exec(ctx, sb.String(), args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetNote returns the note or NOT_FOUND.
func (q *Queries) GetNote(ctx context.Context, id string) (*Note, error) {
	n, ok, err := queryOne(ctx, q, scanNote, `SELECT `+noteCols+` FROM notes WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("笔记", id)
	}
	return n, nil
}

// CurrentNote returns the note videos.current_note_id points at, or
// (nil, nil) when the video has no note yet. NOT_FOUND for an unknown video.
func (q *Queries) CurrentNote(ctx context.Context, videoID string) (*Note, error) {
	var out *Note
	err := q.inRead(ctx, func(q *Queries) error {
		v, err := q.GetVideo(ctx, videoID)
		if err != nil || v.CurrentNoteID == "" {
			return err
		}
		out, err = q.GetNote(ctx, v.CurrentNoteID)
		return err
	})
	return out, err
}

// ListNotes returns the note versions of a video, newest version first.
func (q *Queries) ListNotes(ctx context.Context, videoID string) ([]*Note, error) {
	return queryAll(ctx, q, scanNote,
		`SELECT `+noteCols+` FROM notes WHERE video_id = ? ORDER BY version DESC`, videoID)
}

// ListCitations returns the citations of a note ordered by ref_no, joined
// with the cited segment text (the hover preview of the DTO).
func (q *Queries) ListCitations(ctx context.Context, noteID string) ([]app.Citation, error) {
	return queryAll(ctx, q, func(sc scanner) (app.Citation, error) {
		var c app.Citation
		err := sc.Scan(&c.RefNo, &c.PointID, &c.SegmentID, &c.StartMs, &c.EndMs, &c.Text)
		return c, err
	}, `SELECT c.ref_no, c.point_id, c.segment_id, c.start_ms, c.end_ms, s.text
		FROM citations c JOIN segments s ON s.id = c.segment_id
		WHERE c.note_id = ? ORDER BY c.ref_no`, noteID)
}
