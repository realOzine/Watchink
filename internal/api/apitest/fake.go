// Package apitest provides Fake, an in-memory app.Local for transport tests
// (internal/api, internal/client, internal/engine) and for any entry point
// that wants an engine without a store or pipeline.
package apitest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"sync"
	"time"

	"watchink/internal/app"
)

// Call is one recorded Service invocation.
type Call struct {
	Method string
	Args   []any
}

// Fake is a small but behaving app.Local: imports create videos and jobs,
// jobs long-poll on rev, exports and uploads keep their bytes, and events go
// through a bounded replay buffer like the real engine's (§13.3).
//
// Seed data through the exported fields before use, or with the helper
// methods at any time. All methods are safe for concurrent use.
type Fake struct {
	mu sync.Mutex

	// Info is returned by EngineInfo (ActiveJobs is filled from Active).
	Info app.EngineInfo
	// MediaToken is the only token OpenPlayback accepts.
	MediaToken string
	// PlaybackData holds the playback bytes per video ID.
	PlaybackData map[string][]byte
	// PlaybackModTime is the Last-Modified of every playback file.
	PlaybackModTime time.Time
	// Notes and Transcripts are keyed by video ID.
	Notes       map[string]*app.Note
	Transcripts map[string]*app.Transcript
	// Settings, Tools and LLMTest are returned as they are.
	Settings app.Settings
	Tools    app.ToolsStatus
	LLMTest  app.LLMTestResult
	// WebOn is what WebEnabled reports (the optional browser UI).
	WebOn bool
	// LLMModels is returned by ListLLMModels.
	LLMModels []string
	// ReplayCap is the event replay buffer size; zero means 1000.
	ReplayCap int
	// Active overrides ActiveJobs when >= 0; -1 counts the stored jobs.
	Active int

	videos   map[string]*app.Video
	videoIDs []string
	jobs     map[string]*app.Job
	jobIDs   []string
	uploads  map[string][]byte
	exports  map[string]*fakeExport
	secrets  map[string]string
	errs     map[string]error
	calls    []Call

	jobChanged chan struct{}

	events   []app.Event
	nextID   int64
	subs     map[*subscriber]struct{}
	stopped  bool
	stopKind []bool
}

type fakeExport struct {
	meta app.Export
	data []byte
}

type subscriber struct {
	ch chan app.Event
}

var _ app.Local = (*Fake)(nil)

// New returns an empty Fake with default settings.
func New() *Fake {
	return &Fake{
		Info:            app.EngineInfo{DataDir: "/fake/data", ToolsReady: true},
		MediaToken:      "media-token",
		PlaybackData:    map[string][]byte{},
		PlaybackModTime: time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC),
		Notes:           map[string]*app.Note{},
		Transcripts:     map[string]*app.Transcript{},
		Settings:        app.DefaultSettings(),
		Tools:           app.ToolsStatus{Items: []app.ToolItem{}, Ready: true},
		LLMTest:         app.LLMTestResult{OK: true, StructuredOutput: "json_schema", Model: "fake", LatencyMs: 1},
		Active:          -1,
		videos:          map[string]*app.Video{},
		jobs:            map[string]*app.Job{},
		uploads:         map[string][]byte{},
		exports:         map[string]*fakeExport{},
		secrets:         map[string]string{},
		errs:            map[string]error{},
		jobChanged:      make(chan struct{}),
		subs:            map[*subscriber]struct{}{},
	}
}

// SetError makes the named Service method (e.g. "GetVideo") fail with err;
// a nil err removes the override.
func (f *Fake) SetError(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.errs, method)
		return
	}
	f.errs[method] = err
}

// Calls returns a copy of the recorded calls.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// CallCount counts the recorded calls of one method.
func (f *Fake) CallCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// LastCall returns the most recent call of a method.
func (f *Fake) LastCall(method string) (Call, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Method == method {
			return f.calls[i], true
		}
	}
	return Call{}, false
}

// enter records the call and returns the injected error, if any. The caller
// must hold f.mu.
func (f *Fake) enter(method string, args ...any) error {
	f.calls = append(f.calls, Call{Method: method, Args: args})
	return f.errs[method]
}

// AddVideo stores a video (assigning an ID when empty) and returns a copy.
func (f *Fake) AddVideo(v app.Video) app.Video {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.addVideoLocked(v)
}

func (f *Fake) addVideoLocked(v app.Video) *app.Video {
	if v.VideoID == "" {
		v.VideoID = app.NewID(app.PrefixVideo)
	}
	if v.CreatedAt == "" {
		v.CreatedAt = app.Now()
	}
	if v.UpdatedAt == "" {
		v.UpdatedAt = v.CreatedAt
	}
	if _, ok := f.videos[v.VideoID]; !ok {
		f.videoIDs = append(f.videoIDs, v.VideoID)
	}
	f.videos[v.VideoID] = &v
	return &v
}

// AddJob stores a job (assigning an ID and rev 1 when empty) and returns a copy.
func (f *Fake) AddJob(j app.Job) app.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.addJobLocked(j)
}

func (f *Fake) addJobLocked(j app.Job) *app.Job {
	if j.JobID == "" {
		j.JobID = app.NewID(app.PrefixJob)
	}
	if j.Kind == "" {
		j.Kind = app.JobKindProcessVideo
	}
	if j.Status == "" {
		j.Status = app.JobQueued
	}
	if j.Rev == 0 {
		j.Rev = 1
	}
	if j.Attempt == 0 {
		j.Attempt = 1
	}
	if j.Stages == nil {
		j.Stages = []app.JobStage{}
	}
	if j.CreatedAt == "" {
		j.CreatedAt = app.Now()
	}
	if j.UpdatedAt == "" {
		j.UpdatedAt = j.CreatedAt
	}
	if _, ok := f.jobs[j.JobID]; !ok {
		f.jobIDs = append(f.jobIDs, j.JobID)
	}
	f.jobs[j.JobID] = &j
	return &j
}

// UpdateJob mutates a job, bumps its rev, wakes long polls and emits
// job.updated. It reports whether the job exists.
func (f *Fake) UpdateJob(jobID string, mutate func(j *app.Job)) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[jobID]
	if !ok {
		return false
	}
	f.touchJobLocked(j, mutate)
	return true
}

func (f *Fake) touchJobLocked(j *app.Job, mutate func(j *app.Job)) {
	if mutate != nil {
		mutate(j)
	}
	j.Rev++
	j.UpdatedAt = app.Now()
	close(f.jobChanged)
	f.jobChanged = make(chan struct{})
	cp := *j
	f.emitLocked(app.Event{Type: app.EventJobUpdated, Job: &cp})
}

// AddExport registers an export with its bytes and returns the metadata.
func (f *Fake) AddExport(videoID, format, filename string, data []byte) app.Export {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addExportLocked(videoID, format, filename, data).meta
}

func (f *Fake) addExportLocked(videoID, format, filename string, data []byte) *fakeExport {
	sum := sha256.Sum256(data)
	id := app.NewID(app.PrefixExport)
	e := &fakeExport{data: data, meta: app.Export{
		ExportID:    id,
		VideoID:     videoID,
		Format:      format,
		Path:        "/fake/data/exports/" + videoID + "/" + filename,
		Filename:    filename,
		Bytes:       int64(len(data)),
		SHA256:      hex.EncodeToString(sum[:]),
		DownloadURL: app.APIPrefix + "/exports/" + id + "/download",
		CreatedAt:   app.Now(),
	}}
	f.exports[id] = e
	return e
}

// UploadData returns the bytes stored for an upload.
func (f *Fake) UploadData(uploadID string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.uploads[uploadID]
	return b, ok
}

// Secret returns a stored secret value.
func (f *Fake) Secret(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.secrets[name]
	return v, ok
}

// Emit publishes an event (assigning the next ID) and returns its ID.
func (f *Fake) Emit(ev app.Event) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.emitLocked(ev)
}

func (f *Fake) emitLocked(ev app.Event) int64 {
	f.nextID++
	ev.ID = f.nextID
	limit := f.ReplayCap
	if limit <= 0 {
		limit = 1000
	}
	f.events = append(f.events, ev)
	if len(f.events) > limit {
		f.events = append([]app.Event(nil), f.events[len(f.events)-limit:]...)
	}
	for s := range f.subs {
		select {
		case s.ch <- ev:
		default:
			// A subscriber that cannot keep up is dropped; it reconnects and
			// resumes (or resets) by Last-Event-ID.
			delete(f.subs, s)
			close(s.ch)
		}
	}
	return ev.ID
}

// SubscriberCount is the number of live Subscribe streams.
func (f *Fake) SubscriberCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// Stopped reports whether Stop was called, and with which force values.
func (f *Fake) Stopped() (stopped bool, force []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped, append([]bool(nil), f.stopKind...)
}

// SetActive overrides the ActiveJobs count (-1 = count stored jobs).
func (f *Fake) SetActive(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Active = n
}

func (f *Fake) activeLocked() int {
	if f.Active >= 0 {
		return f.Active
	}
	n := 0
	for _, j := range f.jobs {
		switch j.Status {
		case app.JobQueued, app.JobRunning, app.JobCancelling:
			n++
		}
	}
	return n
}

func notFound(kind, id string) *app.Error {
	return app.E(app.CodeNotFound, "%s不存在：%s", kind, id)
}

// EngineInfo implements app.Service.
func (f *Fake) EngineInfo(context.Context) (*app.EngineInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("EngineInfo"); err != nil {
		return nil, err
	}
	info := f.Info
	info.ActiveJobs = f.activeLocked()
	return &info, nil
}

// Import implements app.Service: it creates a video and a queued job, or
// reports already_processed for a file path imported before.
func (f *Fake) Import(_ context.Context, req app.ImportRequest) (*app.ImportResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("Import", req); err != nil {
		return nil, err
	}
	var uri, typ string
	switch req.Source.Type {
	case app.SourceFile:
		if !strings.HasPrefix(req.Source.Path, "/") {
			return nil, app.E(app.CodeInvalidArgument, "path 必须是绝对路径")
		}
		uri, typ = req.Source.Path, app.SourceFile
	case app.SourceURL:
		if req.Source.URL == "" {
			return nil, app.E(app.CodeInvalidArgument, "缺少 url")
		}
		uri, typ = req.Source.URL, app.SourceYouTube
	case app.SourceUpload:
		if _, ok := f.uploads[req.Source.UploadID]; !ok {
			return nil, notFound("上传", req.Source.UploadID)
		}
		uri, typ = req.Source.UploadID, app.SourceUpload
	default:
		return nil, app.E(app.CodeInvalidArgument, "不支持的 source.type：%q", req.Source.Type)
	}
	for _, id := range f.videoIDs {
		v := f.videos[id]
		if v.Source.URI == uri && v.Status == app.VideoReady && req.ReprocessFrom == "" {
			return &app.ImportResponse{
				VideoID: v.VideoID, JobID: deref(v.LatestJobID),
				Status: app.ImportStatusAlreadyProcessed, Deduplicated: true,
				NoteID: v.NoteID, Warnings: []string{},
			}, nil
		}
	}
	v := f.addVideoLocked(app.Video{
		Title: uri, Status: app.VideoProcessing,
		Source: app.VideoSource{Type: typ, URI: uri},
	})
	vid := v.VideoID
	j := f.addJobLocked(app.Job{VideoID: &vid, RequestedBy: req.RequestedBy, Stage: app.StageEnsureTools})
	jid := j.JobID
	v.LatestJobID = &jid
	return &app.ImportResponse{VideoID: vid, JobID: jid, Status: j.Status, Warnings: []string{}}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// CreateUpload implements app.Service; it reads the stream to the end.
func (f *Fake) CreateUpload(_ context.Context, filename string, size int64, body io.Reader) (*app.Upload, error) {
	f.mu.Lock()
	err := f.enter("CreateUpload", filename, size)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, app.E(app.CodeInternal, "读取上传内容失败").WithCause(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := app.NewID(app.PrefixUpload)
	f.uploads[id] = data
	return &app.Upload{
		UploadID: id, Filename: filename, SizeBytes: int64(len(data)),
		ExpiresAt: app.FormatTime(time.Now().Add(24 * time.Hour)),
	}, nil
}

// page slices ids newest-first by an opaque (here: index) cursor.
func page(ids []string, cursor string, limit int) (out []string, next *string, err error) {
	rev := make([]string, len(ids))
	for i, id := range ids {
		rev[len(ids)-1-i] = id
	}
	start := 0
	if cursor != "" {
		found := false
		for i, id := range rev {
			if id == cursor {
				start, found = i+1, true
				break
			}
		}
		if !found {
			return nil, nil, app.E(app.CodeInvalidArgument, "cursor 无效")
		}
	}
	if limit <= 0 {
		limit = app.DefaultLimit
	}
	end := min(start+limit, len(rev))
	out = rev[start:end]
	if end < len(rev) && len(out) > 0 {
		c := out[len(out)-1]
		next = &c
	}
	return out, next, nil
}

// ListJobs implements app.Service.
func (f *Fake) ListJobs(_ context.Context, req app.ListJobsRequest) (*app.JobList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("ListJobs", req); err != nil {
		return nil, err
	}
	var ids []string
	for _, id := range f.jobIDs {
		j := f.jobs[id]
		if req.Status != "" && j.Status != req.Status {
			continue
		}
		if req.VideoID != "" && deref(j.VideoID) != req.VideoID {
			continue
		}
		ids = append(ids, id)
	}
	sel, next, err := page(ids, req.Cursor, req.Limit)
	if err != nil {
		return nil, err
	}
	res := &app.JobList{Jobs: []app.Job{}, NextCursor: next}
	for _, id := range sel {
		res.Jobs = append(res.Jobs, *f.jobs[id])
	}
	return res, nil
}

// GetJob implements app.Service, including the long poll on rev.
func (f *Fake) GetJob(ctx context.Context, req app.GetJobRequest) (*app.Job, error) {
	f.mu.Lock()
	if err := f.enter("GetJob", req); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	var timeout <-chan time.Time
	if req.WaitSeconds > 0 {
		t := time.NewTimer(time.Duration(req.WaitSeconds) * time.Second)
		defer t.Stop()
		timeout = t.C
	}
	for {
		j, ok := f.jobs[req.JobID]
		if !ok {
			f.mu.Unlock()
			return nil, notFound("任务", req.JobID)
		}
		if req.WaitSeconds <= 0 || j.Rev > req.AfterRev {
			cp := *j
			f.mu.Unlock()
			return &cp, nil
		}
		changed := f.jobChanged
		f.mu.Unlock()
		select {
		case <-changed:
		case <-timeout:
			req.WaitSeconds = 0
		case <-ctx.Done():
			req.WaitSeconds = 0
		}
		f.mu.Lock()
	}
}

// CancelJob implements app.Service.
func (f *Fake) CancelJob(_ context.Context, jobID string) (*app.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("CancelJob", jobID); err != nil {
		return nil, err
	}
	j, ok := f.jobs[jobID]
	if !ok {
		return nil, notFound("任务", jobID)
	}
	if j.Terminal() {
		return nil, app.E(app.CodeConflict, "任务已结束，无法取消")
	}
	f.touchJobLocked(j, func(j *app.Job) { j.Status = app.JobCancelled; j.Stage = "" })
	cp := *j
	return &cp, nil
}

// RetryJob implements app.Service.
func (f *Fake) RetryJob(_ context.Context, jobID string) (*app.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("RetryJob", jobID); err != nil {
		return nil, err
	}
	j, ok := f.jobs[jobID]
	if !ok {
		return nil, notFound("任务", jobID)
	}
	if !j.Terminal() || j.Status == app.JobSucceeded {
		return nil, app.E(app.CodeConflict, "只有失败、取消或中断的任务可以重试")
	}
	f.touchJobLocked(j, func(j *app.Job) { j.Status = app.JobQueued; j.Attempt++; j.Error = nil })
	cp := *j
	return &cp, nil
}

// ListVideos implements app.Service.
func (f *Fake) ListVideos(_ context.Context, req app.ListVideosRequest) (*app.VideoList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("ListVideos", req); err != nil {
		return nil, err
	}
	var ids []string
	for _, id := range f.videoIDs {
		if req.TitleContains != "" && !strings.Contains(f.videos[id].Title, req.TitleContains) {
			continue
		}
		ids = append(ids, id)
	}
	sel, next, err := page(ids, req.Cursor, req.Limit)
	if err != nil {
		return nil, err
	}
	res := &app.VideoList{Videos: []app.Video{}, NextCursor: next}
	for _, id := range sel {
		res.Videos = append(res.Videos, *f.videos[id])
	}
	return res, nil
}

// GetVideo implements app.Service.
func (f *Fake) GetVideo(_ context.Context, videoID string) (*app.Video, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("GetVideo", videoID); err != nil {
		return nil, err
	}
	v, ok := f.videos[videoID]
	if !ok {
		return nil, notFound("视频", videoID)
	}
	cp := *v
	return &cp, nil
}

// GetNote implements app.Service with offset / max_chars paging in runes.
func (f *Fake) GetNote(_ context.Context, req app.GetNoteRequest) (*app.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("GetNote", req); err != nil {
		return nil, err
	}
	n, ok := f.Notes[req.VideoID]
	if !ok {
		return nil, notFound("笔记", req.VideoID)
	}
	cp := *n
	if req.Format != "" {
		cp.Format = req.Format
	}
	if cp.Format == "" {
		cp.Format = app.FormatMarkdown
	}
	runes := []rune(cp.Content)
	cp.TotalChars = len(runes)
	limit := req.MaxChars
	if limit <= 0 {
		limit = app.DefaultMaxChars
	}
	start := min(req.Offset, len(runes))
	end := min(start+limit, len(runes))
	cp.Content = string(runes[start:end])
	cp.NextOffset = nil
	if end < len(runes) {
		cp.NextOffset = &end
	}
	if cp.Citations == nil {
		cp.Citations = []app.Citation{}
	}
	return &cp, nil
}

// GetTranscript implements app.Service: it filters by IDs or time range and
// renders a simple text Content for the non-JSON formats.
func (f *Fake) GetTranscript(_ context.Context, req app.GetTranscriptRequest) (*app.Transcript, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("GetTranscript", req); err != nil {
		return nil, err
	}
	t, ok := f.Transcripts[req.VideoID]
	if !ok {
		return nil, notFound("字幕", req.VideoID)
	}
	cp := *t
	cp.Format = req.Format
	if cp.Format == "" {
		cp.Format = app.FormatJSON
	}
	want := map[string]bool{}
	for _, id := range req.SegmentIDs {
		want[id] = true
	}
	cp.Segments = []app.Segment{}
	for _, s := range t.Segments {
		switch {
		case len(want) > 0 && !want[s.SegmentID]:
			continue
		case req.StartMs != nil && s.EndMs <= *req.StartMs:
			continue
		case req.EndMs != nil && s.StartMs >= *req.EndMs:
			continue
		}
		cp.Segments = append(cp.Segments, s)
	}
	if cp.Format != app.FormatJSON {
		var b strings.Builder
		for _, s := range cp.Segments {
			b.WriteString(s.Text)
			b.WriteByte('\n')
		}
		cp.Content = strings.ToUpper(cp.Format) + "\n" + b.String()
	}
	return &cp, nil
}

// GetPlayback implements app.Service.
func (f *Fake) GetPlayback(_ context.Context, videoID string) (*app.Playback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("GetPlayback", videoID); err != nil {
		return nil, err
	}
	if _, ok := f.PlaybackData[videoID]; !ok {
		return nil, notFound("播放文件", videoID)
	}
	return &app.Playback{
		URL:       "/media/" + videoID + "/playback?mt=" + f.MediaToken,
		Mime:      "video/mp4",
		ExpiresAt: app.FormatTime(time.Now().Add(6 * time.Hour)),
	}, nil
}

// CreateExport implements app.Service.
func (f *Fake) CreateExport(_ context.Context, req app.ExportRequest) (*app.Export, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("CreateExport", req); err != nil {
		return nil, err
	}
	v, ok := f.videos[req.VideoID]
	if !ok {
		return nil, notFound("视频", req.VideoID)
	}
	switch req.Format {
	case app.FormatMD, app.FormatMDRaw, app.FormatJSON, app.FormatSRT, app.FormatVTT, app.FormatTXT:
	default:
		return nil, app.E(app.CodeInvalidArgument, "不支持的导出格式：%q", req.Format)
	}
	ext := strings.TrimSuffix(req.Format, "-raw")
	data := []byte("export of " + v.VideoID + " as " + req.Format + "\n")
	e := f.addExportLocked(v.VideoID, req.Format, v.Title+"."+ext, data)
	meta := e.meta
	return &meta, nil
}

// OpenExport implements app.Service.
func (f *Fake) OpenExport(_ context.Context, exportID string) (io.ReadCloser, *app.Export, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("OpenExport", exportID); err != nil {
		return nil, nil, err
	}
	e, ok := f.exports[exportID]
	if !ok {
		return nil, nil, notFound("导出", exportID)
	}
	meta := e.meta
	return io.NopCloser(bytes.NewReader(e.data)), &meta, nil
}

// GetSettings implements app.Service.
func (f *Fake) GetSettings(context.Context) (*app.Settings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("GetSettings"); err != nil {
		return nil, err
	}
	s := f.Settings
	return &s, nil
}

// PatchSettings implements app.Service for the keys the tests use
// (llm.model, llm.base_url, asr.language); anything else is INVALID_ARGUMENT.
func (f *Fake) PatchSettings(_ context.Context, patch map[string]any) (*app.Settings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("PatchSettings", patch); err != nil {
		return nil, err
	}
	for section, raw := range patch {
		fields, ok := raw.(map[string]any)
		if !ok {
			return nil, app.E(app.CodeInvalidArgument, "未知的配置项：%s", section)
		}
		for key, val := range fields {
			s, _ := val.(string)
			switch section + "." + key {
			case "llm.model":
				f.Settings.LLM.Model = s
			case "llm.base_url":
				f.Settings.LLM.BaseURL = s
			case "asr.language":
				f.Settings.ASR.Language = s
			default:
				return nil, app.E(app.CodeInvalidArgument, "未知的配置项：%s.%s", section, key)
			}
		}
	}
	s := f.Settings
	return &s, nil
}

// SetSecret implements app.Service; an empty value deletes the secret.
func (f *Fake) SetSecret(_ context.Context, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The value is deliberately not recorded in Calls.
	if err := f.enter("SetSecret", name); err != nil {
		return err
	}
	if name != app.SecretLLMAPIKey {
		return app.E(app.CodeInvalidArgument, "未知的凭据：%s", name)
	}
	if value == "" {
		delete(f.secrets, name)
		f.Settings.LLM.APIKeyConfigured = false
		return nil
	}
	f.secrets[name] = value
	f.Settings.LLM.APIKeyConfigured = true
	return nil
}

// WebEnabled implements app.Local.
func (f *Fake) WebEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.WebOn
}

// ListLLMModels implements app.Service.
func (f *Fake) ListLLMModels(_ context.Context, req app.LLMModelsRequest) (*app.LLMModelList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("ListLLMModels", req.BaseURL); err != nil {
		return nil, err
	}
	return &app.LLMModelList{BaseURL: req.BaseURL, Models: append([]string{}, f.LLMModels...)}, nil
}

// TestLLM implements app.Service.
func (f *Fake) TestLLM(context.Context) (*app.LLMTestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("TestLLM"); err != nil {
		return nil, err
	}
	r := f.LLMTest
	return &r, nil
}

func (f *Fake) toolsLocked() *app.ToolsStatus {
	t := f.Tools
	t.Items = append([]app.ToolItem{}, f.Tools.Items...)
	return &t
}

// GetTools implements app.Service.
func (f *Fake) GetTools(context.Context) (*app.ToolsStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("GetTools"); err != nil {
		return nil, err
	}
	return f.toolsLocked(), nil
}

// PrepareTools implements app.Service: it returns the active prepare_tools
// job or creates one.
func (f *Fake) PrepareTools(_ context.Context, req app.PrepareToolsRequest) (*app.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("PrepareTools", req); err != nil {
		return nil, err
	}
	for _, id := range f.jobIDs {
		if j := f.jobs[id]; j.Kind == app.JobKindPrepareTools && !j.Terminal() {
			cp := *j
			return &cp, nil
		}
	}
	j := f.addJobLocked(app.Job{Kind: app.JobKindPrepareTools, Stage: app.StagePrepare})
	cp := *j
	return &cp, nil
}

// UpdateTool implements app.Service.
func (f *Fake) UpdateTool(_ context.Context, name string) (*app.ToolsStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("UpdateTool", name); err != nil {
		return nil, err
	}
	for i := range f.Tools.Items {
		if f.Tools.Items[i].Name == name {
			f.Tools.Items[i].Version = "updated"
			f.Tools.Items[i].Status = app.ToolReady
			return f.toolsLocked(), nil
		}
	}
	return nil, notFound("工具", name)
}

// ImportTool implements app.Service.
func (f *Fake) ImportTool(_ context.Context, req app.ToolImportRequest) (*app.ToolsStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("ImportTool", req); err != nil {
		return nil, err
	}
	for i := range f.Tools.Items {
		if f.Tools.Items[i].Name == req.Name {
			f.Tools.Items[i].Path = req.Path
			f.Tools.Items[i].Status = app.ToolReady
			return f.toolsLocked(), nil
		}
	}
	return nil, notFound("工具", req.Name)
}

// Subscribe implements app.Service with the replay semantics of §13.3:
// events after afterID are replayed when still buffered; otherwise (or when
// afterID is ahead of this run) reset is true and only new events follow.
func (f *Fake) Subscribe(ctx context.Context, afterID int64) (<-chan app.Event, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("Subscribe", afterID); err != nil {
		return nil, false, err
	}
	var replay []app.Event
	reset := false
	switch {
	case afterID > f.nextID:
		reset = true
	case afterID < f.nextID:
		oldest := f.nextID + 1
		if len(f.events) > 0 {
			oldest = f.events[0].ID
		}
		if afterID+1 < oldest {
			reset = true
		} else {
			for _, ev := range f.events {
				if ev.ID > afterID {
					replay = append(replay, ev)
				}
			}
		}
	}
	s := &subscriber{ch: make(chan app.Event, len(replay)+256)}
	for _, ev := range replay {
		s.ch <- ev
	}
	if f.stopped {
		close(s.ch)
		return s.ch, reset, nil
	}
	f.subs[s] = struct{}{}
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.subs[s]; ok {
			delete(f.subs, s)
			close(s.ch)
		}
	}()
	return s.ch, reset, nil
}

// OpenPlayback implements app.Local.
func (f *Fake) OpenPlayback(_ context.Context, videoID, mediaToken string) (*app.PlaybackFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The token is a credential and is not recorded in Calls.
	if err := f.enter("OpenPlayback", videoID); err != nil {
		return nil, err
	}
	if mediaToken != f.MediaToken {
		return nil, app.E(app.CodeUnauthorized, "媒体令牌无效或已过期")
	}
	data, ok := f.PlaybackData[videoID]
	if !ok {
		return nil, notFound("播放文件", videoID)
	}
	return &app.PlaybackFile{
		Content: nopSeekCloser{bytes.NewReader(data)},
		Name:    "playback.mp4",
		Mime:    "video/mp4",
		ModTime: f.PlaybackModTime,
	}, nil
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// ActiveJobs implements app.Local.
func (f *Fake) ActiveJobs(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ActiveJobs"]; err != nil {
		return 0, err
	}
	return f.activeLocked(), nil
}

// Stop implements app.Local: it emits engine.stopping and ends every stream.
func (f *Fake) Stop(_ context.Context, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("Stop", force); err != nil {
		return err
	}
	f.stopKind = append(f.stopKind, force)
	if f.stopped {
		return nil
	}
	f.emitLocked(app.Event{Type: app.EventEngineStopping, Reason: "shutdown"})
	f.stopped = true
	for s := range f.subs {
		delete(f.subs, s)
		close(s.ch)
	}
	return nil
}
