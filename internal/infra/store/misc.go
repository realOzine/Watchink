package store

import (
	"context"
	"database/sql"
	"time"

	"watchink/internal/app"
)

// Upload is a row of the uploads table: a staged Web upload.
type Upload struct {
	ID         string
	Filename   string
	SizeBytes  int64
	SampleHash string // sampled hash, the basis of the source_key
	Path       string // relative to the data dir: uploads/<id>.part
	ExpiresAt  string
	ConsumedBy string // video ID once imported, "" before
}

// DTO converts the row to the wire type.
func (u *Upload) DTO() app.Upload {
	return app.Upload{UploadID: u.ID, Filename: u.Filename, SizeBytes: u.SizeBytes, ExpiresAt: u.ExpiresAt}
}

const uploadCols = `id, filename, size_bytes, sample_hash, path, expires_at, consumed_by`

func scanUpload(sc scanner) (*Upload, error) {
	var (
		u        Upload
		consumed sql.NullString
	)
	if err := sc.Scan(&u.ID, &u.Filename, &u.SizeBytes, &u.SampleHash, &u.Path, &u.ExpiresAt, &consumed); err != nil {
		return nil, err
	}
	u.ConsumedBy = consumed.String
	return &u, nil
}

// CreateUpload inserts u; an empty ID is filled in.
func (q *Queries) CreateUpload(ctx context.Context, u *Upload) error {
	if u.ID == "" {
		u.ID = app.NewID(app.PrefixUpload)
	}
	_, err := q.exec(ctx, `INSERT INTO uploads (`+uploadCols+`) VALUES (?,?,?,?,?,?,?)`,
		u.ID, u.Filename, u.SizeBytes, u.SampleHash, u.Path, u.ExpiresAt, nullable(u.ConsumedBy))
	return err
}

// GetUpload returns the upload or NOT_FOUND.
func (q *Queries) GetUpload(ctx context.Context, id string) (*Upload, error) {
	u, ok, err := queryOne(ctx, q, scanUpload, `SELECT `+uploadCols+` FROM uploads WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("上传", id)
	}
	return u, nil
}

// ConsumeUpload marks the upload as imported by consumedBy (a video ID) so
// the expiry sweep leaves its file to the acquire stage. Consuming it again
// for the same owner is a no-op; for a different owner it is a CONFLICT.
func (q *Queries) ConsumeUpload(ctx context.Context, id, consumedBy string) (*Upload, error) {
	var out *Upload
	err := q.inWrite(ctx, func(q *Queries) error {
		u, err := q.GetUpload(ctx, id)
		if err != nil {
			return err
		}
		if u.ConsumedBy != "" && u.ConsumedBy != consumedBy {
			return app.E(app.CodeConflict, "上传 %s 已被导入", id).WithDetail("consumed_by", u.ConsumedBy)
		}
		if _, err := q.exec(ctx, `UPDATE uploads SET consumed_by = ? WHERE id = ?`, consumedBy, id); err != nil {
			return err
		}
		u.ConsumedBy = consumedBy
		out = u
		return nil
	})
	return out, err
}

// ListExpiredUploads returns the uploads that were never imported and whose
// expiry is at or before now. Consumed uploads are excluded because a queued
// job may still need the staged file.
func (q *Queries) ListExpiredUploads(ctx context.Context, now time.Time) ([]*Upload, error) {
	all, err := queryAll(ctx, q, scanUpload,
		`SELECT `+uploadCols+` FROM uploads WHERE consumed_by IS NULL OR consumed_by = '' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	// Compared in Go: RFC 3339 strings with trimmed fractions do not sort
	// lexicographically, and the table is tiny.
	var out []*Upload
	for _, u := range all {
		t, err := time.Parse(time.RFC3339Nano, u.ExpiresAt)
		if err != nil || !t.After(now) {
			out = append(out, u)
		}
	}
	return out, nil
}

// ListUploads returns every upload row, oldest first.
func (q *Queries) ListUploads(ctx context.Context) ([]*Upload, error) {
	return queryAll(ctx, q, scanUpload, `SELECT `+uploadCols+` FROM uploads ORDER BY id`)
}

// DeleteUpload removes the row (not the file). Deleting a missing row is not
// an error.
func (q *Queries) DeleteUpload(ctx context.Context, id string) error {
	_, err := q.exec(ctx, `DELETE FROM uploads WHERE id = ?`, id)
	return err
}

// Export is a row of the exports table.
type Export struct {
	ID        string
	VideoID   string
	NoteID    string // "" for transcript exports
	Format    string
	Path      string // relative to the data dir
	SHA256    string
	CreatedAt string
}

const exportCols = `id, video_id, note_id, format, path, sha256, created_at`

func scanExport(sc scanner) (*Export, error) {
	var (
		e    Export
		note sql.NullString
	)
	if err := sc.Scan(&e.ID, &e.VideoID, &note, &e.Format, &e.Path, &e.SHA256, &e.CreatedAt); err != nil {
		return nil, err
	}
	e.NoteID = note.String
	return &e, nil
}

// CreateExport inserts e; empty ID / CreatedAt are filled in.
func (q *Queries) CreateExport(ctx context.Context, e *Export) error {
	if e.ID == "" {
		e.ID = app.NewID(app.PrefixExport)
	}
	if e.CreatedAt == "" {
		e.CreatedAt = app.Now()
	}
	_, err := q.exec(ctx, `INSERT INTO exports (`+exportCols+`) VALUES (?,?,?,?,?,?,?)`,
		e.ID, e.VideoID, nullable(e.NoteID), e.Format, e.Path, e.SHA256, e.CreatedAt)
	return err
}

// GetExport returns the export or NOT_FOUND.
func (q *Queries) GetExport(ctx context.Context, id string) (*Export, error) {
	e, ok, err := queryOne(ctx, q, scanExport, `SELECT `+exportCols+` FROM exports WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("导出", id)
	}
	return e, nil
}
