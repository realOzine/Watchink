package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"watchink/internal/app"
	"watchink/internal/cli/clitest"
)

type result struct {
	code     int
	out, err string
}

type harness struct {
	t      *testing.T
	remote *clitest.Remote
	deps   Deps
	conns  []app.ConnectOptions
}

func ptr[T any](v T) *T { return &v }

func newHarness(t *testing.T) *harness {
	t.Helper()
	vid := "vid_1"
	r := &clitest.Remote{
		URL:    "http://127.0.0.1:47823",
		Engine: &app.EngineInfo{Version: "0.1.0", BuildID: "b1", API: 1, PID: 42, Mode: "background", BaseURL: "http://127.0.0.1:47823", DataDir: "/data", StartedAt: "2026-01-02T03:04:05Z", ToolsReady: true, LLMConfigured: true},
		Jobs: map[string]*app.Job{
			"job_1": {JobID: "job_1", Kind: app.JobKindProcessVideo, VideoID: &vid, Status: app.JobRunning, Rev: 3, Stage: app.StageTranscribe, Progress: 0.4,
				Stages:    []app.JobStage{{Stage: app.StageAcquire, Status: app.StageSucceeded, Progress: 1, Reused: true}, {Stage: app.StageTranscribe, Status: app.StageRunning, Progress: 0.5}},
				CreatedAt: "2026-01-02T03:04:05Z", UpdatedAt: "2026-01-02T03:04:05Z"},
		},
		Videos: []app.Video{{VideoID: "vid_1", Title: "线性代数 第一讲", Source: app.VideoSource{Type: app.SourceFile, URI: "/v/a.mp4"}, DurationMs: ptr(int64(3723000)), Status: app.VideoReady, CreatedAt: "2026-01-02T03:04:05Z", UpdatedAt: "2026-01-02T03:04:05Z"}},
		Note:   &app.Note{NoteID: "note_1", VideoID: "vid_1", Title: "线性代数", Format: app.FormatMarkdown, Content: "# 线性代数\n\n要点 [1](wk://seg/x)\n", TotalChars: 20},
		Tr: &app.Transcript{TranscriptID: "tr_1", VideoID: "vid_1", Language: "zh", Format: app.FormatJSON, SegmentCount: 1,
			Segments: []app.Segment{{SegmentID: "seg_x_00001", Idx: 1, StartMs: 1000, EndMs: 2000, Text: "大家好"}}},
		Settings: func() *app.Settings {
			s := app.DefaultSettings()
			s.LLM.BaseURL, s.LLM.Model = "https://llm.example/v1", "m1"
			s.LLM.APIKeyConfigured, s.LLM.APIKeyLast4, s.LLM.APIKeySource = true, "9xyz", "file"
			s.DataDir = "/data"
			return &s
		}(),
		Tools: &app.ToolsStatus{Ready: true, DefaultASRModel: "whisper-small", Items: []app.ToolItem{
			{Name: "ffmpeg", Kind: app.ToolKindTool, Status: app.ToolReady, Required: true, Version: "7.1", SizeBytes: 1 << 20},
			{Name: "whisper-small", Kind: app.ToolKindModel, Status: app.ToolReady, Required: true},
			{Name: "deno", Kind: app.ToolKindTool, Status: app.ToolMissing, Lazy: true, DownloadBytes: 5 << 20},
		}},
	}
	h := &harness{t: t, remote: r}
	h.deps = Deps{
		Connect:         clitest.Connector(r, &h.conns),
		ResolveDataDir:  func(flag string) (string, error) { return cmp(flag, "/data"), nil },
		IsEngineRunning: func(string) bool { return false },
		SetDataDir:      func(string) error { return nil },
		Version:         "0.1.0", BuildID: "b1",
	}
	return h
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (h *harness) run(args ...string) result {
	h.t.Helper()
	var out, errb bytes.Buffer
	code := Execute(context.Background(), h.deps, args, &out, &errb)
	return result{code, out.String(), errb.String()}
}

// oneJSON asserts s is exactly one JSON value and returns it as a map.
func oneJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	if strings.ContainsRune(s, 0x1b) {
		t.Fatalf("output contains ANSI escape: %q", s)
	}
	dec := json.NewDecoder(strings.NewReader(s))
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, s)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout holds more than one JSON value:\n%s", s)
	}
	return v
}

func errCode(t *testing.T, v map[string]any) string {
	t.Helper()
	e, ok := v["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in %v", v)
	}
	return e["code"].(string)
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (h *harness) withExport(body string) {
	sum := sha256.Sum256([]byte(body))
	h.remote.Export = &app.Export{ExportID: "exp_1", VideoID: "vid_1", Format: "md", Path: "/data/exports/n.md", Filename: "线性代数.md", Bytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	h.remote.ExportBody = []byte(body)
}

// TestJSONContract runs every client command with --json and checks the
// rules: exit 0, exactly one JSON value on stdout, silent stderr.
func TestJSONContract(t *testing.T) {
	file := writeTemp(t, "a.mp4", "x")
	cases := []struct {
		name string
		args []string
		key  string // a member that must be present
	}{
		{"version", []string{"version"}, "build_id"},
		{"import", []string{"import", file}, "job_id"},
		{"jobs list", []string{"jobs", "list", "--status", "running", "--video", "vid_1", "--limit", "5"}, "jobs"},
		{"jobs get", []string{"jobs", "get", "job_1"}, "job_id"},
		{"jobs cancel", []string{"jobs", "cancel", "job_1"}, "job_id"},
		{"jobs retry", []string{"jobs", "retry", "job_1"}, "job_id"},
		{"videos list", []string{"videos", "list", "--limit", "3", "--title-contains", "线性"}, "videos"},
		{"videos get", []string{"videos", "get", "vid_1"}, "video_id"},
		{"note get", []string{"note", "get", "vid_1"}, "content"},
		{"note get json", []string{"note", "get", "vid_1", "--format", "json"}, "note_id"},
		{"transcript get", []string{"transcript", "get", "vid_1", "--from", "0:10", "--to", "1:00:00"}, "segments"},
		{"export stdout", []string{"export", "vid_1", "--format", "md", "--out", "-"}, "content"},
		{"config show", []string{"config", "show"}, "llm"},
		{"config get", []string{"config", "get", "llm.model"}, "value"},
		{"config set", []string{"config", "set", "llm.model", "m2"}, "llm"},
		{"config set-data-dir", []string{"config", "set-data-dir", t.TempDir()}, "data_dir"},
		{"config set-secret delete", []string{"config", "set-secret", "llm_api_key", "--delete"}, "name"},
		{"llm test", []string{"llm", "test"}, "ok"},
		{"tools status", []string{"tools", "status"}, "items"},
		{"tools prepare", []string{"tools", "prepare", "ffmpeg"}, "job_id"},
		{"tools prepare wait", []string{"tools", "prepare", "--wait"}, "job_id"},
		{"tools update", []string{"tools", "update", "yt-dlp"}, "items"},
		{"tools import", []string{"tools", "import", "ffmpeg", file}, "items"},
		{"tools import-model", []string{"tools", "import-model", "whisper-small", file}, "items"},
		{"engine status", []string{"engine", "status"}, "running"},
		{"engine stop", []string{"engine", "stop", "--force"}, "stopped"},
		{"doctor", []string{"doctor"}, "checks"},
		{"web", []string{"web"}, "url"},
		{"setup", []string{"setup", "--llm-model", "m3", "--prepare"}, "settings"},
		{"help", []string{"--help"}, "help"},
		{"group help", []string{"jobs"}, "help"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.withExport("# note\n")
			h.deps.OpenBrowser = func(string) error { return nil }
			res := h.run(append(tc.args, "--json")...)
			if res.code != 0 {
				t.Fatalf("exit %d, stdout %s stderr %s", res.code, res.out, res.err)
			}
			v := oneJSON(t, res.out)
			if _, ok := v[tc.key]; !ok {
				t.Errorf("missing %q in %s", tc.key, res.out)
			}
			if res.err != "" {
				t.Errorf("stderr must be silent with --json, got %q", res.err)
			}
			if h.remote.Closed != len(h.conns) {
				t.Errorf("connections opened %d, closed %d", len(h.conns), h.remote.Closed)
			}
		})
	}
}

// TestHumanOutput smoke-tests the Chinese human rendering of each command.
func TestHumanOutput(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"version"}, []string{"watchink 0.1.0 (build b1)"}},
		{[]string{"jobs", "list"}, []string{"任务", "状态", "job_1", "运行中", "40%"}},
		{[]string{"jobs", "get", "job_1"}, []string{"阶段", "✓ acquire", "(复用)", "▶ transcribe", "50%"}},
		{[]string{"videos", "list"}, []string{"标题", "线性代数 第一讲", "1:02:03", "就绪"}},
		{[]string{"videos", "get", "vid_1"}, []string{"来源", "/v/a.mp4"}},
		{[]string{"transcript", "get", "vid_1"}, []string{"[0:00:01] 大家好"}},
		{[]string{"config", "show"}, []string{"llm.model", "= m1", "asr.vad", "= true"}},
		{[]string{"config", "get", "llm.model"}, []string{"m1"}},
		{[]string{"tools", "status"}, []string{"名称", "ffmpeg", "模型", "按需", "均已就绪"}},
		{[]string{"engine", "status"}, []string{"Engine 正在运行", "数据目录"}},
		{[]string{"doctor"}, []string{"✓ 数据目录", "Engine", "在线模型", "末四位 9xyz", "一切就绪"}},
		{[]string{"llm", "test"}, []string{"在线模型可用"}},
		{[]string{"import", "https://www.youtube.com/watch?v=abc"}, []string{"vid_1", "job_1", "jobs get job_1 --follow"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			h := newHarness(t)
			res := h.run(tc.args...)
			if res.code != 0 {
				t.Fatalf("exit %d: %s", res.code, res.err)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.out, w) {
					t.Errorf("missing %q in:\n%s", w, res.out)
				}
			}
			if strings.ContainsRune(res.out, 0x1b) {
				t.Error("ANSI escape in output")
			}
		})
	}
}

func TestNoteGetHumanIsRawMarkdownAndPaged(t *testing.T) {
	h := newHarness(t)
	pages := []string{"# 标题\n\n第一", "页 [1](wk://seg/a)\n"}
	h.remote.GetNoteFunc = func(req app.GetNoteRequest) (*app.Note, error) {
		n := app.Note{NoteID: "note_1", VideoID: req.VideoID, Format: req.Format, TotalChars: 30}
		if req.Offset == 0 {
			n.Content, n.NextOffset = pages[0], ptr(9)
		} else {
			if req.Offset != 9 || req.NoteID != "note_1" {
				t.Errorf("second page request = %+v", req)
			}
			n.Content = pages[1]
		}
		return &n, nil
	}
	res := h.run("note", "get", "vid_1")
	if res.code != 0 || res.out != pages[0]+pages[1] {
		t.Fatalf("got %d %q", res.code, res.out)
	}
	if h.remote.LastNoteReq.Format != app.FormatMarkdown {
		t.Errorf("format = %q", h.remote.LastNoteReq.Format)
	}
	res = h.run("note", "get", "vid_1", "--json")
	v := oneJSON(t, res.out)
	if v["content"] != pages[0]+pages[1] || v["next_offset"] != nil {
		t.Errorf("json note = %v", v)
	}
	if res := h.run("note", "get", "vid_1", "--format", "pdf", "--json"); res.code != 2 {
		t.Errorf("bad format exit = %d", res.code)
	}
}

func TestErrorCodeToExitCode(t *testing.T) {
	want := map[app.Code]int{
		app.CodeInvalidArgument: 2, app.CodeUnsupportedSource: 2, app.CodeNotFound: 3, app.CodeConflict: 4,
		app.CodeEngineUnavailable: 5, app.CodeEngineVersionMismatch: 5, app.CodeToolMissing: 6, app.CodeChecksumMismatch: 6,
		app.CodeModelNotConfigured: 7, app.CodeModelAuthFailed: 7, app.CodePathNotAllowed: 7,
		app.CodeNetworkError: 10, app.CodeSourceUnavailable: 10, app.CodeSourceBlocked: 10,
		app.CodeInternal: 1, app.CodeToolFailed: 1, app.CodeDiskFull: 1,
	}
	for _, code := range append(app.KnownCodes(), app.CodeUnauthorized) {
		t.Run(string(code), func(t *testing.T) {
			h := newHarness(t)
			h.remote.Err = app.E(code, "出错了").WithHint("修一下")
			res := h.run("videos", "list", "--json")
			if res.code != app.ExitCode(code) {
				t.Errorf("exit = %d, want %d", res.code, app.ExitCode(code))
			}
			if w, ok := want[code]; ok && res.code != w {
				t.Errorf("exit = %d, want %d", res.code, w)
			}
			v := oneJSON(t, res.out)
			if errCode(t, v) != string(code) {
				t.Errorf("error code = %v", v)
			}
			if res.err != "" {
				t.Errorf("stderr = %q", res.err)
			}
			// Human mode: nothing on stdout, the error on stderr.
			res = h.run("videos", "list")
			if res.out != "" || !strings.Contains(res.err, "["+string(code)+"]") || !strings.Contains(res.err, "提示：修一下") {
				t.Errorf("human error: out %q err %q", res.out, res.err)
			}
		})
	}
	// A connection failure maps the same way.
	h := newHarness(t)
	h.deps.Connect = clitest.Connector(nil, nil)
	if res := h.run("videos", "list", "--json"); res.code != app.ExitEngine || errCode(t, oneJSON(t, res.out)) != "ENGINE_UNAVAILABLE" {
		t.Errorf("connect failure: %+v", res)
	}
	// A non-app error is INTERNAL.
	h = newHarness(t)
	h.remote.Err = errors.New("boom")
	if res := h.run("videos", "list", "--json"); res.code != 1 || errCode(t, oneJSON(t, res.out)) != "INTERNAL" {
		t.Errorf("plain error: %+v", res)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{"nope"},
		{"jobs", "nope"},
		{"jobs", "get"},
		{"jobs", "get", "a", "b"},
		{"videos", "list", "--bogus"},
		{"videos", "list", "--limit", "abc"},
		{"import"},
		{"import", "a.mp4", "--lang", "fr"},
		{"import", "a.mp4", "--reprocess-from", "probe"},
		{"import", "ftp://host/a.mp4"},
		{"export", "vid_1"},
		{"export", "vid_1", "--format", "pdf"},
		{"transcript", "get", "vid_1", "--from", "abc"},
		{"transcript", "get", "vid_1", "--from", "2:00", "--to", "1:00"},
		{"transcript", "get", "vid_1", "--format", "pdf"},
		{"config", "set", "llm.model"},
		{"config", "set", "nope.key", "v"},
		{"config", "set-secret", "other"},
		{"version", "extra"},
		{"version", "--timeout", "soon"},
		{"desktop"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h := newHarness(t)
			res := h.run(append(args, "--json")...)
			if res.code != app.ExitUsage {
				t.Fatalf("exit = %d, want 2 (%s)", res.code, res.out)
			}
			if c := errCode(t, oneJSON(t, res.out)); c != "INVALID_ARGUMENT" && c != "UNSUPPORTED_SOURCE" {
				t.Errorf("code = %s", c)
			}
			if res.err != "" {
				t.Errorf("stderr = %q", res.err)
			}
			res = h.run(args...)
			if res.code != app.ExitUsage || res.out != "" || !strings.Contains(res.err, "错误 [") {
				t.Errorf("human: %+v", res)
			}
		})
	}
}

func TestGlobalFlagsReachConnector(t *testing.T) {
	h := newHarness(t)
	res := h.run("--data-dir", "/x", "--no-spawn", "--timeout", "3", "videos", "list", "--json")
	if res.code != 0 {
		t.Fatal(res)
	}
	want := app.ConnectOptions{DataDir: "/x", NoSpawn: true, Timeout: 3e9, Requester: "cli"}
	if len(h.conns) != 1 || h.conns[0] != want {
		t.Errorf("connect options = %+v", h.conns)
	}
	h = newHarness(t)
	h.run("videos", "list", "--timeout", "1m30s")
	if h.conns[0].Timeout != 90e9 || h.conns[0].NoSpawn || h.conns[0].DataDir != "" {
		t.Errorf("connect options = %+v", h.conns)
	}
}

func TestVerboseAllowsStderrInJSONMode(t *testing.T) {
	h := newHarness(t)
	res := h.run("videos", "list", "--json", "--verbose")
	oneJSON(t, res.out)
	if !strings.Contains(res.err, "正在连接 Engine") {
		t.Errorf("stderr = %q", res.err)
	}
}

func TestImportSources(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	t.Chdir(dir)
	res := h.run("import", "sub/a.mp4", "--lang", "zh", "--reprocess-from", "note", "--json")
	if res.code != 0 {
		t.Fatal(res)
	}
	got := h.remote.LastImport
	if got.Source.Type != app.SourceFile || !filepath.IsAbs(got.Source.Path) || !strings.HasSuffix(got.Source.Path, filepath.Join("sub", "a.mp4")) {
		t.Errorf("file source = %+v", got.Source)
	}
	if got.Language != "zh" || got.ReprocessFrom != "note" || got.RequestedBy != "cli" {
		t.Errorf("request = %+v", got)
	}
	for _, u := range []string{"https://www.bilibili.com/video/BV1xx", "HTTP://youtu.be/abc"} {
		h.run("import", u, "--json")
		if s := h.remote.LastImport.Source; s.Type != app.SourceURL || s.URL != u || s.Path != "" {
			t.Errorf("url source = %+v", s)
		}
	}
}

func TestImportAlreadyProcessedAndWarnings(t *testing.T) {
	h := newHarness(t)
	h.remote.ImportFunc = func(app.ImportRequest) (*app.ImportResponse, error) {
		return &app.ImportResponse{VideoID: "vid_1", JobID: "job_old", Status: app.ImportStatusAlreadyProcessed, NoteID: ptr("note_1"), Warnings: []string{app.WarnModelNotConfigured}}, nil
	}
	res := h.run("import", "/v/a.mp4", "--wait", "--json")
	if res.code != 0 {
		t.Fatal(res)
	}
	v := oneJSON(t, res.out)
	if v["status"] != "already_processed" || v["note_id"] != "note_1" {
		t.Errorf("out = %v", v)
	}
	if h.remote.Called("GetJob") != 0 {
		t.Error("already_processed must not wait")
	}
	res = h.run("import", "/v/a.mp4")
	if res.code != 0 || !strings.Contains(res.out, "已处理过") || !strings.Contains(res.err, "尚未配置在线模型") {
		t.Errorf("human: %+v", res)
	}
}

// scriptJobs makes GetJob return the given jobs in order (the last one
// repeats).
func scriptJobs(h *harness, jobs ...*app.Job) {
	i := 0
	h.remote.GetJobFunc = func(app.GetJobRequest) (*app.Job, error) {
		j := jobs[min(i, len(jobs)-1)]
		i++
		return j, nil
	}
}

func job(status string, rev int64, progress float64) *app.Job {
	vid := "vid_1"
	j := &app.Job{JobID: "job_1", Kind: app.JobKindProcessVideo, VideoID: &vid, Status: status, Rev: rev, Progress: progress}
	if !j.Terminal() {
		j.Stage = app.StageTranscribe
	}
	return j
}

func TestImportWait(t *testing.T) {
	failed := job(app.JobFailed, 3, 0.5)
	failed.Error = app.E(app.CodeModelAuthFailed, "鉴权失败").WithStage(app.StageNoteMap).WithDetail("http_status", 401)

	cases := []struct {
		name     string
		args     []string
		jobs     []*app.Job
		exit     int
		errCode  string
		status   string
		progress bool
	}{
		{"success", []string{"--wait"}, []*app.Job{job(app.JobQueued, 1, 0), job(app.JobRunning, 2, 0.5), job(app.JobSucceeded, 3, 1)}, 0, "", app.JobSucceeded, true},
		{"failed", []string{"--wait=60"}, []*app.Job{job(app.JobRunning, 2, 0.5), failed}, app.ExitJobFailed, "MODEL_AUTH_FAILED", app.JobFailed, true},
		{"cancelled", []string{"--wait"}, []*app.Job{job(app.JobRunning, 2, 0.5), job(app.JobCancelled, 3, 0.5)}, app.ExitJobCancelled, "CONFLICT", app.JobCancelled, true},
		{"timeout", []string{"--wait=1"}, []*app.Job{job(app.JobRunning, 2, 0.5)}, app.ExitWaitTimeout, "CONFLICT", app.JobRunning, true},
		{"interrupted", []string{"--wait"}, []*app.Job{job(app.JobInterrupted, 2, 0.5)}, app.ExitEngine, "ENGINE_UNAVAILABLE", app.JobInterrupted, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			scriptJobs(h, tc.jobs...)
			res := h.run(append([]string{"import", "/v/a.mp4", "--json"}, tc.args...)...)
			if res.code != tc.exit {
				t.Fatalf("exit = %d, want %d\n%s", res.code, tc.exit, res.out)
			}
			if res.err != "" {
				t.Errorf("stderr = %q", res.err)
			}
			v := oneJSON(t, res.out)
			j := v["job"].(map[string]any)
			if j["status"] != tc.status {
				t.Errorf("job status = %v", j["status"])
			}
			if tc.errCode == "" {
				if v["video_id"] != "vid_1" || v["error"] != nil {
					t.Errorf("success output = %v", v)
				}
			} else {
				if errCode(t, v) != tc.errCode {
					t.Errorf("error = %v", v["error"])
				}
				d := v["error"].(map[string]any)["details"].(map[string]any)
				if d["job_id"] != "job_1" || d["job_status"] != tc.status {
					t.Errorf("details = %v", d)
				}
			}
			for _, req := range h.remote.GetJobReqs[1:] {
				if req.WaitSeconds < 1 || req.WaitSeconds > app.MaxWaitSeconds {
					t.Errorf("wait_seconds = %d", req.WaitSeconds)
				}
			}

			// Human mode: progress on stderr, stage list on stdout.
			h = newHarness(t)
			scriptJobs(h, tc.jobs...)
			res = h.run(append([]string{"import", "/v/a.mp4"}, tc.args...)...)
			if res.code != tc.exit || !strings.Contains(res.err, "%]") || !strings.Contains(res.out, "job_1") {
				t.Errorf("human: %+v", res)
			}
		})
	}
	if failed.Error.Details["job_id"] != nil {
		t.Error("the job's own error object must not be mutated")
	}
}

func TestWaitSendsAfterRev(t *testing.T) {
	h := newHarness(t)
	scriptJobs(h, job(app.JobQueued, 1, 0), job(app.JobRunning, 5, 0.5), job(app.JobSucceeded, 6, 1))
	if res := h.run("jobs", "get", "job_1", "--wait", "30", "--json"); res.code != 0 {
		t.Fatal(res)
	}
	reqs := h.remote.GetJobReqs
	if len(reqs) != 3 || reqs[0].WaitSeconds != 0 || reqs[1].AfterRev != 1 || reqs[2].AfterRev != 5 || reqs[1].WaitSeconds != 30 {
		t.Errorf("requests = %+v", reqs)
	}
}

func TestImportWaitSecondsAsSeparateArgument(t *testing.T) {
	h := newHarness(t)
	scriptJobs(h, job(app.JobSucceeded, 3, 1))
	res := h.run("import", "--wait", "600", "/v/a.mp4", "--json")
	// The CLI makes the path absolute, which on Windows adds a drive letter.
	want, _ := filepath.Abs("/v/a.mp4")
	if res.code != 0 || h.remote.LastImport.Source.Path != want {
		t.Fatalf("%+v %+v", res, h.remote.LastImport)
	}
}

func TestJobsGetWaitExitCodes(t *testing.T) {
	h := newHarness(t)
	failed := job(app.JobFailed, 3, 0.5)
	failed.Error = app.E(app.CodeASRFailed, "转写失败")
	scriptJobs(h, failed)
	// Without --wait a failed job is just a resource.
	if res := h.run("jobs", "get", "job_1", "--json"); res.code != 0 || oneJSON(t, res.out)["status"] != "failed" {
		t.Errorf("plain get: %+v", res)
	}
	res := h.run("jobs", "get", "job_1", "--wait", "5", "--json")
	if res.code != app.ExitJobFailed || errCode(t, oneJSON(t, res.out)) != "ASR_FAILED" {
		t.Errorf("wait get: %+v", res)
	}
	h = newHarness(t)
	scriptJobs(h, job(app.JobRunning, 2, 0.1))
	if res := h.run("jobs", "get", "job_1", "--wait", "1", "--json"); res.code != app.ExitWaitTimeout {
		t.Errorf("timeout: %+v", res)
	}
}

func TestJobsFollowNDJSON(t *testing.T) {
	h := newHarness(t)
	scriptJobs(h, job(app.JobQueued, 1, 0), job(app.JobRunning, 2, 0.3), job(app.JobRunning, 2, 0.3), job(app.JobRunning, 3, 0.8), job(app.JobSucceeded, 4, 1))
	res := h.run("jobs", "get", "job_1", "--follow", "--json")
	if res.code != 0 || res.err != "" {
		t.Fatalf("%+v", res)
	}
	lines := strings.Split(strings.TrimSpace(res.out), "\n")
	var revs []float64
	for _, l := range lines {
		var j map[string]any
		if err := json.Unmarshal([]byte(l), &j); err != nil {
			t.Fatalf("line is not JSON: %q", l)
		}
		revs = append(revs, j["rev"].(float64))
	}
	if !reflect.DeepEqual(revs, []float64{1, 2, 3, 4}) {
		t.Errorf("revs = %v (one line per rev change)", revs)
	}

	// A failing job ends the stream with its own line and exit code 8.
	h = newHarness(t)
	failed := job(app.JobFailed, 2, 0.2)
	failed.Error = app.E(app.CodeASRFailed, "转写失败")
	scriptJobs(h, job(app.JobRunning, 1, 0), failed)
	res = h.run("jobs", "get", "job_1", "--follow", "--json")
	lines = strings.Split(strings.TrimSpace(res.out), "\n")
	if res.code != app.ExitJobFailed || len(lines) != 2 || !strings.Contains(lines[1], `"status":"failed"`) {
		t.Errorf("failed follow: %+v", res)
	}

	// Human follow: progress on stderr, final job on stdout.
	h = newHarness(t)
	scriptJobs(h, job(app.JobRunning, 1, 0.2), job(app.JobSucceeded, 2, 1))
	res = h.run("jobs", "get", "job_1", "--follow")
	if res.code != 0 || !strings.Contains(res.err, "20%") || !strings.Contains(res.out, "已完成") {
		t.Errorf("human follow: %+v", res)
	}
}

func TestExport(t *testing.T) {
	const body = "# 笔记\n\n内容\n"
	t.Run("default file in cwd", func(t *testing.T) {
		h := newHarness(t)
		h.withExport(body)
		dir := t.TempDir()
		t.Chdir(dir)
		res := h.run("export", "vid_1", "--format", "md", "--json")
		if res.code != 0 {
			t.Fatal(res)
		}
		v := oneJSON(t, res.out)
		got, err := os.ReadFile(filepath.Join(dir, "线性代数.md"))
		if err != nil || string(got) != body {
			t.Fatalf("file: %q %v", got, err)
		}
		if filepath.Base(v["output_path"].(string)) != "线性代数.md" || v["sha256"] == "" || v["content"] != nil {
			t.Errorf("out = %v", v)
		}
		if h.remote.LastExportReq != (app.ExportRequest{VideoID: "vid_1", Format: "md"}) {
			t.Errorf("req = %+v", h.remote.LastExportReq)
		}
	})
	t.Run("explicit path and directory", func(t *testing.T) {
		h := newHarness(t)
		h.withExport(body)
		dir := t.TempDir()
		target := filepath.Join(dir, "out.md")
		if res := h.run("export", "vid_1", "--format", "md", "--out", target); res.code != 0 || !strings.Contains(res.out, target) {
			t.Fatal(res)
		}
		if got, _ := os.ReadFile(target); string(got) != body {
			t.Errorf("file = %q", got)
		}
		if res := h.run("export", "vid_1", "--format", "md", "--out", dir); res.code != 0 {
			t.Fatal(res)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "线性代数.md")); string(got) != body {
			t.Errorf("dir file = %q", got)
		}
	})
	t.Run("stdout", func(t *testing.T) {
		h := newHarness(t)
		h.withExport(body)
		if res := h.run("export", "vid_1", "--format", "md", "--out", "-"); res.code != 0 || res.out != body || res.err != "" {
			t.Errorf("%+v", res)
		}
		res := h.run("export", "vid_1", "--format", "md", "--out", "-", "--json")
		if v := oneJSON(t, res.out); v["content"] != body || v["output_path"] != "-" {
			t.Errorf("json stdout = %v", v)
		}
	})
	t.Run("file name cannot escape the directory", func(t *testing.T) {
		h := newHarness(t)
		h.withExport(body)
		h.remote.Export.Filename = "../../evil.md"
		dir := t.TempDir()
		t.Chdir(dir)
		res := h.run("export", "vid_1", "--format", "md", "--json")
		if res.code != 0 {
			t.Fatal(res)
		}
		if _, err := os.Stat(filepath.Join(dir, "evil.md")); err != nil {
			t.Errorf("expected file inside cwd: %v", err)
		}
	})
	t.Run("checksum mismatch leaves no file", func(t *testing.T) {
		h := newHarness(t)
		h.withExport(body)
		h.remote.Export.SHA256 = strings.Repeat("0", 64)
		dir := t.TempDir()
		res := h.run("export", "vid_1", "--format", "md", "--out", filepath.Join(dir, "o.md"), "--json")
		if res.code != app.ExitToolNotReady || errCode(t, oneJSON(t, res.out)) != "CHECKSUM_MISMATCH" {
			t.Errorf("%+v", res)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("leftover files: %v", entries)
		}
	})
	t.Run("unwritable target", func(t *testing.T) {
		h := newHarness(t)
		h.withExport(body)
		res := h.run("export", "vid_1", "--format", "md", "--out", filepath.Join(t.TempDir(), "missing", "o.md"), "--json")
		if res.code != app.ExitUsage {
			t.Errorf("%+v", res)
		}
	})
}

func TestParseTime(t *testing.T) {
	good := map[string]int64{"0:00": 0, "1:05": 65000, "90:00": 5400000, "1:02:03": 3723000, "12345ms": 12345, "0:01.5": 1500, "0ms": 0}
	for in, want := range good {
		if got, err := parseTime(in); err != nil || got != want {
			t.Errorf("parseTime(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "12", "1:60", "1:99:00", "-1:00", "a:b", "1:2:3:4", "12s", "-5ms", "1:", ":30", "1:1e1"} {
		if _, err := parseTime(in); !app.IsCode(err, app.CodeInvalidArgument) {
			t.Errorf("parseTime(%q) err = %v", in, err)
		}
	}
}

func TestTranscriptGet(t *testing.T) {
	h := newHarness(t)
	h.remote.Tr.Format, h.remote.Tr.Content = app.FormatSRT, "1\n00:00:01,000 --> 00:00:02,000\n大家好\n"
	res := h.run("transcript", "get", "vid_1", "--from", "1:05", "--to", "70000ms", "--format", "srt")
	if res.code != 0 || res.out != h.remote.Tr.Content {
		t.Fatalf("%+v", res)
	}
	req := h.remote.LastTrReq
	if *req.StartMs != 65000 || *req.EndMs != 70000 || req.Format != "srt" || req.MaxChars >= 0 {
		t.Errorf("req = %+v", req)
	}
	// An explicit --format json prints JSON even in human mode.
	h = newHarness(t)
	res = h.run("transcript", "get", "vid_1", "--format", "json")
	if v := oneJSON(t, res.out); v["transcript_id"] != "tr_1" {
		t.Errorf("out = %s", res.out)
	}
	// A truncating engine is paged through.
	h = newHarness(t)
	h.remote.GetTranscriptFunc = func(req app.GetTranscriptRequest) (*app.Transcript, error) {
		if req.StartMs == nil {
			return &app.Transcript{TranscriptID: "tr_1", Segments: []app.Segment{{Idx: 1}}, Truncated: true, NextStartMs: ptr(int64(5000))}, nil
		}
		return &app.Transcript{TranscriptID: "tr_1", Segments: []app.Segment{{Idx: 2, StartMs: *req.StartMs}}}, nil
	}
	res = h.run("transcript", "get", "vid_1", "--json")
	v := oneJSON(t, res.out)
	if len(v["segments"].([]any)) != 2 || v["truncated"] != false {
		t.Errorf("paged = %s", res.out)
	}
}

func TestConfigSetTypeInference(t *testing.T) {
	cases := []struct {
		key, value string
		want       string // JSON of the patch
	}{
		{"llm.model", "gpt-4o-mini", `{"llm":{"model":"gpt-4o-mini"}}`},
		{"llm.model", "123", `{"llm":{"model":"123"}}`},
		{"llm.temperature", "0.5", `{"llm":{"temperature":0.5}}`},
		{"llm.temperature", "1", `{"llm":{"temperature":1}}`},
		{"llm.max_concurrency", "4", `{"llm":{"max_concurrency":4}}`},
		{"engine.port", "8080", `{"engine":{"port":8080}}`},
		{"engine.auto_resume", "false", `{"engine":{"auto_resume":false}}`},
		{"asr.vad", "on", `{"asr":{"vad":true}}`},
		{"engine.idle_exit", "30m", `{"engine":{"idle_exit":"30m"}}`},
		{"security.import_roots", "~, /Volumes/A", `{"security":{"import_roots":["~","/Volumes/A"]}}`},
		{"security.import_roots", `["/a,b"]`, `{"security":{"import_roots":["/a,b"]}}`},
		{"security.import_roots", "", `{"security":{"import_roots":[]}}`},
		{"download.mirrors.huggingface.co", "https://hf-mirror.com", `{"download":{"mirrors":{"huggingface.co":"https://hf-mirror.com"}}}`},
		{"download.mirrors.huggingface.co", "", `{"download":{"mirrors":{"huggingface.co":null}}}`},
		{"download.mirrors", `{"a.b":"https://x"}`, `{"download":{"mirrors":{"a.b":"https://x"}}}`},
		{"network.proxy", "", `{"network":{"proxy":""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			h := newHarness(t)
			res := h.run("config", "set", tc.key, tc.value, "--json")
			if res.code != 0 {
				t.Fatal(res)
			}
			got, _ := json.Marshal(h.remote.LastPatch)
			if string(got) != tc.want {
				t.Errorf("patch = %s, want %s", got, tc.want)
			}
		})
	}
	bad := [][2]string{
		{"engine.port", "abc"}, {"engine.port", "1.5"}, {"asr.vad", "maybe"}, {"llm.temperature", "hot"},
		{"llm", "x"}, {"llm.api_key", "sk-secret-value"}, {"llm.api_key_configured", "true"}, {"data_dir", "/x"},
		{"llm.model.extra", "x"}, {"", "x"}, {"security.import_roots", "[1,2]"},
	}
	for _, kv := range bad {
		h := newHarness(t)
		res := h.run("config", "set", kv[0], kv[1], "--json")
		if res.code != app.ExitUsage || h.remote.Called("PatchSettings") != 0 {
			t.Errorf("config set %s %s: %+v", kv[0], kv[1], res)
		}
		if kv[1] != "x" && kv[1] != "true" && strings.Contains(res.out, kv[1]) {
			t.Errorf("error output repeats the value: %s", res.out)
		}
	}
}

func TestConfigGet(t *testing.T) {
	h := newHarness(t)
	h.remote.Settings.Download.Mirrors = map[string]string{"huggingface.co": "https://hf-mirror.com"}
	for key, want := range map[string]any{
		"llm.model": "m1", "asr.vad": false, "download.mirrors.huggingface.co": "https://hf-mirror.com",
	} {
		res := h.run("config", "get", key, "--json")
		if v := oneJSON(t, res.out); res.code != 0 || v["value"] != want {
			t.Errorf("%s = %v", key, v)
		}
	}
	if res := h.run("config", "get", "llm", "--json"); oneJSON(t, res.out)["value"].(map[string]any)["model"] != "m1" {
		t.Errorf("group get: %s", res.out)
	}
	if res := h.run("config", "get", "llm"); !strings.Contains(res.out, "llm.model") {
		t.Errorf("human group get: %s", res.out)
	}
	if res := h.run("config", "get", "nope", "--json"); res.code != app.ExitNotFound {
		t.Errorf("unknown key: %+v", res)
	}
}

const secret = "sk-TOPSECRET-abcdef123456"

// assertNoSecret fails when the secret shows up in any output.
func assertNoSecret(t *testing.T, res result) {
	t.Helper()
	if strings.Contains(res.out, secret) || strings.Contains(res.err, secret) {
		t.Fatalf("secret leaked: %+v", res)
	}
}

func TestSecretNeverEchoed(t *testing.T) {
	for _, mode := range [][]string{{}, {"--json"}, {"--json", "--verbose"}, {"--verbose"}} {
		h := newHarness(t)
		h.deps.Stdin = strings.NewReader(secret + "\n")
		res := h.run(append([]string{"config", "set-secret", "llm_api_key"}, mode...)...)
		if res.code != 0 || h.remote.LastSecret != secret || h.remote.LastSecretName != "llm_api_key" {
			t.Fatalf("%v: %+v secret=%q", mode, res, h.remote.LastSecret)
		}
		assertNoSecret(t, res)

		h = newHarness(t)
		h.deps.Stdin = strings.NewReader(secret)
		res = h.run(append([]string{"setup", "--llm-base-url", "https://x/v1", "--llm-model", "m", "--llm-api-key-stdin"}, mode...)...)
		if res.code != 0 || h.remote.LastSecret != secret {
			t.Fatalf("setup %v: %+v", mode, res)
		}
		assertNoSecret(t, res)

		// Failures must not leak it either.
		h = newHarness(t)
		h.deps.Stdin = strings.NewReader(secret)
		h.remote.Err = app.E(app.CodeInternal, "写入失败")
		res = h.run(append([]string{"config", "set-secret", "llm_api_key"}, mode...)...)
		if res.code != 1 {
			t.Fatalf("%+v", res)
		}
		assertNoSecret(t, res)
	}
}

func TestSetSecretEdgeCases(t *testing.T) {
	h := newHarness(t)
	h.deps.Stdin = strings.NewReader("  \n")
	if res := h.run("config", "set-secret", "llm_api_key", "--json"); res.code != app.ExitUsage || h.remote.SecretSet {
		t.Errorf("empty stdin must not delete the key: %+v", res)
	}
	h = newHarness(t)
	if res := h.run("config", "set-secret", "llm_api_key", "--delete", "--json"); res.code != 0 || !h.remote.SecretSet || h.remote.LastSecret != "" {
		t.Errorf("--delete: %+v", res)
	}
	// On a terminal the injected no-echo reader is used.
	h = newHarness(t)
	h.deps.IsTerminal = func() bool { return true }
	h.deps.ReadPassword = func() (string, error) { return secret, nil }
	res := h.run("config", "set-secret", "llm_api_key")
	if res.code != 0 || h.remote.LastSecret != secret {
		t.Errorf("terminal: %+v", res)
	}
	assertNoSecret(t, res)
}

func TestSetDataDir(t *testing.T) {
	h := newHarness(t)
	var set string
	h.deps.SetDataDir = func(d string) error { set = d; return nil }
	t.Chdir(t.TempDir())
	res := h.run("config", "set-data-dir", "newdata", "--json")
	if res.code != 0 || !filepath.IsAbs(set) || filepath.Base(set) != "newdata" {
		t.Fatalf("%+v set=%q", res, set)
	}
	if len(h.conns) != 0 {
		t.Error("set-data-dir must not connect to (or spawn) an engine")
	}
	h = newHarness(t)
	var asked string
	h.deps.IsEngineRunning = func(d string) bool { asked = d; return true }
	h.deps.SetDataDir = func(string) error { t.Error("must not be called"); return nil }
	res = h.run("config", "set-data-dir", "/x", "--json")
	if res.code != app.ExitConflict || errCode(t, oneJSON(t, res.out)) != "CONFLICT" || asked != "/data" {
		t.Errorf("%+v asked=%q", res, asked)
	}
}

func TestSetup(t *testing.T) {
	h := newHarness(t)
	h.remote.PrepareFunc = func(app.PrepareToolsRequest) (*app.Job, error) {
		return &app.Job{JobID: "job_prep", Kind: app.JobKindPrepareTools, Status: app.JobRunning, Rev: 1}, nil
	}
	scriptJobs(h, &app.Job{JobID: "job_prep", Kind: app.JobKindPrepareTools, Status: app.JobSucceeded, Rev: 2, Progress: 1})
	res := h.run("setup", "--llm-base-url", "https://x/v1", "--llm-model", "m", "--asr-model", "whisper-base", "--prepare", "--json")
	if res.code != 0 {
		t.Fatal(res)
	}
	got, _ := json.Marshal(h.remote.LastPatch)
	if string(got) != `{"asr":{"model":"whisper-base"},"llm":{"base_url":"https://x/v1","model":"m"}}` {
		t.Errorf("patch = %s", got)
	}
	v := oneJSON(t, res.out)
	if v["prepare_job"].(map[string]any)["status"] != "succeeded" || v["llm_configured"] != true || h.remote.SecretSet {
		t.Errorf("out = %s", res.out)
	}

	// A failing prepare job exits 8.
	h = newHarness(t)
	h.remote.PrepareFunc = func(app.PrepareToolsRequest) (*app.Job, error) {
		return &app.Job{JobID: "job_prep", Status: app.JobFailed, Rev: 2, Error: app.E(app.CodeNetworkError, "下载失败")}, nil
	}
	res = h.run("setup", "--prepare", "--json")
	if res.code != app.ExitJobFailed || errCode(t, oneJSON(t, res.out)) != "NETWORK_ERROR" {
		t.Errorf("%+v", res)
	}

	// No flags, not a terminal: nothing is asked or changed.
	h = newHarness(t)
	h.deps.Stdin = strings.NewReader("should not be read\n")
	res = h.run("setup")
	if res.code != 0 || h.remote.Called("PatchSettings") != 0 || h.remote.SecretSet || !strings.Contains(res.out, "配置完成") {
		t.Errorf("%+v", res)
	}
	// A terminal with --json does not prompt either.
	h = newHarness(t)
	h.deps.IsTerminal = func() bool { return true }
	h.deps.Stdin = strings.NewReader("https://typed/v1\n")
	res = h.run("setup", "--json")
	if res.code != 0 || h.remote.Called("PatchSettings") != 0 || res.err != "" {
		t.Errorf("%+v", res)
	}
}

func TestSetupInteractive(t *testing.T) {
	h := newHarness(t)
	h.remote.Tools.Ready, h.remote.Tools.MissingDownloadBytes = false, 500<<20
	h.deps.IsTerminal = func() bool { return true }
	h.deps.ReadPassword = func() (string, error) { return secret, nil }
	h.deps.Stdin = strings.NewReader("https://typed/v1\n\nn\n")
	res := h.run("setup")
	if res.code != 0 {
		t.Fatal(res)
	}
	got, _ := json.Marshal(h.remote.LastPatch)
	if string(got) != `{"llm":{"base_url":"https://typed/v1"}}` || h.remote.LastSecret != secret || h.remote.Called("PrepareTools") != 0 {
		t.Errorf("patch = %s secret set=%v prepare=%d", got, h.remote.SecretSet, h.remote.Called("PrepareTools"))
	}
	if !strings.Contains(res.err, "接口地址") || !strings.Contains(res.err, "[m1]") || strings.Contains(res.out, "接口地址 base_url") {
		t.Errorf("prompts belong on stderr: %+v", res)
	}
	assertNoSecret(t, res)
}

func TestEngineStatusAndStop(t *testing.T) {
	h := newHarness(t)
	res := h.run("engine", "status", "--json")
	v := oneJSON(t, res.out)
	if res.code != 0 || v["running"] != true || v["engine"].(map[string]any)["pid"] != float64(42) {
		t.Errorf("%+v", res)
	}
	if !h.conns[0].NoSpawn {
		t.Error("engine status must never spawn an engine")
	}

	h = newHarness(t)
	h.deps.Connect = clitest.Connector(nil, &h.conns)
	res = h.run("engine", "status", "--json")
	v = oneJSON(t, res.out)
	if res.code != 0 || v["running"] != false || v["engine"] != nil || v["data_dir"] != "/data" || !h.conns[0].NoSpawn {
		t.Errorf("not running: %+v", res)
	}
	if res = h.run("engine", "status"); res.code != 0 || !strings.Contains(res.out, "Engine 未运行") || res.err != "" {
		t.Errorf("human not running: %+v", res)
	}
	if res = h.run("engine", "stop", "--json"); res.code != 0 || oneJSON(t, res.out)["was_running"] != false {
		t.Errorf("stop when stopped: %+v", res)
	}

	h = newHarness(t)
	res = h.run("engine", "stop", "--force", "--json")
	if res.code != 0 || !h.remote.ShutdownCalled || !h.remote.LastForce || !h.conns[0].NoSpawn {
		t.Errorf("stop: %+v", res)
	}
	// A version mismatch is a real error, not "not running".
	h = newHarness(t)
	h.deps.Connect = func(context.Context, app.ConnectOptions) (app.Remote, error) {
		return nil, app.E(app.CodeEngineVersionMismatch, "版本不一致")
	}
	if res = h.run("engine", "status", "--json"); res.code != app.ExitEngine {
		t.Errorf("mismatch: %+v", res)
	}
}

func TestDoctor(t *testing.T) {
	statuses := func(v map[string]any) map[string]string {
		out := map[string]string{}
		for _, c := range v["checks"].([]any) {
			m := c.(map[string]any)
			out[m["name"].(string)] = m["status"].(string)
		}
		return out
	}
	h := newHarness(t)
	res := h.run("doctor", "--json")
	v := oneJSON(t, res.out)
	if res.code != 0 || v["ok"] != true {
		t.Fatalf("%+v", res)
	}
	if got := statuses(v); got["data_dir"] != "ok" || got["engine"] != "ok" || got["tools"] != "ok" || got["models"] != "ok" || got["llm"] != "ok" || got["desktop"] != "skipped" {
		t.Errorf("checks = %v", got)
	}
	if strings.Contains(res.out, "api_key\"") {
		t.Error("doctor must only report key state")
	}

	t.Run("llm not configured", func(t *testing.T) {
		h := newHarness(t)
		h.remote.Settings.LLM.APIKeyConfigured = false
		res := h.run("doctor", "--json")
		v := oneJSON(t, res.out)
		if res.code != app.ExitConfig || errCode(t, v) != "MODEL_NOT_CONFIGURED" || v["ok"] != false || statuses(v)["llm"] != "fail" {
			t.Errorf("%+v", res)
		}
		res = h.run("doctor")
		if res.code != app.ExitConfig || !strings.Contains(res.out, "✗ 在线模型") || !strings.Contains(res.err, "MODEL_NOT_CONFIGURED") {
			t.Errorf("human: %+v", res)
		}
	})
	t.Run("tools missing wins over llm", func(t *testing.T) {
		h := newHarness(t)
		h.remote.Settings.LLM.Model = ""
		h.remote.Tools.Ready = false
		h.remote.Tools.Items[1].Status = app.ToolMissing
		res := h.run("doctor", "--json")
		v := oneJSON(t, res.out)
		if got := statuses(v); res.code != app.ExitToolNotReady || got["models"] != "fail" || got["tools"] != "ok" || got["llm"] != "fail" {
			t.Errorf("%+v", res)
		}
	})
	t.Run("engine unavailable", func(t *testing.T) {
		h := newHarness(t)
		h.deps.Connect = clitest.Connector(nil, nil)
		res := h.run("doctor", "--json")
		v := oneJSON(t, res.out)
		if got := statuses(v); res.code != app.ExitEngine || got["engine"] != "fail" || got["tools"] != "skipped" || got["llm"] != "skipped" {
			t.Errorf("%+v", res)
		}
	})
	t.Run("desktop prerequisites", func(t *testing.T) {
		webkit := SystemPrereq{Name: "WebKitGTK 4.1", Package: "libwebkit2gtk-4.1-0", Found: true}
		libav := SystemPrereq{Name: "GStreamer libav 插件", Package: "gstreamer1.0-libav", Optional: true, Found: true}
		run := func(list []SystemPrereq, args ...string) (result, map[string]any) {
			h := newHarness(t)
			h.deps.RunDesktop = func(context.Context, string) error { return nil }
			h.deps.DesktopPrereqs = func() []SystemPrereq { return list }
			res := h.run(append([]string{"doctor"}, args...)...)
			if len(args) == 0 {
				return res, nil
			}
			v := oneJSON(t, res.out)
			for _, c := range v["checks"].([]any) {
				if m := c.(map[string]any); m["name"] == "desktop" {
					return res, m
				}
			}
			t.Fatalf("no desktop check: %+v", res)
			return res, nil
		}

		// Nothing to check (macOS) and everything found: ok either way.
		if res, ch := run(nil, "--json"); res.code != 0 || ch["status"] != "ok" || ch["details"] != nil {
			t.Errorf("no prerequisites: %+v", res)
		}
		if res, ch := run([]SystemPrereq{webkit, libav}, "--json"); res.code != 0 || ch["status"] != "ok" || len(ch["details"].(map[string]any)["prerequisites"].([]any)) != 2 {
			t.Errorf("all found: %+v", res)
		}

		// Decoders missing: a warning that names the package, exit 0.
		libav.Found = false
		res, ch := run([]SystemPrereq{webkit, libav}, "--json")
		if res.code != 0 || ch["status"] != "warn" || !strings.Contains(ch["hint"].(string), "apt install gstreamer1.0-libav") {
			t.Errorf("decoder missing: %+v", res)
		}
		if res, _ := run([]SystemPrereq{webkit, libav}); res.code != 0 || !strings.Contains(res.out, "! 桌面界面") || !strings.Contains(res.out, "sudo apt install gstreamer1.0-libav") {
			t.Errorf("decoder missing, human: %+v", res)
		}

		// WebView missing: the check fails and lists every missing package.
		webkit.Found = false
		res, ch = run([]SystemPrereq{webkit, libav}, "--json")
		if res.code != app.ExitToolNotReady || ch["status"] != "fail" || ch["code"] != "TOOL_MISSING" ||
			!strings.Contains(ch["hint"].(string), "apt install libwebkit2gtk-4.1-0 gstreamer1.0-libav") {
			t.Errorf("webview missing: %+v", res)
		}
	})
	t.Run("build mismatch is a warning", func(t *testing.T) {
		h := newHarness(t)
		h.remote.Engine.BuildID = "other"
		res := h.run("doctor", "--json")
		if v := oneJSON(t, res.out); res.code != 0 || statuses(v)["engine"] != "warn" {
			t.Errorf("%+v", res)
		}
	})
	t.Run("desktop webview", func(t *testing.T) {
		h := newHarness(t)
		h.deps.RunDesktop = func(context.Context, string) error { return nil }
		// The system ships the WebView (macOS): nothing to report.
		h.deps.WebView = func() (string, string, bool) { return "", "", false }
		res := h.run("doctor", "--json")
		if v := oneJSON(t, res.out); res.code != 0 || statuses(v)["desktop"] != "ok" {
			t.Errorf("system webview: %+v", res)
		}
		// A required runtime that is installed is named with its version.
		h.deps.WebView = func() (string, string, bool) { return "WebView2 运行时", "140.0.1.2", true }
		res = h.run("doctor")
		if res.code != 0 || !strings.Contains(res.out, "✓ 桌面界面") || !strings.Contains(res.out, "WebView2 运行时 140.0.1.2") {
			t.Errorf("installed: %+v", res)
		}
		// A missing one is a warning with an install hint, not a failure:
		// the CLI and MCP do not need it.
		h.deps.WebView = func() (string, string, bool) { return "WebView2 运行时", "", true }
		res = h.run("doctor", "--json")
		if v := oneJSON(t, res.out); res.code != 0 || v["ok"] != true || statuses(v)["desktop"] != "warn" {
			t.Errorf("missing: %+v", res)
		}
		if res = h.run("doctor"); res.code != 0 || !strings.Contains(res.out, "! 桌面界面") || !strings.Contains(res.out, "webview2") {
			t.Errorf("missing, human: %+v", res)
		}
	})
}

func TestLLMTestFailure(t *testing.T) {
	h := newHarness(t)
	h.remote.LLMTest = &app.LLMTestResult{OK: false, Error: app.E(app.CodeModelAuthFailed, "鉴权失败")}
	res := h.run("llm", "test", "--json")
	v := oneJSON(t, res.out)
	if res.code != app.ExitConfig || errCode(t, v) != "MODEL_AUTH_FAILED" || v["result"].(map[string]any)["ok"] != false {
		t.Errorf("%+v", res)
	}
}

func TestWeb(t *testing.T) {
	h := newHarness(t)
	var opened string
	h.deps.OpenBrowser = func(u string) error { opened = u; return nil }

	// The first call switches the optional Web UI on, then opens it.
	res := h.run("web", "--json")
	v := oneJSON(t, res.out)
	if res.code != 0 || opened != h.remote.URL || v["enabled"] != true || v["opened"] != true || v["url"] != h.remote.URL {
		t.Fatalf("%+v", res)
	}
	web, _ := h.remote.LastPatch["web"].(map[string]any)
	if web["enabled"] != true {
		t.Fatalf("patch = %v", h.remote.LastPatch)
	}

	// Already on: nothing is patched again.
	h.remote.Settings.Web.Enabled = true
	h.remote.LastPatch = nil
	if res = h.run("web"); res.code != 0 || h.remote.LastPatch != nil || !strings.Contains(res.out, h.remote.URL) {
		t.Fatalf("%+v patch=%v", res, h.remote.LastPatch)
	}

	// Without a browser the address is printed for the user to open.
	h.deps.OpenBrowser = func(string) error { return errors.New("no display") }
	res = h.run("web", "--json")
	if v := oneJSON(t, res.out); res.code != 0 || v["opened"] != false || v["url"] != h.remote.URL {
		t.Errorf("%+v", res)
	}
}

func TestProcessCommands(t *testing.T) {
	h := newHarness(t)
	var got []string
	h.deps.RunServe = func(_ context.Context, o ServeOptions) error {
		got = append(got, "serve", o.DataDir)
		if o.Port != 8080 || !o.Open {
			t.Errorf("serve options = %+v", o)
		}
		return nil
	}
	h.deps.RunEngine = func(_ context.Context, dir string) error { got = append(got, "engine", dir); return nil }
	h.deps.RunMCP = func(_ context.Context, o MCPOptions) error {
		got = append(got, "mcp", o.DataDir)
		if !o.NoSpawn {
			t.Errorf("mcp options = %+v", o)
		}
		return nil
	}
	for _, args := range [][]string{
		{"serve", "--port", "8080", "--open"},
		{"engine", "run", "--data-dir", "/e"},
		{"mcp", "--no-spawn"},
	} {
		if res := h.run(args...); res.code != 0 || res.out != "" {
			t.Errorf("%v: %+v", args, res)
		}
	}
	if want := []string{"serve", "/data", "engine", "/e", "mcp", "/data"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v", got)
	}
	if len(h.conns) != 0 {
		t.Error("process commands must not connect")
	}
	h.deps.RunEngine = func(context.Context, string) error { return errors.New("bind failed") }
	if res := h.run("engine", "run", "--json"); res.code != 1 || errCode(t, oneJSON(t, res.out)) != "INTERNAL" {
		t.Errorf("engine run failure: %+v", res)
	}
}

func TestDesktopAndBareCommand(t *testing.T) {
	// nogui: the bare command prints help, `desktop` is a clear error.
	h := newHarness(t)
	res := h.run()
	if res.code != 0 || !strings.Contains(res.out, "import") || !strings.Contains(res.out, "Usage") {
		t.Errorf("bare nogui: %+v", res)
	}
	if strings.Contains(res.out, "_guard") || strings.Contains(res.out, "completion") {
		t.Errorf("hidden commands in help: %s", res.out)
	}
	if res = h.run("desktop"); res.code != app.ExitUsage || !strings.Contains(res.err, "不包含桌面界面") {
		t.Errorf("desktop nogui: %+v", res)
	}
	var dirs []string
	h.deps.RunDesktop = func(_ context.Context, dir string) error { dirs = append(dirs, dir); return nil }
	h.run()
	h.run("desktop", "--data-dir", "/d")
	if !reflect.DeepEqual(dirs, []string{"/data", "/d"}) {
		t.Errorf("desktop dirs = %v", dirs)
	}
}

func TestGuardPassesRawArgs(t *testing.T) {
	h := newHarness(t)
	var got []string
	h.deps.Guard = func(args []string) int { got = args; return 37 }
	raw := []string{"_guard", "--json", "--pid=1", "--", "-x", "--help"}
	if res := h.run(raw...); res.code != 37 || res.out != "" || res.err != "" {
		t.Errorf("%+v", res)
	}
	if !reflect.DeepEqual(got, raw[1:]) {
		t.Errorf("guard args = %v", got)
	}
	// Through the cobra tree as well.
	root := New(h.deps)
	root.SetArgs(raw)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	err := root.Execute()
	var f *failure
	if !errors.As(err, &f) || f.exit != 37 || !reflect.DeepEqual(got, raw[1:]) {
		t.Errorf("cobra guard: %v %v", err, got)
	}
	h.deps.Guard = nil
	if res := h.run("_guard", "x"); res.code != app.ExitUsage {
		t.Errorf("nil guard: %+v", res)
	}
}

func TestEveryDocumentedCommandExists(t *testing.T) {
	root := New(Deps{})
	for _, path := range []string{
		"desktop", "serve", "web", "mcp", "setup", "import", "jobs list", "jobs get", "jobs cancel", "jobs retry",
		"videos list", "videos get", "note get", "transcript get", "export", "config show", "config get", "config set",
		"config set-data-dir", "config set-secret", "llm test", "tools status", "tools prepare", "tools update",
		"tools import", "tools import-model", "engine status", "engine stop", "engine run", "doctor", "version", "_guard",
	} {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil || cmd.CommandPath() != "watchink "+path {
			t.Errorf("command %q not found (%v)", path, err)
		}
	}
	for _, f := range []string{"data-dir", "json", "verbose", "no-spawn", "timeout"} {
		if root.PersistentFlags().Lookup(f) == nil {
			t.Errorf("global flag --%s missing", f)
		}
	}
}

func TestTableAlignsCJK(t *testing.T) {
	var b bytes.Buffer
	table(&b, []string{"标题", "状态"}, [][]string{{"线性代数", "就绪"}, {"abc", "ok"}})
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	col := func(s, sub string) int { return displayWidth(s[:strings.Index(s, sub)]) }
	if col(lines[0], "状态") != col(lines[1], "就绪") || col(lines[1], "就绪") != col(lines[2], "ok") {
		t.Errorf("misaligned:\n%s", b.String())
	}
	if got := truncate("线性代数第一讲", 8); displayWidth(got) > 8 {
		t.Errorf("truncate = %q", got)
	}
}
