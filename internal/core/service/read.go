package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"watchink/internal/app"
	"watchink/internal/capability/notes"
	"watchink/internal/core/pipeline"
	"watchink/internal/infra/store"
)

// mediaTokenTTL is the lifetime of a signed playback URL.
const mediaTokenTTL = 6 * time.Hour

func (s *Service) noteRow(ctx context.Context, videoID, noteID string) (*store.Note, error) {
	if _, err := s.st.GetVideo(ctx, videoID); err != nil {
		return nil, err
	}
	if noteID != "" {
		n, err := s.st.GetNote(ctx, noteID)
		if err != nil {
			return nil, err
		}
		if n.VideoID != videoID {
			return nil, app.E(app.CodeNotFound, "笔记 %s 不属于视频 %s", noteID, videoID)
		}
		return n, nil
	}
	n, err := s.st.CurrentNote(ctx, videoID)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, app.E(app.CodeNotFound, "该视频还没有笔记").WithHint("用 get_job 查看处理进度；任务失败时修复后重试")
	}
	return n, nil
}

func (s *Service) artifactBytes(ctx context.Context, id string) ([]byte, error) {
	a, err := s.st.GetArtifact(ctx, id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.st.Abs(a.Path))
	if err != nil {
		return nil, app.E(app.CodeInternal, "读取产物失败：%v", err)
	}
	return b, nil
}

// GetNote implements app.Service.
func (s *Service) GetNote(ctx context.Context, req app.GetNoteRequest) (*app.Note, error) {
	format := req.Format
	switch format {
	case "", "md", app.FormatMarkdown:
		format = app.FormatMarkdown
	case app.FormatJSON:
	default:
		return nil, app.E(app.CodeInvalidArgument, "format 只能是 markdown 或 json")
	}
	if req.Offset < 0 {
		return nil, app.E(app.CodeInvalidArgument, "offset 不能为负数")
	}
	n, err := s.noteRow(ctx, req.VideoID, req.NoteID)
	if err != nil {
		return nil, err
	}
	artID := n.MDArtifactID
	if format == app.FormatJSON {
		artID = n.JSONArtifactID
	}
	content, err := s.artifactBytes(ctx, artID)
	if err != nil {
		return nil, err
	}
	cites, err := s.st.ListCitations(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	if cites == nil {
		cites = []app.Citation{}
	}
	out := &app.Note{
		NoteID: n.ID, VideoID: n.VideoID, TranscriptID: n.TranscriptID, Version: n.Version, Title: n.Title, Format: format,
		Citations: cites, LLMProvider: n.LLMProvider, LLMModel: n.LLMModel, PromptVersion: n.PromptVersion, CreatedAt: n.CreatedAt,
	}
	_ = json.Unmarshal(n.Coverage, &out.Coverage)
	out.Content, out.TotalChars, out.NextOffset = notes.Paginate(string(content), req.Offset, req.MaxChars)
	return out, nil
}

// transcriptSegments returns the requested part of a transcript version in
// idx order: the listed segment IDs (IDs of other versions are not part of
// the answer), or the segments overlapping [start_ms, end_ms); both bounds
// are optional.
func (s *Service) transcriptSegments(ctx context.Context, transcriptID string, req app.GetTranscriptRequest) ([]app.Segment, error) {
	if len(req.SegmentIDs) > 0 {
		rows, err := s.st.SegmentsByIDs(ctx, req.SegmentIDs)
		return segmentDTOs(rows, transcriptID), err
	}
	rows, err := s.st.SegmentsInRange(ctx, transcriptID, req.StartMs, req.EndMs, 0)
	return segmentDTOs(rows, ""), err
}

// segmentDTOs converts rows, keeping only those of onlyTranscript when set.
func segmentDTOs(rows []store.Segment, onlyTranscript string) []app.Segment {
	out := make([]app.Segment, 0, len(rows))
	for _, r := range rows {
		if onlyTranscript == "" || r.TranscriptID == onlyTranscript {
			out = append(out, r.DTO())
		}
	}
	return out
}

// transcriptFormat validates a transcript request and returns its format.
func transcriptFormat(req app.GetTranscriptRequest) (string, error) {
	format := req.Format
	switch format {
	case "":
		format = app.FormatJSON
	case app.FormatJSON, app.FormatSRT, app.FormatVTT, app.FormatTXT:
	default:
		return "", app.E(app.CodeInvalidArgument, "format 只能是 json、srt、vtt 或 txt")
	}
	if len(req.SegmentIDs) > app.MaxSegmentIDs {
		return "", app.E(app.CodeInvalidArgument, "segment_ids 最多 %d 个", app.MaxSegmentIDs)
	}
	if len(req.SegmentIDs) > 0 && (req.StartMs != nil || req.EndMs != nil) {
		return "", app.E(app.CodeInvalidArgument, "segment_ids 不能与 start_ms / end_ms 同时使用")
	}
	if req.StartMs != nil && req.EndMs != nil && *req.EndMs <= *req.StartMs {
		return "", app.E(app.CodeInvalidArgument, "end_ms 必须大于 start_ms")
	}
	return format, nil
}

// GetTranscript implements app.Service.
func (s *Service) GetTranscript(ctx context.Context, req app.GetTranscriptRequest) (*app.Transcript, error) {
	format, err := transcriptFormat(req)
	if err != nil {
		return nil, err
	}
	v, err := s.st.GetVideo(ctx, req.VideoID)
	if err != nil {
		return nil, err
	}
	if v.CurrentTranscriptID == "" {
		return nil, app.E(app.CodeNotFound, "该视频还没有字幕").WithHint("用 get_job 查看处理进度")
	}
	tr, err := s.st.GetTranscript(ctx, v.CurrentTranscriptID)
	if err != nil {
		return nil, err
	}
	rows, err := s.transcriptSegments(ctx, tr.ID, req)
	if err != nil {
		return nil, err
	}
	out := &app.Transcript{
		TranscriptID: tr.ID, VideoID: tr.VideoID, Language: tr.Language, Engine: tr.Engine, Model: tr.Model,
		SegmentCount: tr.SegmentCount, Format: format, Segments: make([]app.Segment, 0, len(rows)),
	}
	limit := req.MaxChars
	if limit == 0 {
		limit = app.DefaultMaxChars
	}
	chars := 0
	for _, r := range rows {
		n := utf8.RuneCountInString(r.Text)
		// Always return at least one segment so paging makes progress.
		if limit > 0 && len(out.Segments) > 0 && chars+n > limit {
			out.Truncated = true
			next := r.StartMs
			out.NextStartMs = &next
			break
		}
		chars += n
		out.Segments = append(out.Segments, r)
	}
	switch format {
	case app.FormatSRT:
		out.Content = notes.SRT(out.Segments)
	case app.FormatVTT:
		out.Content = notes.VTT(out.Segments)
	case app.FormatTXT:
		out.Content = notes.TXT(out.Segments)
	}
	return out, nil
}

// --- playback ---

// mediaToken signs "<video_id>\n<expiry>" with the per-run key: the token
// grants read access to one video's playback file only.
func (s *Service) mediaToken(videoID string, exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, s.mediaKey)
	mac.Write([]byte(videoID + "\n" + e))
	return e + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Service) checkMediaToken(videoID, token string) bool {
	e, _, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	unix, err := strconv.ParseInt(e, 10, 64)
	if err != nil || time.Now().Unix() > unix {
		return false
	}
	want := s.mediaToken(videoID, time.Unix(unix, 0))
	return subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}

func playbackMime(a *store.Artifact) string {
	var m pipeline.PlaybackMeta
	_ = json.Unmarshal(a.Meta, &m)
	if m.Mime != "" {
		return m.Mime
	}
	return "video/mp4"
}

// GetPlayback implements app.Service.
func (s *Service) GetPlayback(ctx context.Context, videoID string) (*app.Playback, error) {
	if _, err := s.st.GetVideo(ctx, videoID); err != nil {
		return nil, err
	}
	a := s.playbackArtifact(ctx, videoID)
	if a == nil {
		return nil, app.E(app.CodeNotFound, "播放文件尚未就绪").WithHint("playback 阶段完成后再试")
	}
	exp := time.Now().Add(mediaTokenTTL)
	return &app.Playback{
		URL:  s.base() + "/media/" + videoID + "/playback?mt=" + s.mediaToken(videoID, exp),
		Mime: playbackMime(a), ExpiresAt: app.FormatTime(exp),
	}, nil
}

// OpenPlayback implements app.Local.
func (s *Service) OpenPlayback(ctx context.Context, videoID, mediaToken string) (*app.PlaybackFile, error) {
	if !s.checkMediaToken(videoID, mediaToken) {
		return nil, app.E(app.CodeUnauthorized, "播放链接无效或已过期")
	}
	a := s.playbackArtifact(ctx, videoID)
	if a == nil {
		return nil, app.E(app.CodeNotFound, "播放文件尚未就绪")
	}
	f, err := os.Open(s.st.Abs(a.Path))
	if err != nil {
		return nil, app.E(app.CodeNotFound, "播放文件不存在")
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &app.PlaybackFile{Content: f, Name: filepath.Base(a.Path), Mime: playbackMime(a), ModTime: fi.ModTime()}, nil
}

// --- export ---

// CreateExport implements app.Service.
func (s *Service) CreateExport(ctx context.Context, req app.ExportRequest) (*app.Export, error) {
	v, err := s.st.GetVideo(ctx, req.VideoID)
	if err != nil {
		return nil, err
	}
	var content []byte
	noteID := ""
	switch req.Format {
	case app.FormatMD, app.FormatMDRaw, app.FormatJSON:
		n, err := s.noteRow(ctx, v.ID, "")
		if err != nil {
			return nil, err
		}
		noteID = n.ID
		artID := n.MDArtifactID
		if req.Format == app.FormatJSON {
			artID = n.JSONArtifactID
		}
		if content, err = s.artifactBytes(ctx, artID); err != nil {
			return nil, err
		}
		if req.Format == app.FormatMD {
			cites, err := s.st.ListCitations(ctx, n.ID)
			if err != nil {
				return nil, err
			}
			content = []byte(notes.ExportMarkdown(string(content), pipeline.VideoSource(v), cites))
		}
	case app.FormatSRT, app.FormatVTT, app.FormatTXT:
		tr, err := s.GetTranscript(ctx, app.GetTranscriptRequest{VideoID: v.ID, Format: req.Format, MaxChars: -1})
		if err != nil {
			return nil, err
		}
		content = []byte(tr.Content)
	default:
		return nil, app.E(app.CodeInvalidArgument, "format 只能是 md、md-raw、json、srt、vtt 或 txt")
	}
	id := app.NewID(app.PrefixExport)
	filename := notes.ExportFilename(v.Title, v.ID, req.Format)
	// Each export gets its own directory so the user-facing file name
	// stays clean and repeated exports never overwrite each other.
	rel := path.Join(s.st.ExportDir(v.ID), id, filename)
	fi, err := s.st.WriteFileAtomic(rel, content)
	if err != nil {
		return nil, err
	}
	row := &store.Export{ID: id, VideoID: v.ID, NoteID: noteID, Format: req.Format, Path: rel, SHA256: fi.SHA256}
	if err := s.st.CreateExport(ctx, row); err != nil {
		return nil, err
	}
	return s.exportDTO(row, fi.Size), nil
}

func (s *Service) exportDTO(e *store.Export, size int64) *app.Export {
	return &app.Export{
		ExportID: e.ID, VideoID: e.VideoID, Format: e.Format, Path: s.st.Abs(e.Path), Filename: path.Base(e.Path),
		Bytes: size, SHA256: e.SHA256, DownloadURL: s.base() + app.APIPrefix + "/exports/" + e.ID + "/download", CreatedAt: e.CreatedAt,
	}
}

// OpenExport implements app.Service.
func (s *Service) OpenExport(ctx context.Context, exportID string) (io.ReadCloser, *app.Export, error) {
	e, err := s.st.GetExport(ctx, exportID)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(s.st.Abs(e.Path))
	if err != nil {
		return nil, nil, app.E(app.CodeNotFound, "导出文件已不存在，请重新导出")
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, s.exportDTO(e, fi.Size()), nil
}
