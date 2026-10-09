package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"watchink/internal/app"
)

// Artifact kinds (§6.4).
const (
	KindSource   = "source"
	KindProbe    = "probe"
	KindAudio    = "audio"
	KindASRRaw   = "asr_raw"
	KindPlayback = "playback"
	KindMapChunk = "map_chunk"
	KindReduce   = "reduce"
	KindNoteMD   = "note_md"
	KindNoteJSON = "note_json"
	KindExport   = "export"
)

// Artifact is a row of the artifacts table: a registered file (or a
// metadata-only result when Path is empty) identified by (Kind, Fingerprint).
type Artifact struct {
	ID          string
	VideoID     string
	Kind        string
	Fingerprint string
	Path        string // relative to the data dir, "" for metadata-only artifacts
	SHA256      string
	SizeBytes   int64
	Purged      bool            // file deleted by policy; rebuild by fingerprint (§7.4)
	Meta        json.RawMessage // JSON object, "{}" when empty
	CreatedAt   string
}

const artifactCols = `id, video_id, kind, fingerprint, path, sha256, size_bytes, purged, meta, created_at`

func scanArtifact(sc scanner) (*Artifact, error) {
	var (
		a         Artifact
		path, sha sql.NullString
		size      sql.NullInt64
		meta      string
	)
	if err := sc.Scan(&a.ID, &a.VideoID, &a.Kind, &a.Fingerprint, &path, &sha, &size, &a.Purged, &meta, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.Path, a.SHA256, a.SizeBytes, a.Meta = path.String, sha.String, size.Int64, json.RawMessage(meta)
	return &a, nil
}

func rawJSON(m json.RawMessage) string {
	if len(m) == 0 {
		return "{}"
	}
	return string(m)
}

// PutArtifact registers a (insert or update on (kind, fingerprint)) and
// returns the stored row. When the pair already exists its ID and created_at
// are kept — other rows reference the ID — while video, path, sha256, size
// and meta are replaced and purged is reset to false. Register the row only
// after the file has been committed (§6.2).
func (q *Queries) PutArtifact(ctx context.Context, a *Artifact) (*Artifact, error) {
	id := a.ID
	if id == "" {
		id = app.NewID(app.PrefixArtifact)
	}
	created := a.CreatedAt
	if created == "" {
		created = app.Now()
	}
	var out *Artifact
	err := q.inWrite(ctx, func(q *Queries) error {
		_, err := q.exec(ctx, `INSERT INTO artifacts (`+artifactCols+`)
			VALUES (?,?,?,?,?,?,?,0,?,?)
			ON CONFLICT (kind, fingerprint) DO UPDATE SET
				video_id = excluded.video_id, path = excluded.path, sha256 = excluded.sha256,
				size_bytes = excluded.size_bytes, purged = 0, meta = excluded.meta`,
			id, a.VideoID, a.Kind, a.Fingerprint, nullable(a.Path), nullable(a.SHA256), a.SizeBytes,
			rawJSON(a.Meta), created)
		if err != nil {
			return err
		}
		out, err = q.FindArtifact(ctx, a.Kind, a.Fingerprint)
		return err
	})
	return out, err
}

// FindArtifact returns the artifact registered for (kind, fingerprint), or
// (nil, nil). Use Store.VerifyArtifact before reusing it.
func (q *Queries) FindArtifact(ctx context.Context, kind, fingerprint string) (*Artifact, error) {
	a, ok, err := queryOne(ctx, q, scanArtifact,
		`SELECT `+artifactCols+` FROM artifacts WHERE kind = ? AND fingerprint = ?`, kind, fingerprint)
	if err != nil || !ok {
		return nil, err
	}
	return a, nil
}

// GetArtifact returns the artifact or NOT_FOUND.
func (q *Queries) GetArtifact(ctx context.Context, id string) (*Artifact, error) {
	a, ok, err := queryOne(ctx, q, scanArtifact, `SELECT `+artifactCols+` FROM artifacts WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("产物", id)
	}
	return a, nil
}

// ListArtifacts returns the artifacts of a video, oldest first. A non-empty
// kind restricts the result to that kind.
func (q *Queries) ListArtifacts(ctx context.Context, videoID, kind string) ([]*Artifact, error) {
	if kind == "" {
		return queryAll(ctx, q, scanArtifact,
			`SELECT `+artifactCols+` FROM artifacts WHERE video_id = ? ORDER BY id`, videoID)
	}
	return queryAll(ctx, q, scanArtifact,
		`SELECT `+artifactCols+` FROM artifacts WHERE video_id = ? AND kind = ? ORDER BY id`, videoID, kind)
}

// MarkArtifactPurged records that the file of the artifact was deleted by
// policy (e.g. audio.wav after a successful transcription); the row stays so
// the artifact can be rebuilt by fingerprint. NOT_FOUND when missing. See
// Store.PurgeArtifact for the variant that also removes the file.
func (q *Queries) MarkArtifactPurged(ctx context.Context, id string) error {
	res, err := q.exec(ctx, `UPDATE artifacts SET purged = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("产物", id)
	}
	return nil
}
