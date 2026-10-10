// Package clitest provides a scriptable in-memory app.Remote for tests of
// the entry points. It has no behaviour of its own: every method returns the
// canned value of the matching field, or calls the matching hook.
package clitest

import (
	"bytes"
	"context"
	"io"
	"sync"

	"watchink/internal/app"
)

// Remote is a fake app.Remote. Set a *Func hook to script a method; without
// one the method returns the canned value (Err first, when set).
type Remote struct {
	mu sync.Mutex
	// Calls lists the methods invoked, in order.
	Calls []string
	// Closed counts Close calls.
	Closed int

	// Err, when non-nil, is returned by every method without a hook.
	Err error

	Engine   *app.EngineInfo
	Settings *app.Settings
	Tools    *app.ToolsStatus
	Jobs     map[string]*app.Job
	Videos   []app.Video
	Note     *app.Note
	Tr       *app.Transcript
	Export   *app.Export
	// ExportBody is the content served by OpenExport.
	ExportBody []byte
	LLMTest    *app.LLMTestResult
	URL        string

	// Recorded inputs of the last matching call.
	LastImport     app.ImportRequest
	LastPatch      map[string]any
	LastSecretName string
	LastSecret     string
	SecretSet      bool
	LastListJobs   app.ListJobsRequest
	LastListVideos app.ListVideosRequest
	LastNoteReq    app.GetNoteRequest
	LastTrReq      app.GetTranscriptRequest
	LastExportReq  app.ExportRequest
	LastPrepare    app.PrepareToolsRequest
	LastToolImport app.ToolImportRequest
	LastUpdateTool string
	LastForce      bool
	ShutdownCalled bool
	GetJobReqs     []app.GetJobRequest

	ImportFunc        func(app.ImportRequest) (*app.ImportResponse, error)
	GetJobFunc        func(app.GetJobRequest) (*app.Job, error)
	CancelJobFunc     func(string) (*app.Job, error)
	RetryJobFunc      func(string) (*app.Job, error)
	GetNoteFunc       func(app.GetNoteRequest) (*app.Note, error)
	GetTranscriptFunc func(app.GetTranscriptRequest) (*app.Transcript, error)
	PrepareFunc       func(app.PrepareToolsRequest) (*app.Job, error)
}

var _ app.Remote = (*Remote)(nil)

func (r *Remote) record(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls = append(r.Calls, name)
	return r.Err
}

// Called reports how many times method was invoked.
func (r *Remote) Called(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.Calls {
		if c == method {
			n++
		}
	}
	return n
}

func notFound(what string) error { return app.E(app.CodeNotFound, "%s 不存在", what) }

func (r *Remote) EngineInfo(context.Context) (*app.EngineInfo, error) {
	if err := r.record("EngineInfo"); err != nil {
		return nil, err
	}
	if r.Engine == nil {
		return &app.EngineInfo{}, nil
	}
	return r.Engine, nil
}

func (r *Remote) Import(_ context.Context, req app.ImportRequest) (*app.ImportResponse, error) {
	err := r.record("Import")
	r.LastImport = req
	if r.ImportFunc != nil {
		return r.ImportFunc(req)
	}
	if err != nil {
		return nil, err
	}
	return &app.ImportResponse{VideoID: "vid_1", JobID: "job_1", Status: app.JobQueued}, nil
}

func (r *Remote) CreateUpload(context.Context, string, int64, io.Reader) (*app.Upload, error) {
	if err := r.record("CreateUpload"); err != nil {
		return nil, err
	}
	return &app.Upload{UploadID: "upl_1"}, nil
}

func (r *Remote) ListJobs(_ context.Context, req app.ListJobsRequest) (*app.JobList, error) {
	if err := r.record("ListJobs"); err != nil {
		return nil, err
	}
	r.LastListJobs = req
	out := &app.JobList{}
	for _, j := range r.Jobs {
		out.Jobs = append(out.Jobs, *j)
	}
	return out, nil
}

func (r *Remote) GetJob(_ context.Context, req app.GetJobRequest) (*app.Job, error) {
	err := r.record("GetJob")
	r.mu.Lock()
	r.GetJobReqs = append(r.GetJobReqs, req)
	r.mu.Unlock()
	if r.GetJobFunc != nil {
		return r.GetJobFunc(req)
	}
	if err != nil {
		return nil, err
	}
	if j, ok := r.Jobs[req.JobID]; ok {
		return j, nil
	}
	return nil, notFound("任务")
}

func (r *Remote) jobAction(name, id string, fn func(string) (*app.Job, error)) (*app.Job, error) {
	err := r.record(name)
	if fn != nil {
		return fn(id)
	}
	if err != nil {
		return nil, err
	}
	if j, ok := r.Jobs[id]; ok {
		return j, nil
	}
	return nil, notFound("任务")
}

func (r *Remote) CancelJob(_ context.Context, id string) (*app.Job, error) {
	return r.jobAction("CancelJob", id, r.CancelJobFunc)
}

func (r *Remote) RetryJob(_ context.Context, id string) (*app.Job, error) {
	return r.jobAction("RetryJob", id, r.RetryJobFunc)
}

func (r *Remote) ListVideos(_ context.Context, req app.ListVideosRequest) (*app.VideoList, error) {
	if err := r.record("ListVideos"); err != nil {
		return nil, err
	}
	r.LastListVideos = req
	return &app.VideoList{Videos: r.Videos}, nil
}

func (r *Remote) GetVideo(_ context.Context, id string) (*app.Video, error) {
	if err := r.record("GetVideo"); err != nil {
		return nil, err
	}
	for i := range r.Videos {
		if r.Videos[i].VideoID == id {
			return &r.Videos[i], nil
		}
	}
	return nil, notFound("视频")
}

func (r *Remote) GetNote(_ context.Context, req app.GetNoteRequest) (*app.Note, error) {
	err := r.record("GetNote")
	r.LastNoteReq = req
	if r.GetNoteFunc != nil {
		return r.GetNoteFunc(req)
	}
	if err != nil {
		return nil, err
	}
	if r.Note == nil {
		return nil, notFound("笔记")
	}
	n := *r.Note
	return &n, nil
}

func (r *Remote) GetTranscript(_ context.Context, req app.GetTranscriptRequest) (*app.Transcript, error) {
	err := r.record("GetTranscript")
	r.LastTrReq = req
	if r.GetTranscriptFunc != nil {
		return r.GetTranscriptFunc(req)
	}
	if err != nil {
		return nil, err
	}
	if r.Tr == nil {
		return nil, notFound("字幕")
	}
	t := *r.Tr
	return &t, nil
}

func (r *Remote) GetPlayback(context.Context, string) (*app.Playback, error) {
	if err := r.record("GetPlayback"); err != nil {
		return nil, err
	}
	return &app.Playback{}, nil
}

func (r *Remote) CreateExport(_ context.Context, req app.ExportRequest) (*app.Export, error) {
	if err := r.record("CreateExport"); err != nil {
		return nil, err
	}
	r.LastExportReq = req
	if r.Export == nil {
		return nil, notFound("笔记")
	}
	e := *r.Export
	return &e, nil
}

func (r *Remote) OpenExport(_ context.Context, id string) (io.ReadCloser, *app.Export, error) {
	if err := r.record("OpenExport"); err != nil {
		return nil, nil, err
	}
	if r.Export == nil || r.Export.ExportID != id {
		return nil, nil, notFound("导出")
	}
	e := *r.Export
	return io.NopCloser(bytes.NewReader(r.ExportBody)), &e, nil
}

func (r *Remote) GetSettings(context.Context) (*app.Settings, error) {
	if err := r.record("GetSettings"); err != nil {
		return nil, err
	}
	if r.Settings == nil {
		s := app.DefaultSettings()
		return &s, nil
	}
	s := *r.Settings
	return &s, nil
}

func (r *Remote) PatchSettings(_ context.Context, patch map[string]any) (*app.Settings, error) {
	if err := r.record("PatchSettings"); err != nil {
		return nil, err
	}
	r.LastPatch = patch
	return r.GetSettings(context.Background())
}

func (r *Remote) SetSecret(_ context.Context, name, value string) error {
	if err := r.record("SetSecret"); err != nil {
		return err
	}
	r.LastSecretName, r.LastSecret, r.SecretSet = name, value, true
	return nil
}

func (r *Remote) ListLLMModels(_ context.Context, req app.LLMModelsRequest) (*app.LLMModelList, error) {
	if err := r.record("ListLLMModels"); err != nil {
		return nil, err
	}
	return &app.LLMModelList{BaseURL: req.BaseURL, Models: []string{"model-a", "model-b"}}, nil
}

func (r *Remote) TestLLM(context.Context) (*app.LLMTestResult, error) {
	if err := r.record("TestLLM"); err != nil {
		return nil, err
	}
	if r.LLMTest == nil {
		return &app.LLMTestResult{OK: true, Model: "m", StructuredOutput: "json_schema", LatencyMs: 12}, nil
	}
	return r.LLMTest, nil
}

func (r *Remote) tools() *app.ToolsStatus {
	if r.Tools == nil {
		return &app.ToolsStatus{Ready: true}
	}
	t := *r.Tools
	return &t
}

func (r *Remote) GetTools(context.Context) (*app.ToolsStatus, error) {
	if err := r.record("GetTools"); err != nil {
		return nil, err
	}
	return r.tools(), nil
}

func (r *Remote) PrepareTools(_ context.Context, req app.PrepareToolsRequest) (*app.Job, error) {
	err := r.record("PrepareTools")
	r.LastPrepare = req
	if r.PrepareFunc != nil {
		return r.PrepareFunc(req)
	}
	if err != nil {
		return nil, err
	}
	return &app.Job{JobID: "job_prep", Kind: app.JobKindPrepareTools, Status: app.JobSucceeded, Rev: 1, Progress: 1}, nil
}

func (r *Remote) UpdateTool(_ context.Context, name string) (*app.ToolsStatus, error) {
	if err := r.record("UpdateTool"); err != nil {
		return nil, err
	}
	r.LastUpdateTool = name
	return r.tools(), nil
}

func (r *Remote) ImportTool(_ context.Context, req app.ToolImportRequest) (*app.ToolsStatus, error) {
	if err := r.record("ImportTool"); err != nil {
		return nil, err
	}
	r.LastToolImport = req
	return r.tools(), nil
}

func (r *Remote) Subscribe(context.Context, int64) (<-chan app.Event, bool, error) {
	if err := r.record("Subscribe"); err != nil {
		return nil, false, err
	}
	ch := make(chan app.Event)
	close(ch)
	return ch, false, nil
}

func (r *Remote) BaseURL() string { return r.URL }

func (r *Remote) Shutdown(_ context.Context, force bool) error {
	if err := r.record("Shutdown"); err != nil {
		return err
	}
	r.ShutdownCalled, r.LastForce = true, force
	return nil
}

func (r *Remote) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Closed++
	return nil
}

// Connector returns an app.Connector that always yields r and records the
// options of every connection in *opts (when non-nil).
func Connector(r *Remote, opts *[]app.ConnectOptions) app.Connector {
	return func(_ context.Context, o app.ConnectOptions) (app.Remote, error) {
		if opts != nil {
			*opts = append(*opts, o)
		}
		if o.NoSpawn && r == nil {
			return nil, app.E(app.CodeEngineUnavailable, "Engine 未运行")
		}
		if r == nil {
			return nil, app.E(app.CodeEngineUnavailable, "无法连接 Engine")
		}
		return r, nil
	}
}
