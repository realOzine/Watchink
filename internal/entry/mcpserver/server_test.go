package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"watchink/internal/app"
)

// absPath turns a Unix-style absolute path into one that is also absolute on
// the OS running the test: Windows needs a drive letter, and the import tool
// only accepts absolute local paths.
func absPath(p string) string {
	if runtime.GOOS == "windows" {
		return `C:` + filepath.FromSlash(p)
	}
	return p
}

// fakeRemote implements the part of app.Remote the MCP tools use; any other
// method panics through the embedded nil interface.
type fakeRemote struct {
	app.Remote

	mu     sync.Mutex
	calls  []string
	closed int
	// err is returned by every method when set.
	err error

	importReq app.ImportRequest
	jobReq    app.GetJobRequest
	jobID     string
	listReq   app.ListVideosRequest
	noteReq   app.GetNoteRequest
	trReq     app.GetTranscriptRequest
	exportReq app.ExportRequest

	job        *app.Job
	exportBody string
}

func (f *fakeRemote) record(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	return f.err
}

func (f *fakeRemote) called(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == name {
			n++
		}
	}
	return n
}

func (f *fakeRemote) BaseURL() string { return "http://127.0.0.1:47823" }

func (f *fakeRemote) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeRemote) Import(_ context.Context, req app.ImportRequest) (*app.ImportResponse, error) {
	if err := f.record("Import"); err != nil {
		return nil, err
	}
	f.importReq = req
	return &app.ImportResponse{VideoID: "vid_1", JobID: "job_1", Status: app.JobQueued}, nil
}

func (f *fakeRemote) theJob() *app.Job {
	if f.job != nil {
		return f.job
	}
	vid := "vid_1"
	return &app.Job{JobID: "job_1", Kind: app.JobKindProcessVideo, VideoID: &vid, Status: app.JobRunning, Rev: 4, Attempt: 1,
		Stage: app.StageTranscribe, Progress: 0.4, CreatedAt: "2026-01-02T03:04:05Z", UpdatedAt: "2026-01-02T03:04:05Z",
		Stages: []app.JobStage{{Stage: app.StageTranscribe, Status: app.StageRunning, Progress: 0.5, Attempt: 1}}}
}

func (f *fakeRemote) GetJob(_ context.Context, req app.GetJobRequest) (*app.Job, error) {
	if err := f.record("GetJob"); err != nil {
		return nil, err
	}
	f.jobReq = req
	return f.theJob(), nil
}

func (f *fakeRemote) CancelJob(_ context.Context, id string) (*app.Job, error) {
	if err := f.record("CancelJob"); err != nil {
		return nil, err
	}
	f.jobID = id
	j := f.theJob()
	j.Status = app.JobCancelling
	return j, nil
}

func (f *fakeRemote) RetryJob(_ context.Context, id string) (*app.Job, error) {
	if err := f.record("RetryJob"); err != nil {
		return nil, err
	}
	f.jobID = id
	// A bare job: nil stages must still satisfy the output schema.
	return &app.Job{JobID: id, Kind: app.JobKindProcessVideo, Status: app.JobQueued, Rev: 9}, nil
}

func (f *fakeRemote) ListVideos(_ context.Context, req app.ListVideosRequest) (*app.VideoList, error) {
	if err := f.record("ListVideos"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.listReq = req
	f.mu.Unlock()
	if req.TitleContains == "none" {
		return &app.VideoList{}, nil
	}
	next := "cur2"
	return &app.VideoList{NextCursor: &next, Videos: []app.Video{{VideoID: "vid_1", Title: "线性代数", Source: app.VideoSource{Type: app.SourceYouTube, URI: "https://youtu.be/x"}, Status: app.VideoReady}}}, nil
}

func (f *fakeRemote) GetNote(_ context.Context, req app.GetNoteRequest) (*app.Note, error) {
	if err := f.record("GetNote"); err != nil {
		return nil, err
	}
	f.noteReq = req
	next := 100
	return &app.Note{NoteID: "note_1", VideoID: req.VideoID, TranscriptID: "tr_1", Version: 2, Title: "线性代数", Format: app.FormatMarkdown,
		Content: "# 线性代数\n[1](wk://seg/a)", TotalChars: 500, NextOffset: &next, LLMModel: "m",
		Citations: []app.Citation{{RefNo: 1, PointID: "p1", SegmentID: "seg_a", StartMs: 1000, EndMs: 2000}},
		Coverage:  app.Coverage{Chunks: 3, Points: 7}}, nil
}

func (f *fakeRemote) GetTranscript(_ context.Context, req app.GetTranscriptRequest) (*app.Transcript, error) {
	if err := f.record("GetTranscript"); err != nil {
		return nil, err
	}
	f.trReq = req
	if len(req.SegmentIDs) > 0 {
		return &app.Transcript{TranscriptID: "tr_1", Language: "zh"}, nil
	}
	next := int64(9000)
	return &app.Transcript{TranscriptID: "tr_1", VideoID: req.VideoID, Language: "zh", Engine: "whispercpp", SegmentCount: 1, Format: app.FormatJSON,
		Segments: []app.Segment{{SegmentID: "seg_a", Idx: 1, StartMs: 1000, EndMs: 2000, Text: "大家好"}}, Truncated: true, NextStartMs: &next}, nil
}

func (f *fakeRemote) CreateExport(_ context.Context, req app.ExportRequest) (*app.Export, error) {
	if err := f.record("CreateExport"); err != nil {
		return nil, err
	}
	f.exportReq = req
	return &app.Export{ExportID: "exp_1", VideoID: req.VideoID, Format: req.Format, Path: "/data/exports/n.md", Filename: "n.md", Bytes: int64(len(f.exportBody)), SHA256: "abc", DownloadURL: "http://x"}, nil
}

func (f *fakeRemote) OpenExport(_ context.Context, id string) (io.ReadCloser, *app.Export, error) {
	if err := f.record("OpenExport"); err != nil {
		return nil, nil, err
	}
	return io.NopCloser(strings.NewReader(f.exportBody)), &app.Export{ExportID: id}, nil
}

type fixture struct {
	t        *testing.T
	remotes  []*fakeRemote // handed out in order; the last one repeats
	connects []app.ConnectOptions
	connErr  error
	session  *mcp.ClientSession
	done     chan error
	mu       sync.Mutex
}

func (fx *fixture) connector() app.Connector {
	return func(_ context.Context, o app.ConnectOptions) (app.Remote, error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.connects = append(fx.connects, o)
		if fx.connErr != nil {
			return nil, fx.connErr
		}
		return fx.remotes[min(len(fx.connects), len(fx.remotes))-1], nil
	}
}

func (fx *fixture) connectCount() int {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return len(fx.connects)
}

// start runs the real serve loop over the SDK's in-memory transport.
func start(t *testing.T, remotes ...*fakeRemote) *fixture {
	t.Helper()
	if len(remotes) == 0 {
		remotes = []*fakeRemote{{exportBody: "# note\n"}}
	}
	fx := &fixture{t: t, remotes: remotes, done: make(chan error, 1)}
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		fx.done <- serve(ctx, Options{Connect: fx.connector(), DataDir: "/d", NoSpawn: true, Version: "9.9.9"}, st)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	var err error
	for i := 0; i < 200; i++ { // the server side must be connected first
		if fx.session, err = client.Connect(ctx, ct, nil); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		fx.session.Close()
		cancel()
	})
	return fx
}

// call invokes a tool; a protocol-level error fails the test.
func (fx *fixture) call(name string, args any) *mcp.CallToolResult {
	fx.t.Helper()
	res, err := fx.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		fx.t.Fatalf("%s: protocol error (business errors must be tool results): %v", name, err)
	}
	return res
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("want one content block, got %d", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T", res.Content[0])
	}
	return tc.Text
}

// ok asserts a success result whose text block is the same JSON as
// structuredContent, and returns it.
func (fx *fixture) ok(name string, args any) map[string]any {
	fx.t.Helper()
	res := fx.call(name, args)
	if res.IsError {
		fx.t.Fatalf("%s: unexpected error result: %s", name, text(fx.t, res))
	}
	var fromText, structured map[string]any
	if err := json.Unmarshal([]byte(text(fx.t, res)), &fromText); err != nil {
		fx.t.Fatalf("%s: text content is not JSON: %v", name, err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &structured); err != nil || structured == nil {
		fx.t.Fatalf("%s: structuredContent missing: %s", name, raw)
	}
	if !reflect.DeepEqual(fromText, structured) {
		fx.t.Fatalf("%s: text and structuredContent differ:\n%v\n%v", name, fromText, structured)
	}
	return structured
}

// fail asserts an isError result carrying the error object.
func (fx *fixture) fail(name string, args any) *app.Error {
	fx.t.Helper()
	res := fx.call(name, args)
	if !res.IsError {
		fx.t.Fatalf("%s: expected isError result", name)
	}
	if res.StructuredContent != nil {
		fx.t.Errorf("%s: error results must not carry structuredContent", name)
	}
	body := text(fx.t, res)
	var raw map[string]map[string]any
	var env app.ErrorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error == nil || json.Unmarshal([]byte(body), &raw) != nil {
		fx.t.Fatalf("%s: content is not an error object: %s", name, body)
	}
	for _, k := range []string{"code", "message", "retryable", "user_action_required"} {
		if _, ok := raw["error"][k]; !ok {
			fx.t.Errorf("%s: error object lacks %q: %s", name, k, body)
		}
	}
	return env.Error
}

type M = map[string]any

var allTools = []string{ToolImportVideo, ToolGetJob, ToolCancelJob, ToolRetryJob, ToolListVideos, ToolGetNote, ToolGetTranscript, ToolExportNote}

func TestInitializeAndDiscoveryWithoutEngine(t *testing.T) {
	fx := start(t)
	fx.connErr = app.E(app.CodeEngineUnavailable, "没有 Engine")
	init := fx.session.InitializeResult()
	if init.ServerInfo.Name != "watchink" || init.ServerInfo.Version != "9.9.9" {
		t.Errorf("server info = %+v", init.ServerInfo)
	}
	for _, want := range []string{"import_video", "get_job", "wait_seconds", "get_note", "get_transcript", "segment_ids", "isError"} {
		if !strings.Contains(init.Instructions, want) {
			t.Errorf("instructions do not mention %q", want)
		}
	}
	if init.Capabilities.Tools == nil || init.Capabilities.Resources != nil || init.Capabilities.Prompts != nil {
		t.Errorf("capabilities = %+v (tools only)", init.Capabilities)
	}
	res, err := fx.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	want := slices.Clone(allTools)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v", names)
	}
	if fx.connectCount() != 0 {
		t.Error("tool discovery must not connect to the engine")
	}
	// The first call connects; a failure is a tool error, not a protocol one.
	if e := fx.fail(ToolListVideos, M{}); e.Code != app.CodeEngineUnavailable || !e.Retryable {
		t.Errorf("error = %+v", e)
	}
	if got := fx.connects[0]; got != (app.ConnectOptions{DataDir: "/d", NoSpawn: true, Requester: "mcp"}) {
		t.Errorf("connect options = %+v", got)
	}
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 && r != '…' {
			return false
		}
	}
	return true
}

// checkDescriptions walks a schema: every property needs an English
// description.
func checkDescriptions(t *testing.T, path string, s map[string]any) {
	t.Helper()
	props, _ := s["properties"].(map[string]any)
	for name, p := range props {
		ps := p.(map[string]any)
		d, _ := ps["description"].(string)
		if d == "" || !isASCII(d) {
			t.Errorf("%s.%s: missing or non-English description %q", path, name, d)
		}
		checkDescriptions(t, path+"."+name, ps)
		if items, ok := ps["items"].(map[string]any); ok {
			checkDescriptions(t, path+"."+name+"[]", items)
		}
	}
}

func TestToolListSchemasAndAnnotations(t *testing.T) {
	fx := start(t)
	res, err := fx.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
		in := tool.InputSchema.(map[string]any)
		out, _ := tool.OutputSchema.(map[string]any)
		if in["type"] != "object" || in["additionalProperties"] != false {
			t.Errorf("%s: input schema = %v", tool.Name, in)
		}
		if out == nil || out["type"] != "object" {
			t.Errorf("%s: missing output schema", tool.Name)
		}
		if len(tool.Description) < 40 || !isASCII(tool.Description) || tool.Title == "" {
			t.Errorf("%s: description/title not usable: %q", tool.Name, tool.Description)
		}
		checkDescriptions(t, tool.Name+".in", in)
		checkDescriptions(t, tool.Name+".out", out)
		if tool.Annotations == nil {
			t.Fatalf("%s: no annotations", tool.Name)
		}
	}

	prop := func(tool, name string) map[string]any {
		t.Helper()
		p, ok := tools[tool].InputSchema.(map[string]any)["properties"].(map[string]any)[name].(map[string]any)
		if !ok {
			t.Fatalf("%s has no input %q", tool, name)
		}
		return p
	}
	required := func(tool string) []any {
		r, _ := tools[tool].InputSchema.(map[string]any)["required"].([]any)
		return r
	}
	if w := prop(ToolGetJob, "wait_seconds"); w["minimum"] != float64(0) || w["maximum"] != float64(30) || w["type"] != "integer" {
		t.Errorf("wait_seconds = %v", w)
	}
	for _, c := range []struct {
		tool, name string
		enum       []any
	}{
		{ToolImportVideo, "language", []any{"auto", "zh", "en"}},
		{ToolImportVideo, "reprocess_from", []any{"acquire", "transcribe", "note"}},
		{ToolGetNote, "format", []any{"markdown", "json"}},
		{ToolExportNote, "format", []any{"md", "md-raw", "json", "srt", "vtt", "txt"}},
	} {
		if got := prop(c.tool, c.name)["enum"]; !reflect.DeepEqual(got, c.enum) {
			t.Errorf("%s.%s enum = %v", c.tool, c.name, got)
		}
	}
	if prop(ToolGetTranscript, "segment_ids")["maxItems"] != float64(50) || prop(ToolListVideos, "limit")["maximum"] != float64(100) {
		t.Error("segment_ids / limit bounds missing")
	}
	for tool, want := range map[string][]any{
		ToolImportVideo: {"source"}, ToolGetJob: {"job_id"}, ToolCancelJob: {"job_id"}, ToolRetryJob: {"job_id"},
		ToolListVideos: nil, ToolGetNote: {"video_id"}, ToolGetTranscript: {"video_id"}, ToolExportNote: {"video_id", "format"},
	} {
		if got := required(tool); !reflect.DeepEqual(got, want) {
			t.Errorf("%s required = %v, want %v", tool, got, want)
		}
	}

	// Annotations per the table.
	type hints struct{ readOnly, idempotent bool }
	wantHints := map[string]hints{
		ToolImportVideo: {false, true}, ToolGetJob: {true, false}, ToolCancelJob: {false, true}, ToolRetryJob: {false, false},
		ToolListVideos: {true, false}, ToolGetNote: {true, false}, ToolGetTranscript: {true, false}, ToolExportNote: {false, false},
	}
	for name, want := range wantHints {
		a := tools[name].Annotations
		if a.ReadOnlyHint != want.readOnly || a.IdempotentHint != want.idempotent {
			t.Errorf("%s: readOnly=%v idempotent=%v", name, a.ReadOnlyHint, a.IdempotentHint)
		}
		if !a.ReadOnlyHint && (a.DestructiveHint == nil || *a.DestructiveHint) {
			t.Errorf("%s: no v0 tool is destructive", name)
		}
	}
}

// jsonFields lists the JSON member names of a struct type.
func jsonFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func schemaProps(s schema) []string {
	var out []string
	for name := range s["properties"].(schema) {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// TestSchemasMatchGoTypes keeps the hand-written schemas in sync with the
// typed inputs and the contract DTOs.
func TestSchemasMatchGoTypes(t *testing.T) {
	items := func(s schema, name string) schema { return s["properties"].(schema)[name].(schema)["items"].(schema) }
	sub := func(s schema, name string) schema { return s["properties"].(schema)[name].(schema) }
	cases := []struct {
		name string
		s    schema
		typ  reflect.Type
	}{
		{"import in", importInput, reflect.TypeFor[ImportVideoInput]()},
		{"get_job in", getJobInput, reflect.TypeFor[GetJobInput]()},
		{"job id in", jobIDInput, reflect.TypeFor[JobIDInput]()},
		{"list_videos in", listVideosInput, reflect.TypeFor[ListVideosInput]()},
		{"get_note in", getNoteInput, reflect.TypeFor[GetNoteInput]()},
		{"get_transcript in", getTranscriptInput, reflect.TypeFor[GetTranscriptInput]()},
		{"export in", exportInput, reflect.TypeFor[ExportNoteInput]()},
		{"import out", importSchema, reflect.TypeFor[app.ImportResponse]()},
		{"job out", jobSchema, reflect.TypeFor[app.Job]()},
		{"job stage", items(jobSchema, "stages"), reflect.TypeFor[app.JobStage]()},
		{"job result", sub(jobSchema, "result"), reflect.TypeFor[app.JobResult]()},
		{"error", errorSchema, reflect.TypeFor[app.Error]()},
		{"video list", videoListSchema, reflect.TypeFor[app.VideoList]()},
		{"video", videoSchema, reflect.TypeFor[app.Video]()},
		{"video source", sub(videoSchema, "source"), reflect.TypeFor[app.VideoSource]()},
		{"note out", noteSchema, reflect.TypeFor[NoteOutput]()},
		{"citation", items(noteSchema, "citations"), reflect.TypeFor[app.Citation]()},
		{"coverage", sub(noteSchema, "coverage"), reflect.TypeFor[app.Coverage]()},
		{"transcript out", transcriptSchema, reflect.TypeFor[TranscriptOutput]()},
		{"segment", segmentSchema, reflect.TypeFor[app.Segment]()},
		{"export out", exportSchema, reflect.TypeFor[ExportOutput]()},
	}
	for _, c := range cases {
		if got, want := schemaProps(c.s), jsonFields(c.typ); !slices.Equal(got, want) {
			t.Errorf("%s: schema properties %v != struct fields %v", c.name, got, want)
		}
	}
}

func TestImportVideo(t *testing.T) {
	fx := start(t)
	r := fx.remotes[0]
	out := fx.ok(ToolImportVideo, M{"source": absPath("/Users/me/lec.mp4"), "language": "zh", "reprocess_from": "note"})
	if out["video_id"] != "vid_1" || out["job_id"] != "job_1" || out["status"] != "queued" || out["note_id"] != nil {
		t.Errorf("out = %v", out)
	}
	if w, ok := out["warnings"].([]any); !ok || len(w) != 0 {
		t.Errorf("warnings must be an array: %v", out["warnings"])
	}
	want := app.ImportRequest{Source: app.ImportSource{Type: app.SourceFile, Path: absPath("/Users/me/lec.mp4")}, Language: "zh", ReprocessFrom: "note", RequestedBy: "mcp"}
	if r.importReq != want {
		t.Errorf("request = %+v", r.importReq)
	}
	fx.ok(ToolImportVideo, M{"source": "https://www.youtube.com/watch?v=abc"})
	if r.importReq.Source != (app.ImportSource{Type: app.SourceURL, URL: "https://www.youtube.com/watch?v=abc"}) {
		t.Errorf("url source = %+v", r.importReq.Source)
	}

	before := r.called("Import")
	for src, code := range map[string]app.Code{
		"lec.mp4": app.CodeInvalidArgument, "./a/lec.mp4": app.CodeInvalidArgument, "~/lec.mp4": app.CodeInvalidArgument,
		"   ": app.CodeInvalidArgument, "ftp://host/a.mp4": app.CodeUnsupportedSource,
	} {
		if e := fx.fail(ToolImportVideo, M{"source": src}); e.Code != code {
			t.Errorf("source %q: code = %s, want %s", src, e.Code, code)
		}
	}
	if r.called("Import") != before {
		t.Error("invalid sources must not reach the engine")
	}

	r.err = app.E(app.CodePathNotAllowed, "路径不在允许的导入根目录内").WithHint("调整 security.import_roots")
	e := fx.fail(ToolImportVideo, M{"source": absPath("/etc/passwd")})
	if e.Code != app.CodePathNotAllowed || e.Retryable || !e.UserActionRequired || e.Hint == "" {
		t.Errorf("error = %+v", e)
	}
}

func TestJobTools(t *testing.T) {
	fx := start(t)
	r := fx.remotes[0]
	out := fx.ok(ToolGetJob, M{"job_id": "job_1", "wait_seconds": 30, "after_rev": 3})
	if r.jobReq != (app.GetJobRequest{JobID: "job_1", WaitSeconds: 30, AfterRev: 3}) {
		t.Errorf("request = %+v", r.jobReq)
	}
	if out["status"] != "running" || out["rev"] != float64(4) || out["error"] != nil || len(out["stages"].([]any)) != 1 {
		t.Errorf("out = %v", out)
	}
	fx.ok(ToolGetJob, M{"job_id": "job_1"})
	if r.jobReq.WaitSeconds != 0 {
		t.Errorf("default wait = %d", r.jobReq.WaitSeconds)
	}

	// A failed job is a successful read whose error member is the object.
	r.job = &app.Job{JobID: "job_1", Kind: app.JobKindProcessVideo, Status: app.JobFailed, Rev: 7,
		Error:  app.E(app.CodeModelAuthFailed, "鉴权失败").WithStage(app.StageNoteMap).WithDetail("http_status", 401),
		Stages: []app.JobStage{{Stage: app.StageNoteMap, Status: app.StageFailed, Error: app.E(app.CodeModelAuthFailed, "鉴权失败")}}}
	out = fx.ok(ToolGetJob, M{"job_id": "job_1"})
	if je := out["error"].(map[string]any); je["code"] != "MODEL_AUTH_FAILED" || je["retryable"] != true || je["stage"] != "note_map" {
		t.Errorf("job error = %v", out["error"])
	}
	r.job = nil

	if out = fx.ok(ToolCancelJob, M{"job_id": "job_1"}); out["status"] != "cancelling" || r.jobID != "job_1" {
		t.Errorf("cancel = %v", out)
	}
	out = fx.ok(ToolRetryJob, M{"job_id": "job_2"})
	if s, ok := out["stages"].([]any); out["status"] != "queued" || r.jobID != "job_2" || !ok || len(s) != 0 || out["video_id"] != nil {
		t.Errorf("retry = %v", out)
	}

	for _, tool := range []string{ToolGetJob, ToolCancelJob, ToolRetryJob} {
		r.err = app.E(app.CodeNotFound, "任务不存在")
		if e := fx.fail(tool, M{"job_id": "job_x"}); e.Code != app.CodeNotFound {
			t.Errorf("%s: %+v", tool, e)
		}
		r.err = app.E(app.CodeConflict, "当前状态不允许该操作")
		if e := fx.fail(tool, M{"job_id": "job_1"}); e.Code != app.CodeConflict {
			t.Errorf("%s: %+v", tool, e)
		}
	}
}

func TestListVideos(t *testing.T) {
	fx := start(t)
	r := fx.remotes[0]
	out := fx.ok(ToolListVideos, M{"limit": 5, "cursor": "cur1", "title_contains": "线性"})
	if r.listReq != (app.ListVideosRequest{Limit: 5, Cursor: "cur1", TitleContains: "线性"}) {
		t.Errorf("request = %+v", r.listReq)
	}
	v := out["videos"].([]any)[0].(map[string]any)
	if out["next_cursor"] != "cur2" || v["video_id"] != "vid_1" || v["duration_ms"] != nil {
		t.Errorf("out = %v", out)
	}
	out = fx.ok(ToolListVideos, M{"title_contains": "none"})
	if vs, ok := out["videos"].([]any); !ok || len(vs) != 0 || out["next_cursor"] != nil {
		t.Errorf("empty list = %v", out)
	}
	r.err = app.E(app.CodeInvalidArgument, "cursor 无效")
	if e := fx.fail(ToolListVideos, M{"cursor": "bad"}); e.Code != app.CodeInvalidArgument {
		t.Errorf("%+v", e)
	}
}

func TestGetNote(t *testing.T) {
	fx := start(t)
	r := fx.remotes[0]
	out := fx.ok(ToolGetNote, M{"video_id": "vid_1", "format": "markdown", "offset": 50, "max_chars": 50})
	if r.noteReq != (app.GetNoteRequest{VideoID: "vid_1", Format: "markdown", Offset: 50, MaxChars: 50}) {
		t.Errorf("request = %+v", r.noteReq)
	}
	var keys []string
	for k := range out {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"citations", "content", "coverage", "format", "next_offset", "note_id", "title", "total_chars"}; !slices.Equal(keys, want) {
		t.Errorf("output members = %v, want exactly %v", keys, want)
	}
	if out["next_offset"] != float64(100) || out["total_chars"] != float64(500) || out["citations"].([]any)[0].(map[string]any)["segment_id"] != "seg_a" {
		t.Errorf("out = %v", out)
	}
	r.err = app.E(app.CodeNotFound, "该视频还没有笔记")
	if e := fx.fail(ToolGetNote, M{"video_id": "vid_2"}); e.Code != app.CodeNotFound {
		t.Errorf("%+v", e)
	}
}

func TestGetTranscript(t *testing.T) {
	fx := start(t)
	r := fx.remotes[0]
	out := fx.ok(ToolGetTranscript, M{"video_id": "vid_1", "start_ms": 0, "end_ms": 60000, "max_chars": 100})
	req := r.trReq
	if req.VideoID != "vid_1" || *req.StartMs != 0 || *req.EndMs != 60000 || req.MaxChars != 100 || req.Format != app.FormatJSON || req.SegmentIDs != nil {
		t.Errorf("request = %+v", req)
	}
	var keys []string
	for k := range out {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"language", "next_start_ms", "segments", "transcript_id", "truncated"}; !slices.Equal(keys, want) {
		t.Errorf("output members = %v, want exactly %v", keys, want)
	}
	if out["truncated"] != true || out["next_start_ms"] != float64(9000) || out["segments"].([]any)[0].(map[string]any)["text"] != "大家好" {
		t.Errorf("out = %v", out)
	}

	out = fx.ok(ToolGetTranscript, M{"video_id": "vid_1", "segment_ids": []string{"seg_a", "seg_b"}})
	if !slices.Equal(r.trReq.SegmentIDs, []string{"seg_a", "seg_b"}) || r.trReq.StartMs != nil {
		t.Errorf("request = %+v", r.trReq)
	}
	if s, ok := out["segments"].([]any); !ok || len(s) != 0 || out["next_start_ms"] != nil {
		t.Errorf("empty segments = %v", out)
	}

	before := r.called("GetTranscript")
	for _, args := range []M{
		{"video_id": "vid_1", "segment_ids": []string{"seg_a"}, "start_ms": 5},
		{"video_id": "vid_1", "start_ms": 500, "end_ms": 100},
		{"video_id": "vid_1", "segment_ids": make([]string, 51)},
		{"video_id": "vid_1", "start_ms": -1},
	} {
		if e := fx.fail(ToolGetTranscript, args); e.Code != app.CodeInvalidArgument {
			t.Errorf("%v: %+v", args, e)
		}
	}
	if r.called("GetTranscript") != before {
		t.Error("invalid arguments must not reach the engine")
	}
	r.err = app.E(app.CodeNotFound, "视频不存在")
	if e := fx.fail(ToolGetTranscript, M{"video_id": "vid_x"}); e.Code != app.CodeNotFound {
		t.Errorf("%+v", e)
	}
}

func TestExportNote(t *testing.T) {
	fx := start(t)
	r := fx.remotes[0]
	out := fx.ok(ToolExportNote, M{"video_id": "vid_1", "format": "md"})
	if r.exportReq != (app.ExportRequest{VideoID: "vid_1", Format: "md"}) {
		t.Errorf("request = %+v", r.exportReq)
	}
	if out["path"] != "/data/exports/n.md" || out["filename"] != "n.md" || out["bytes"] != float64(7) || out["sha256"] != "abc" || out["content"] != "# note\n" {
		t.Errorf("out = %v", out)
	}
	if _, leaked := out["download_url"]; leaked || len(out) != 5 {
		t.Errorf("output members = %v", out)
	}

	out = fx.ok(ToolExportNote, M{"video_id": "vid_1", "format": "srt", "inline": false})
	if _, has := out["content"]; has || r.called("OpenExport") != 1 {
		t.Errorf("inline=false must not read the file: %v", out)
	}

	// At the limit (20000 chars) and above, only the path is returned.
	r.exportBody = strings.Repeat("字", app.DefaultMaxChars)
	if out = fx.ok(ToolExportNote, M{"video_id": "vid_1", "format": "md"}); out["content"] != nil {
		t.Error("content at max_chars must not be inlined")
	}
	r.exportBody = strings.Repeat("字", app.DefaultMaxChars-1)
	if out = fx.ok(ToolExportNote, M{"video_id": "vid_1", "format": "md"}); out["content"] != r.exportBody {
		t.Error("content just under max_chars must be inlined")
	}
	opens := r.called("OpenExport")
	r.exportBody = strings.Repeat("x", maxInlineBytes+1)
	if out = fx.ok(ToolExportNote, M{"video_id": "vid_1", "format": "txt"}); out["content"] != nil || r.called("OpenExport") != opens {
		t.Error("oversized exports must not be read at all")
	}

	r.err = app.E(app.CodeNotFound, "该视频还没有笔记")
	if e := fx.fail(ToolExportNote, M{"video_id": "vid_1", "format": "md"}); e.Code != app.CodeNotFound {
		t.Errorf("%+v", e)
	}
}

// TestSchemaViolationsAreErrorObjects: arguments rejected by the input
// schema come back in the same error-object shape, without touching the
// engine.
func TestSchemaViolationsAreErrorObjects(t *testing.T) {
	fx := start(t)
	cases := []struct {
		tool string
		args M
	}{
		{ToolGetJob, M{}},
		{ToolGetJob, M{"job_id": ""}},
		{ToolGetJob, M{"job_id": "job_1", "wait_seconds": 31}},
		{ToolGetJob, M{"job_id": "job_1", "wait_seconds": -1}},
		{ToolGetJob, M{"job_id": "job_1", "wait_seconds": "soon"}},
		{ToolGetJob, M{"job_id": "job_1", "surprise": true}},
		{ToolImportVideo, M{"source": absPath("/a.mp4"), "language": "fr"}},
		{ToolImportVideo, M{"source": absPath("/a.mp4"), "reprocess_from": "probe"}},
		{ToolImportVideo, M{}},
		{ToolListVideos, M{"limit": 101}},
		{ToolListVideos, M{"limit": 0}},
		{ToolGetNote, M{"video_id": "vid_1", "format": "html"}},
		{ToolGetNote, M{"video_id": "vid_1", "offset": -5}},
		{ToolExportNote, M{"video_id": "vid_1"}},
		{ToolExportNote, M{"video_id": "vid_1", "format": "pdf"}},
		{ToolCancelJob, M{"job": "job_1"}},
	}
	for _, c := range cases {
		e := fx.fail(c.tool, c.args)
		if e.Code != app.CodeInvalidArgument || e.Retryable || e.Details["reason"] == nil {
			t.Errorf("%s %v: %+v", c.tool, c.args, e)
		}
	}
	if fx.connectCount() != 0 {
		t.Error("schema violations must not connect to the engine")
	}
	// An unknown tool is the one legitimate protocol error.
	if _, err := fx.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "nope", Arguments: M{}}); err == nil {
		t.Error("unknown tool should be a protocol error")
	}
}

func TestErrorObjectPreservesContractFields(t *testing.T) {
	fx := start(t)
	for _, code := range app.KnownCodes() {
		want := app.E(code, "出错了").WithHint("提示").WithDetail("k", "v")
		fx.remotes[0].err = want
		got := fx.fail(ToolGetNote, M{"video_id": "vid_1"})
		if code == app.CodeEngineUnavailable {
			continue // covered by the reconnect tests
		}
		if got.Code != code || got.Retryable != want.Retryable || got.UserActionRequired != want.UserActionRequired || got.Hint != "提示" || got.Details["k"] != "v" || got.Message != "出错了" {
			t.Errorf("%s: %+v", code, got)
		}
	}
	// A non-app error is INTERNAL, still a tool result.
	fx.remotes[0].err = io.ErrUnexpectedEOF
	if e := fx.fail(ToolGetNote, M{"video_id": "vid_1"}); e.Code != app.CodeInternal {
		t.Errorf("%+v", e)
	}
}

func TestReconnectOnceWhenEngineWentAway(t *testing.T) {
	dead := &fakeRemote{err: app.E(app.CodeEngineUnavailable, "连接被拒绝")}
	alive := &fakeRemote{}
	fx := start(t, dead, alive)

	// The engine idle-exited: the call fails once, the bridge reconnects
	// (re-spawning) and the caller only sees the successful retry.
	out := fx.ok(ToolListVideos, M{})
	if out["next_cursor"] != "cur2" {
		t.Errorf("out = %v", out)
	}
	if fx.connectCount() != 2 || dead.closed != 1 || dead.called("ListVideos") != 1 || alive.called("ListVideos") != 1 {
		t.Errorf("connects=%d dead.closed=%d dead.calls=%v alive.calls=%v", fx.connectCount(), dead.closed, dead.calls, alive.calls)
	}
	// The new connection is kept for later calls.
	fx.ok(ToolGetJob, M{"job_id": "job_1"})
	if fx.connectCount() != 2 || alive.closed != 0 {
		t.Errorf("connection not reused: connects=%d", fx.connectCount())
	}
}

func TestReconnectIsAttemptedOnlyOnce(t *testing.T) {
	dead1 := &fakeRemote{err: app.E(app.CodeEngineUnavailable, "连接被拒绝")}
	dead2 := &fakeRemote{err: app.E(app.CodeEngineUnavailable, "连接被拒绝")}
	fx := start(t, dead1, dead2)
	if e := fx.fail(ToolGetNote, M{"video_id": "vid_1"}); e.Code != app.CodeEngineUnavailable {
		t.Errorf("%+v", e)
	}
	if fx.connectCount() != 2 || dead1.called("GetNote") != 1 || dead2.called("GetNote") != 1 {
		t.Errorf("connects=%d calls=%v %v", fx.connectCount(), dead1.calls, dead2.calls)
	}

	// Other errors never trigger a reconnect.
	r := &fakeRemote{err: app.E(app.CodeNotFound, "不存在")}
	fx = start(t, r)
	fx.fail(ToolGetNote, M{"video_id": "vid_1"})
	if fx.connectCount() != 1 || r.closed != 0 || r.called("GetNote") != 1 {
		t.Errorf("connects=%d closed=%d", fx.connectCount(), r.closed)
	}

	// A failed reconnect is reported, and the next call tries again.
	dead := &fakeRemote{err: app.E(app.CodeEngineUnavailable, "连接被拒绝")}
	fx = start(t, dead)
	fx.fail(ToolGetNote, M{"video_id": "vid_1"}) // connect #1, reconnect #2 hands out `dead` again
	fx.mu.Lock()
	fx.connErr = app.E(app.CodeEngineUnavailable, "无法拉起 Engine")
	fx.mu.Unlock()
	if e := fx.fail(ToolGetNote, M{"video_id": "vid_1"}); e.Code != app.CodeEngineUnavailable || e.Message != "无法拉起 Engine" {
		t.Errorf("%+v", e)
	}
}

func TestConcurrentCallsShareOneConnection(t *testing.T) {
	fx := start(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := fx.session.CallTool(context.Background(), &mcp.CallToolParams{Name: ToolListVideos, Arguments: M{}})
			if err != nil || res.IsError {
				t.Errorf("call failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if fx.connectCount() != 1 {
		t.Errorf("connects = %d", fx.connectCount())
	}
}

// TestDisconnectKeepsJobsRunning: when the client goes away the server only
// closes its engine connection; it never cancels jobs or stops the engine.
func TestDisconnectKeepsJobsRunning(t *testing.T) {
	fx := start(t)
	r := fx.remotes[0]
	fx.ok(ToolImportVideo, M{"source": absPath("/v/a.mp4")})
	fx.session.Close()
	select {
	case <-fx.done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after the client disconnected")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed != 1 {
		t.Errorf("remote closed %d times", r.closed)
	}
	if !slices.Equal(r.calls, []string{"Import"}) {
		t.Errorf("calls after disconnect = %v", r.calls)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	_, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, Options{}, st) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("cancellation is a clean stop, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
}

func TestNewServerDefaults(t *testing.T) {
	s := NewServer(Options{})
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if v := cs.InitializeResult().ServerInfo.Version; v == "" {
		t.Error("version must default to internal/version")
	}
	// Without a connector every call is a clean ENGINE_UNAVAILABLE result.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: ToolListVideos, Arguments: M{}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "ENGINE_UNAVAILABLE") {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}
