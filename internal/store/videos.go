package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strings"

	"watchink/internal/app"
)

// Video is a row of the videos table.
type Video struct {
	ID                  string
	SourceType          string // file / upload / youtube / bilibili
	SourceKey           string // dedup key, unique
	SourceURI           string
	Title               string
	DurationMs          *int64
	Language            string
	CurrentTranscriptID string
	CurrentNoteID       string
	CreatedAt           string
	UpdatedAt           string
}

// VideoPatch lists the columns UpdateVideo may change; nil fields are left
// alone. An empty string clears a nullable column.
type VideoPatch struct {
	Title               *string
	DurationMs          *int64
	Language            *string
	CurrentTranscriptID *string
	CurrentNoteID       *string
}

// VideoFilter selects a page of videos, newest first.
type VideoFilter struct {
	TitleContains string
	Limit         int    // 0 = app.DefaultLimit, capped at app.MaxLimit
	Cursor        string // opaque value returned as next cursor by a previous call
}

const videoCols = `id, source_type, source_key, source_uri, title, duration_ms, language,
	current_transcript_id, current_note_id, created_at, updated_at`

func scanVideo(sc scanner) (*Video, error) {
	var (
		v                     Video
		uri, lang, trID, ntID sql.NullString
		dur                   sql.NullInt64
	)
	if err := sc.Scan(&v.ID, &v.SourceType, &v.SourceKey, &uri, &v.Title, &dur, &lang,
		&trID, &ntID, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return nil, err
	}
	v.SourceURI, v.Language = uri.String, lang.String
	v.CurrentTranscriptID, v.CurrentNoteID = trID.String, ntID.String
	if dur.Valid {
		v.DurationMs = &dur.Int64
	}
	return &v, nil
}

// CreateVideo inserts v. Empty ID / CreatedAt / UpdatedAt are filled in. A
// duplicate source_key yields a CONFLICT error for which IsUniqueViolation
// is true; inside Store.Write, look the key up first (FindVideoBySourceKey)
// and the single writer guarantees nobody can insert it in between.
func (q *Queries) CreateVideo(ctx context.Context, v *Video) error {
	if v.ID == "" {
		v.ID = app.NewID(app.PrefixVideo)
	}
	if v.CreatedAt == "" {
		v.CreatedAt = app.Now()
	}
	if v.UpdatedAt == "" {
		v.UpdatedAt = v.CreatedAt
	}
	var dur any
	if v.DurationMs != nil {
		dur = *v.DurationMs
	}
	_, err := q.exec(ctx, `INSERT INTO videos (`+videoCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		v.ID, v.SourceType, v.SourceKey, nullable(v.SourceURI), v.Title, dur, nullable(v.Language),
		nullable(v.CurrentTranscriptID), nullable(v.CurrentNoteID), v.CreatedAt, v.UpdatedAt)
	return err
}

// GetVideo returns the video or NOT_FOUND.
func (q *Queries) GetVideo(ctx context.Context, id string) (*Video, error) {
	v, ok, err := queryOne(ctx, q, scanVideo, `SELECT `+videoCols+` FROM videos WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("视频", id)
	}
	return v, nil
}

// FindVideoBySourceKey returns the video with the dedup key, or (nil, nil).
func (q *Queries) FindVideoBySourceKey(ctx context.Context, sourceKey string) (*Video, error) {
	v, ok, err := queryOne(ctx, q, scanVideo, `SELECT `+videoCols+` FROM videos WHERE source_key = ?`, sourceKey)
	if err != nil || !ok {
		return nil, err
	}
	return v, nil
}

// ListVideos returns one page, newest first, and the cursor of the next page
// ("" when there is none). TitleContains is a case-insensitive (ASCII)
// substring filter.
func (q *Queries) ListVideos(ctx context.Context, f VideoFilter) (videos []*Video, nextCursor string, err error) {
	limit := clampLimit(f.Limit)
	var (
		where []string
		args  []any
	)
	if f.Cursor != "" {
		after, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "id < ?")
		args = append(args, after)
	}
	if f.TitleContains != "" {
		where = append(where, `title LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(f.TitleContains)+"%")
	}
	query := `SELECT ` + videoCols + ` FROM videos`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)
	videos, err = queryAll(ctx, q, scanVideo, query, args...)
	if err != nil {
		return nil, "", err
	}
	if len(videos) > limit {
		videos = videos[:limit]
		nextCursor = encodeCursor(videos[limit-1].ID)
	}
	return videos, nextCursor, nil
}

// UpdateVideo applies the patch, refreshes updated_at and returns the new
// row. NOT_FOUND when the video does not exist.
func (q *Queries) UpdateVideo(ctx context.Context, id string, p VideoPatch) (*Video, error) {
	sets := []string{"updated_at = ?"}
	args := []any{app.Now()}
	if p.Title != nil {
		sets, args = append(sets, "title = ?"), append(args, *p.Title)
	}
	if p.DurationMs != nil {
		sets, args = append(sets, "duration_ms = ?"), append(args, *p.DurationMs)
	}
	if p.Language != nil {
		sets, args = append(sets, "language = ?"), append(args, nullable(*p.Language))
	}
	if p.CurrentTranscriptID != nil {
		sets, args = append(sets, "current_transcript_id = ?"), append(args, nullable(*p.CurrentTranscriptID))
	}
	if p.CurrentNoteID != nil {
		sets, args = append(sets, "current_note_id = ?"), append(args, nullable(*p.CurrentNoteID))
	}
	args = append(args, id)
	var out *Video
	err := q.inWrite(ctx, func(q *Queries) error {
		res, err := q.exec(ctx, `UPDATE videos SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("视频", id)
		}
		out, err = q.GetVideo(ctx, id)
		return err
	})
	return out, err
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Cursors are opaque to callers: base64url of a small JSON object
// holding the ID of the last row of the previous page.
type cursorPayload struct {
	After string `json:"a"`
}

func encodeCursor(lastID string) string {
	b, _ := json.Marshal(cursorPayload{After: lastID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(c string) (string, error) {
	var p cursorPayload
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	if err != nil || p.After == "" {
		return "", app.E(app.CodeInvalidArgument, "cursor 无效").WithHint("请使用上一页返回的 next_cursor")
	}
	return p.After, nil
}
