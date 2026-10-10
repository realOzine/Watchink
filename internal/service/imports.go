package service

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"watchink/internal/app"
	"watchink/internal/config"
	"watchink/internal/jobs"
	"watchink/internal/media"
	"watchink/internal/pipeline"
	"watchink/internal/store"
)

// uploadTTL is how long a staged upload waits for its import.
const uploadTTL = 24 * time.Hour

// resolved is an import source after validation and normalization.
type resolved struct {
	sourceType, key, uri, title string
	opt                         pipeline.JobOptions
}

// Import implements app.Service: validate the request, turn the source into
// a dedup key, then — in one write transaction — either answer from what the
// library already has or create the video and its job.
func (s *Service) Import(ctx context.Context, req app.ImportRequest) (*app.ImportResponse, error) {
	if err := validateImport(req); err != nil {
		return nil, err
	}
	src, err := s.resolveSource(ctx, req.Source)
	if err != nil {
		return nil, err
	}
	src.opt.Language, src.opt.ReprocessFrom = req.Language, req.ReprocessFrom

	resp := &app.ImportResponse{Warnings: []string{}}
	var created string // job to announce after commit
	err = s.st.Write(ctx, func(tx *store.Tx) error {
		created = ""
		v, err := tx.FindVideoBySourceKey(ctx, src.key)
		if err != nil {
			return err
		}
		if v == nil {
			v = &store.Video{ID: app.NewID(app.PrefixVideo), SourceType: src.sourceType, SourceKey: src.key, SourceURI: src.uri, Title: src.title}
			if err := tx.CreateVideo(ctx, v); err != nil {
				return err
			}
		} else if answered, err := answerFromExisting(ctx, tx, v, req.ReprocessFrom, resp); err != nil || answered {
			return err
		}
		// The job keeps the source type of the stored video: a re-import
		// of the same bytes through another entry reuses the library copy.
		src.opt.SourceType = v.SourceType
		if src.sourceType != v.SourceType && req.ReprocessFrom == jobs.ReprocessAcquire {
			return app.E(app.CodeConflict, "该视频最初通过 %s 导入，请用相同方式重新获取", v.SourceType)
		}
		job, err := s.sched.NewJob(ctx, tx.Queries, jobs.JobParams{Kind: app.JobKindProcessVideo, VideoID: v.ID, RequestedBy: req.RequestedBy, Options: src.opt})
		if err != nil {
			return err
		}
		if src.opt.UploadID != "" {
			if _, err := tx.ConsumeUpload(ctx, src.opt.UploadID, v.ID); err != nil {
				return err
			}
		}
		created = job.ID
		resp.VideoID, resp.JobID, resp.Status, resp.Deduplicated = v.ID, job.ID, job.Status, false
		return nil
	})
	if err != nil {
		return nil, err
	}
	if created != "" {
		s.sched.Announce(ctx, created)
		s.publishVideo(resp.VideoID)
	}
	if !pipeline.LLMConfigured(s.Settings()) {
		// the import is accepted; the job fails fast at note_map.
		resp.Warnings = append(resp.Warnings, app.WarnModelNotConfigured)
	}
	return resp, nil
}

func validateImport(req app.ImportRequest) error {
	switch req.Language {
	case "", "auto", "zh", "en":
	default:
		return app.E(app.CodeInvalidArgument, "language 只能是 auto、zh 或 en")
	}
	switch req.ReprocessFrom {
	case "", jobs.ReprocessAcquire, jobs.ReprocessTranscribe, jobs.ReprocessNote:
	default:
		return app.E(app.CodeInvalidArgument, "reprocess_from 只能是 acquire、transcribe 或 note")
	}
	return nil
}

// answerFromExisting applies the repeat-import table to a video that
// is already in the library. It fills resp and reports true when no new job
// is needed: the video has an active job, or (without reprocess_from) it has
// been processed before, whatever the outcome.
func answerFromExisting(ctx context.Context, tx *store.Tx, v *store.Video, reprocessFrom string, resp *app.ImportResponse) (bool, error) {
	resp.VideoID = v.ID
	if v.CurrentNoteID != "" {
		id := v.CurrentNoteID
		resp.NoteID = &id
	}
	// At most one active job per video, checked inside the import
	// transaction.
	active, err := tx.ActiveJobOfVideo(ctx, v.ID)
	if err != nil {
		return false, err
	}
	if active != nil {
		resp.JobID, resp.Status, resp.Deduplicated = active.ID, active.Status, true
		return true, nil
	}
	if reprocessFrom != "" {
		return false, nil
	}
	latest, err := tx.LatestJobOfVideo(ctx, v.ID)
	if err != nil || latest == nil {
		return false, err
	}
	resp.JobID, resp.Status, resp.Deduplicated = latest.ID, latest.Status, true
	if latest.Status == app.JobSucceeded {
		// A finished video is not an error and not a new job.
		resp.Status = app.ImportStatusAlreadyProcessed
	}
	return true, nil
}

func (s *Service) resolveSource(ctx context.Context, in app.ImportSource) (*resolved, error) {
	switch in.Type {
	case app.SourceFile:
		if in.Path == "" {
			return nil, app.E(app.CodeInvalidArgument, "source.path 不能为空")
		}
		p, err := config.CheckImportPath(in.Path, s.Settings().Security.ImportRoots)
		if err != nil {
			return nil, err
		}
		key, _, err := media.SampleHash(p)
		if err != nil {
			return nil, app.E(app.CodeInvalidArgument, "无法读取文件：%v", err)
		}
		return &resolved{sourceType: app.SourceFile, key: key, uri: p, title: titleOf(p), opt: pipeline.JobOptions{Path: p}}, nil
	case app.SourceUpload:
		up, err := s.st.GetUpload(ctx, in.UploadID)
		if err != nil {
			return nil, err
		}
		if up.ConsumedBy != "" {
			return nil, app.E(app.CodeConflict, "该上传已被导入，请重新上传")
		}
		return &resolved{sourceType: app.SourceUpload, key: up.SampleHash, uri: up.Filename, title: titleOf(up.Filename), opt: pipeline.JobOptions{UploadID: up.ID}}, nil
	case app.SourceURL:
		ref, err := media.NormalizeURL(in.URL)
		if err != nil {
			return nil, err
		}
		if ref.NeedsResolve {
			// b23.tv short links are resolved synchronously.
			if ref, err = media.ResolveShortLink(ctx, pipeline.HTTPClient(s.Settings().Network.Proxy), in.URL); err != nil {
				return nil, err
			}
		}
		return &resolved{sourceType: ref.Type, key: ref.Key, uri: ref.CanonicalURL, title: ref.CanonicalURL, opt: pipeline.JobOptions{URL: ref.CanonicalURL}}, nil
	}
	return nil, app.E(app.CodeInvalidArgument, "source.type 只能是 file、url 或 upload")
}

func titleOf(name string) string {
	base := filepath.Base(name)
	if t := strings.TrimSuffix(base, filepath.Ext(base)); t != "" {
		return t
	}
	return base
}

// CreateUpload implements app.Service: the body is streamed to
// <data>/uploads/<id>.part, bounded by security.max_upload_size.
func (s *Service) CreateUpload(ctx context.Context, filename string, size int64, body io.Reader) (*app.Upload, error) {
	filename = filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if filename == "" || filename == "." || filename == "/" {
		return nil, app.E(app.CodeInvalidArgument, "缺少文件名（X-Filename）")
	}
	limit, err := config.ParseSize(s.Settings().Security.MaxUploadSize)
	if err != nil || limit <= 0 {
		limit = 8 << 30
	}
	if size > limit {
		return nil, app.E(app.CodeInvalidArgument, "文件超过上传大小上限 %s", s.Settings().Security.MaxUploadSize)
	}
	id := app.NewID(app.PrefixUpload)
	rel := s.st.UploadPath(id)
	abs := s.st.Abs(rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(f, io.LimitReader(&ctxReader{ctx: ctx, r: body}, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		err = media.WrapDiskFull(err, "")
	case n > limit:
		err = app.E(app.CodeInvalidArgument, "文件超过上传大小上限 %s", s.Settings().Security.MaxUploadSize)
	case n == 0:
		err = app.E(app.CodeInvalidArgument, "上传内容为空")
	}
	if err != nil {
		os.Remove(abs)
		return nil, err
	}
	key, _, err := media.SampleHash(abs)
	if err != nil {
		os.Remove(abs)
		return nil, err
	}
	up := &store.Upload{ID: id, Filename: filename, SizeBytes: n, SampleHash: key, Path: rel, ExpiresAt: app.FormatTime(time.Now().Add(uploadTTL))}
	if err := s.st.CreateUpload(ctx, up); err != nil {
		os.Remove(abs)
		return nil, err
	}
	dto := up.DTO()
	return &dto, nil
}

// ctxReader stops a streamed copy when the request is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
