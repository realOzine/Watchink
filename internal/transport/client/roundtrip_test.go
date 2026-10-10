package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"watchink/internal/app"
	"watchink/internal/transport/api"
	"watchink/internal/transport/api/apitest"
)

const rtToken = "roundtrip-token-0123456789"

type rig struct {
	fake  *apitest.Fake
	h     *api.Handler
	srv   *httptest.Server
	c     *Client
	stops chan bool
}

// newRig wires fake app.Local → api.NewHandler → httptest server → Client.
func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{fake: apitest.New(), stops: make(chan bool, 4)}
	r.h = api.NewHandler(api.Options{
		Service:           r.fake,
		Token:             rtToken,
		OnShutdown:        func(force bool) { r.stops <- force },
		HeartbeatInterval: 20 * time.Millisecond,
		EngineInfo:        func(info *app.EngineInfo) { info.PID = 77; info.BuildID = "rt-build"; info.API = app.APIVersion },
	})
	r.srv = httptest.NewServer(r.h)
	r.c = New(Options{BaseURL: r.srv.URL + "/", Token: rtToken, Requester: "cli", StreamIdleTimeout: 300 * time.Millisecond})
	t.Cleanup(func() {
		r.c.Close()
		r.h.Close()
		r.srv.CloseClientConnections()
		r.srv.Close()
	})
	return r
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func wantCode(t *testing.T, err error, code app.Code) *app.Error {
	t.Helper()
	var e *app.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
	return e
}

func ptr[T any](v T) *T { return &v }

func TestRemoteBasics(t *testing.T) {
	r := newRig(t)
	if r.c.BaseURL() != r.srv.URL || r.c.Token() != rtToken {
		t.Fatalf("BaseURL %q Token mismatch", r.c.BaseURL())
	}
	var remote app.Remote = NewWithToken(r.srv.URL, rtToken)
	defer remote.Close()
	r.fake.Info.LLMConfigured = true
	r.fake.AddJob(app.Job{Status: app.JobRunning})
	info, err := remote.EngineInfo(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	want := app.EngineInfo{BuildID: "rt-build", API: 1, PID: 77, DataDir: "/fake/data", ActiveJobs: 1, ToolsReady: true, LLMConfigured: true}
	if *info != want {
		t.Fatalf("EngineInfo = %+v", info)
	}

	// A wrong token decodes into UNAUTHORIZED.
	bad := NewWithToken(r.srv.URL, "wrong")
	_, err = bad.EngineInfo(ctxT(t))
	wantCode(t, err, app.CodeUnauthorized)

	// Shutdown.
	for _, force := range []bool{false, true} {
		if err := r.c.Shutdown(ctxT(t), force); err != nil {
			t.Fatal(err)
		}
		if got := <-r.stops; got != force {
			t.Fatalf("shutdown force = %v, want %v", got, force)
		}
	}
}

func TestImportAndUploadRoundTrip(t *testing.T) {
	r := newRig(t)
	ctx := ctxT(t)

	res, err := r.c.Import(ctx, app.ImportRequest{Source: app.ImportSource{Type: app.SourceFile, Path: "/Users/me/第 1 讲.mp4"}, Language: "zh"})
	if err != nil {
		t.Fatal(err)
	}
	if res.VideoID == "" || res.JobID == "" || res.Status != app.JobQueued || res.Deduplicated || res.NoteID != nil || res.Warnings == nil {
		t.Fatalf("Import = %+v", res)
	}
	call, _ := r.fake.LastCall("Import")
	got := call.Args[0].(app.ImportRequest)
	if got.RequestedBy != "cli" || got.Language != "zh" || got.Source.Path != "/Users/me/第 1 讲.mp4" {
		t.Fatalf("service saw %+v", got)
	}
	// An explicit requester is kept.
	if _, err := r.c.Import(ctx, app.ImportRequest{Source: app.ImportSource{Type: app.SourceURL, URL: "https://www.bilibili.com/video/BV1xx411c7mD?p=2"}, RequestedBy: "mcp", ReprocessFrom: "note"}); err != nil {
		t.Fatal(err)
	}
	call, _ = r.fake.LastCall("Import")
	if got := call.Args[0].(app.ImportRequest); got.RequestedBy != "mcp" || got.ReprocessFrom != "note" {
		t.Fatalf("service saw %+v", got)
	}
	_, err = r.c.Import(ctx, app.ImportRequest{Source: app.ImportSource{Type: "ftp"}})
	wantCode(t, err, app.CodeInvalidArgument)
	r.fake.SetError("Import", app.E(app.CodePathNotAllowed, "路径不在允许的根目录内").WithHint("修改 security.import_roots").WithDetail("path", "/etc"))
	_, err = r.c.Import(ctx, app.ImportRequest{Source: app.ImportSource{Type: app.SourceFile, Path: "/etc/x"}})
	e := wantCode(t, err, app.CodePathNotAllowed)
	if e.Hint != "修改 security.import_roots" || e.Details["path"] != "/etc" || !e.UserActionRequired || e.Retryable {
		t.Fatalf("decoded error = %+v", e)
	}
	r.fake.SetError("Import", nil)

	// Upload: known size, unknown size (chunked), empty.
	payload := bytes.Repeat([]byte("lecture-bytes-"), 100_000) // 1.4 MB
	up, err := r.c.CreateUpload(ctx, "课程/第 1 讲?.mp4", int64(len(payload)), bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := r.fake.UploadData(up.UploadID)
	if !bytes.Equal(stored, payload) || up.SizeBytes != int64(len(payload)) || up.Filename != "第 1 讲?.mp4" || up.ExpiresAt == "" {
		t.Fatalf("upload = %+v (stored %d bytes)", up, len(stored))
	}
	call, _ = r.fake.LastCall("CreateUpload")
	if call.Args[1] != int64(len(payload)) {
		t.Fatalf("size seen by service = %v", call.Args[1])
	}
	// io.MultiReader hides the length: the body must be streamed chunked.
	up, err = r.c.CreateUpload(ctx, "b.mkv", -1, io.MultiReader(bytes.NewReader(payload), strings.NewReader("tail")))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ = r.fake.UploadData(up.UploadID)
	call, _ = r.fake.LastCall("CreateUpload")
	if len(stored) != len(payload)+4 || call.Args[1] != int64(-1) {
		t.Fatalf("chunked upload: %d bytes, size arg %v", len(stored), call.Args[1])
	}
	up, err = r.c.CreateUpload(ctx, "empty.mp4", 0, strings.NewReader(""))
	if err != nil || up.SizeBytes != 0 {
		t.Fatalf("empty upload: %+v %v", up, err)
	}
	// The upload can be imported.
	if _, err := r.c.Import(ctx, app.ImportRequest{Source: app.ImportSource{Type: app.SourceUpload, UploadID: up.UploadID}}); err != nil {
		t.Fatal(err)
	}
	_, err = r.c.Import(ctx, app.ImportRequest{Source: app.ImportSource{Type: app.SourceUpload, UploadID: "upl_missing"}})
	wantCode(t, err, app.CodeNotFound)
	r.fake.SetError("CreateUpload", app.E(app.CodeDiskFull, "磁盘空间不足"))
	_, err = r.c.CreateUpload(ctx, "c.mp4", 3, strings.NewReader("abc"))
	wantCode(t, err, app.CodeDiskFull)
	_, err = r.c.CreateUpload(ctx, "", 3, strings.NewReader("abc"))
	wantCode(t, err, app.CodeInvalidArgument)
}

func TestJobsRoundTrip(t *testing.T) {
	r := newRig(t)
	ctx := ctxT(t)
	vid := "vid_1"
	var ids []string
	for i := 0; i < 5; i++ {
		status := app.JobQueued
		if i == 0 {
			status = app.JobFailed
		}
		j := r.fake.AddJob(app.Job{VideoID: &vid, Status: status, Stage: app.StageTranscribe, Progress: 0.42,
			Stages: []app.JobStage{{Stage: app.StageAcquire, Status: app.StageSucceeded, Progress: 1, Attempt: 1,
				Error: app.E(app.CodeNetworkError, "x").WithStage("acquire")}},
			Error: app.E(app.CodeASRFailed, "转写失败").WithStage("transcribe")})
		ids = append(ids, j.JobID)
	}
	other := "vid_2"
	r.fake.AddJob(app.Job{VideoID: &other})

	list, err := r.c.ListJobs(ctx, app.ListJobsRequest{VideoID: vid, Limit: 2})
	if err != nil || len(list.Jobs) != 2 || list.NextCursor == nil || list.Jobs[0].JobID != ids[4] {
		t.Fatalf("page 1 = %+v %v", list, err)
	}
	list2, err := r.c.ListJobs(ctx, app.ListJobsRequest{VideoID: vid, Limit: 2, Cursor: *list.NextCursor})
	if err != nil || len(list2.Jobs) != 2 || list2.Jobs[0].JobID != ids[2] {
		t.Fatalf("page 2 = %+v %v", list2, err)
	}
	list3, _ := r.c.ListJobs(ctx, app.ListJobsRequest{VideoID: vid, Limit: 2, Cursor: *list2.NextCursor})
	if len(list3.Jobs) != 1 || list3.NextCursor != nil {
		t.Fatalf("page 3 = %+v", list3)
	}
	failed, _ := r.c.ListJobs(ctx, app.ListJobsRequest{Status: app.JobFailed})
	if len(failed.Jobs) != 1 || failed.Jobs[0].JobID != ids[0] {
		t.Fatalf("status filter = %+v", failed)
	}
	call, _ := r.fake.LastCall("ListJobs")
	if req := call.Args[0].(app.ListJobsRequest); req.Limit != app.DefaultLimit || req.Status != app.JobFailed {
		t.Fatalf("service saw %+v", req)
	}
	_, err = r.c.ListJobs(ctx, app.ListJobsRequest{Cursor: "bogus"})
	wantCode(t, err, app.CodeInvalidArgument)

	// The full DTO survives, nested errors included.
	job, err := r.c.GetJob(ctx, app.GetJobRequest{JobID: ids[0]})
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != app.JobFailed || *job.VideoID != vid || job.Rev != 1 || job.Progress != 0.42 ||
		job.Error == nil || job.Error.Code != app.CodeASRFailed || job.Error.Stage != "transcribe" || !job.Error.Retryable ||
		len(job.Stages) != 1 || job.Stages[0].Error.Code != app.CodeNetworkError || !job.Terminal() {
		t.Fatalf("job = %+v", job)
	}
	_, err = r.c.GetJob(ctx, app.GetJobRequest{JobID: "job_missing"})
	wantCode(t, err, app.CodeNotFound)

	// Retry / cancel and their conflicts.
	retried, err := r.c.RetryJob(ctx, ids[0])
	if err != nil || retried.Status != app.JobQueued || retried.Attempt != 2 || retried.Rev != 2 || retried.Error != nil {
		t.Fatalf("retry = %+v %v", retried, err)
	}
	_, err = r.c.RetryJob(ctx, ids[0])
	wantCode(t, err, app.CodeConflict)
	cancelled, err := r.c.CancelJob(ctx, ids[1])
	if err != nil || cancelled.Status != app.JobCancelled {
		t.Fatalf("cancel = %+v %v", cancelled, err)
	}
	_, err = r.c.CancelJob(ctx, ids[1])
	wantCode(t, err, app.CodeConflict)
	_, err = r.c.CancelJob(ctx, "job_missing")
	wantCode(t, err, app.CodeNotFound)
	_, err = r.c.RetryJob(ctx, "job/../x")
	wantCode(t, err, app.CodeNotFound)
}

func TestGetJobLongPoll(t *testing.T) {
	r := newRig(t)
	ctx := ctxT(t)
	job := r.fake.AddJob(app.Job{Status: app.JobRunning})

	// rev already ahead: returns at once.
	began := time.Now()
	got, err := r.c.GetJob(ctx, app.GetJobRequest{JobID: job.JobID, WaitSeconds: 30, AfterRev: 0})
	if err != nil || got.Rev != 1 || time.Since(began) > 2*time.Second {
		t.Fatalf("immediate: %+v %v after %v", got, err, time.Since(began))
	}

	// Blocks until the rev changes.
	go func() {
		time.Sleep(150 * time.Millisecond)
		r.fake.UpdateJob(job.JobID, func(j *app.Job) { j.Progress = 0.9 })
	}()
	began = time.Now()
	got, err = r.c.GetJob(ctx, app.GetJobRequest{JobID: job.JobID, WaitSeconds: 30, AfterRev: 1})
	if err != nil || got.Rev != 2 || got.Progress != 0.9 {
		t.Fatalf("after change: %+v %v", got, err)
	}
	if d := time.Since(began); d < 100*time.Millisecond || d > 5*time.Second {
		t.Fatalf("long poll returned after %v", d)
	}
	call, _ := r.fake.LastCall("GetJob")
	if req := call.Args[0].(app.GetJobRequest); req.WaitSeconds != 30 || req.AfterRev != 1 {
		t.Fatalf("service saw %+v", req)
	}

	// Times out with the unchanged job.
	began = time.Now()
	got, err = r.c.GetJob(ctx, app.GetJobRequest{JobID: job.JobID, WaitSeconds: 1, AfterRev: 2})
	if err != nil || got.Rev != 2 {
		t.Fatalf("timeout: %+v %v", got, err)
	}
	if d := time.Since(began); d < 900*time.Millisecond || d > 5*time.Second {
		t.Fatalf("1 s long poll took %v", d)
	}

	// The caller's own cancellation is reported as such, not as an engine error.
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err = r.c.GetJob(cctx, app.GetJobRequest{JobID: job.JobID, WaitSeconds: 30, AfterRev: 2})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled long poll: %v", err)
	}
}

func TestVideosNotesTranscriptsRoundTrip(t *testing.T) {
	r := newRig(t)
	ctx := ctxT(t)
	for _, title := range []string{"线性代数 第1讲", "线性代数 第2讲", "Operating Systems"} {
		r.fake.AddVideo(app.Video{Title: title, Status: app.VideoReady})
	}
	v := r.fake.AddVideo(app.Video{
		VideoID: "vid_full", Title: "完整", Status: app.VideoReady,
		Source:     app.VideoSource{Type: app.SourceYouTube, URI: "https://www.youtube.com/watch?v=x", PlatformID: "x"},
		DurationMs: ptr(int64(3753000)), Language: ptr("zh"), LatestJobID: ptr("job_1"),
		TranscriptID: ptr("tr_1"), NoteID: ptr("note_1"), PlaybackReady: true, WebURL: "http://127.0.0.1:1/videos/vid_full",
	})

	got, err := r.c.GetVideo(ctx, "vid_full")
	if err != nil || !reflect.DeepEqual(*got, v) {
		t.Fatalf("GetVideo = %+v %v\nwant %+v", got, err, v)
	}
	_, err = r.c.GetVideo(ctx, "vid_missing")
	wantCode(t, err, app.CodeNotFound)

	list, err := r.c.ListVideos(ctx, app.ListVideosRequest{TitleContains: "线性代数", Limit: 1})
	if err != nil || len(list.Videos) != 1 || list.Videos[0].Title != "线性代数 第2讲" || list.NextCursor == nil {
		t.Fatalf("ListVideos = %+v %v", list, err)
	}
	list, err = r.c.ListVideos(ctx, app.ListVideosRequest{TitleContains: "线性代数", Limit: 1, Cursor: *list.NextCursor})
	if err != nil || len(list.Videos) != 1 || list.Videos[0].Title != "线性代数 第1讲" || list.NextCursor != nil {
		t.Fatalf("ListVideos page 2 = %+v %v", list, err)
	}
	all, _ := r.c.ListVideos(ctx, app.ListVideosRequest{})
	if len(all.Videos) != 4 {
		t.Fatalf("all videos = %d", len(all.Videos))
	}

	// Note: paging in code points, both formats, a historical note ID.
	content := strings.Repeat("笔记[1](wk://seg/seg_x) ", 10)
	r.fake.Notes["vid_full"] = &app.Note{
		NoteID: "note_1", VideoID: "vid_full", TranscriptID: "tr_1", Version: 2, Title: "完整", Content: content,
		Citations: []app.Citation{{RefNo: 1, PointID: "p1", SegmentID: "seg_x", StartMs: 1000, EndMs: 2000, Text: "引文"}},
		Coverage:  app.Coverage{Chunks: 3, Points: 9, Citations: 12, OverviewFallback: true},
		LLMModel:  "m", LLMProvider: "openai-compatible", PromptVersion: "v1", CreatedAt: app.Now(),
	}
	note, err := r.c.GetNote(ctx, app.GetNoteRequest{VideoID: "vid_full"})
	if err != nil || note.Content != content || note.Format != app.FormatMarkdown || note.NextOffset != nil ||
		note.TotalChars != len([]rune(content)) || note.Citations[0].Text != "引文" || !note.Coverage.OverviewFallback || note.Version != 2 {
		t.Fatalf("GetNote = %+v %v", note, err)
	}
	note, err = r.c.GetNote(ctx, app.GetNoteRequest{VideoID: "vid_full", Format: app.FormatJSON, Offset: 2, MaxChars: 5, NoteID: "note_0"})
	if err != nil || note.Content != string([]rune(content)[2:7]) || *note.NextOffset != 7 || note.Format != app.FormatJSON {
		t.Fatalf("paged note = %+v %v", note, err)
	}
	call, _ := r.fake.LastCall("GetNote")
	if req := call.Args[0].(app.GetNoteRequest); req != (app.GetNoteRequest{VideoID: "vid_full", Format: "json", Offset: 2, MaxChars: 5, NoteID: "note_0"}) {
		t.Fatalf("service saw %+v", req)
	}
	_, err = r.c.GetNote(ctx, app.GetNoteRequest{VideoID: "vid_missing"})
	wantCode(t, err, app.CodeNotFound)
	_, err = r.c.GetNote(ctx, app.GetNoteRequest{VideoID: "vid_full", Format: "html"})
	wantCode(t, err, app.CodeInvalidArgument)

	// Transcript: ranges, IDs, formats.
	tr := &app.Transcript{TranscriptID: "tr_1", VideoID: "vid_full", Language: "zh", Engine: "whispercpp", Model: "small", SegmentCount: 3,
		Truncated: true, NextStartMs: ptr(int64(3000))}
	for i, text := range []string{"第一句", "第二句", "第三句"} {
		tr.Segments = append(tr.Segments, app.Segment{SegmentID: app.SegmentID("tr_01JABCDEFGHJKMNPQRSTVWXYZ0", i+1), Idx: i + 1,
			StartMs: int64(i) * 1000, EndMs: int64(i+1) * 1000, Text: text})
	}
	r.fake.Transcripts["vid_full"] = tr
	full, err := r.c.GetTranscript(ctx, app.GetTranscriptRequest{VideoID: "vid_full"})
	if err != nil || len(full.Segments) != 3 || full.Format != app.FormatJSON || full.Content != "" ||
		!full.Truncated || *full.NextStartMs != 3000 || full.Engine != "whispercpp" {
		t.Fatalf("GetTranscript = %+v %v", full, err)
	}
	ranged, err := r.c.GetTranscript(ctx, app.GetTranscriptRequest{VideoID: "vid_full", StartMs: ptr(int64(1000)), EndMs: ptr(int64(2000)), MaxChars: -1})
	if err != nil || len(ranged.Segments) != 1 || ranged.Segments[0].Text != "第二句" {
		t.Fatalf("ranged = %+v %v", ranged, err)
	}
	call, _ = r.fake.LastCall("GetTranscript")
	if req := call.Args[0].(app.GetTranscriptRequest); *req.StartMs != 1000 || *req.EndMs != 2000 || req.MaxChars != -1 {
		t.Fatalf("service saw %+v", req)
	}
	// start_ms = 0 is a value, not "absent".
	if _, err := r.c.GetTranscript(ctx, app.GetTranscriptRequest{VideoID: "vid_full", StartMs: ptr(int64(0))}); err != nil {
		t.Fatal(err)
	}
	call, _ = r.fake.LastCall("GetTranscript")
	if req := call.Args[0].(app.GetTranscriptRequest); req.StartMs == nil || *req.StartMs != 0 || req.EndMs != nil {
		t.Fatalf("service saw %+v", req)
	}
	byID, err := r.c.GetTranscript(ctx, app.GetTranscriptRequest{VideoID: "vid_full",
		SegmentIDs: []string{tr.Segments[2].SegmentID, tr.Segments[0].SegmentID}, Format: app.FormatSRT})
	if err != nil || len(byID.Segments) != 2 || byID.Format != app.FormatSRT || byID.Content != "SRT\n第一句\n第三句\n" || !byID.Truncated {
		t.Fatalf("by ID = %+v %v", byID, err)
	}
	for _, f := range []string{app.FormatVTT, app.FormatTXT} {
		got, err := r.c.GetTranscript(ctx, app.GetTranscriptRequest{VideoID: "vid_full", Format: f})
		if err != nil || !strings.HasPrefix(got.Content, strings.ToUpper(f)+"\n第一句") || got.TranscriptID != "tr_1" {
			t.Fatalf("%s = %+v %v", f, got, err)
		}
	}
	ids50 := make([]string, 50)
	for i := range ids50 {
		ids50[i] = app.SegmentID("tr_01JABCDEFGHJKMNPQRSTVWXYZ0", i+1)
	}
	if _, err := r.c.GetTranscript(ctx, app.GetTranscriptRequest{VideoID: "vid_full", SegmentIDs: ids50}); err != nil {
		t.Fatalf("50 ids: %v", err)
	}
	n := r.fake.CallCount("GetTranscript")
	_, err = r.c.GetTranscript(ctx, app.GetTranscriptRequest{VideoID: "vid_full", SegmentIDs: append(ids50, "seg_extra")})
	wantCode(t, err, app.CodeInvalidArgument)
	if r.fake.CallCount("GetTranscript") != n {
		t.Fatal("51 segment IDs reached the service")
	}
	_, err = r.c.GetTranscript(ctx, app.GetTranscriptRequest{VideoID: "vid_missing"})
	wantCode(t, err, app.CodeNotFound)
	_, err = r.c.GetTranscript(ctx, app.GetTranscriptRequest{VideoID: "vid_full", Format: "pdf"})
	wantCode(t, err, app.CodeInvalidArgument)
}

func TestPlaybackAndMediaRange(t *testing.T) {
	r := newRig(t)
	ctx := ctxT(t)
	media := make([]byte, 100_000)
	for i := range media {
		media[i] = byte(i * 7)
	}
	r.fake.PlaybackData["vid_1"] = media

	pb, err := r.c.GetPlayback(ctx, "vid_1")
	if err != nil || pb.Mime != "video/mp4" || pb.ExpiresAt == "" {
		t.Fatalf("GetPlayback = %+v %v", pb, err)
	}
	_, err = r.c.GetPlayback(ctx, "vid_missing")
	wantCode(t, err, app.CodeNotFound)

	// The URL works without any Authorization header, like a <video> element.
	get := func(headers ...string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", r.srv.URL+pb.URL, nil)
		for _, h := range headers {
			k, v, _ := strings.Cut(h, ": ")
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	resp, body := get()
	if resp.StatusCode != 200 || !bytes.Equal(body, media) || resp.Header.Get("Accept-Ranges") != "bytes" ||
		resp.Header.Get("Content-Type") != "video/mp4" || resp.Header.Get("Last-Modified") == "" {
		t.Fatalf("full: %d %v", resp.StatusCode, resp.Header)
	}
	lastMod := resp.Header.Get("Last-Modified")

	cases := []struct {
		rng          string
		status       int
		from, to     int
		contentRange string
	}{
		{"bytes=0-99", 206, 0, 100, "bytes 0-99/100000"},
		{"bytes=50000-50009", 206, 50000, 50010, "bytes 50000-50009/100000"},
		{"bytes=99990-", 206, 99990, 100000, "bytes 99990-99999/100000"},
		{"bytes=-10", 206, 99990, 100000, "bytes 99990-99999/100000"},
		{"bytes=99990-200000", 206, 99990, 100000, "bytes 99990-99999/100000"},
	}
	for _, c := range cases {
		resp, body := get("Range: " + c.rng)
		if resp.StatusCode != c.status || !bytes.Equal(body, media[c.from:c.to]) || resp.Header.Get("Content-Range") != c.contentRange {
			t.Errorf("%s: %d %q (%d bytes)", c.rng, resp.StatusCode, resp.Header.Get("Content-Range"), len(body))
		}
	}
	if resp, _ := get("Range: bytes=200000-"); resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("unsatisfiable range: %d", resp.StatusCode)
	}
	// Conditional requests.
	if resp, body := get("If-Modified-Since: " + lastMod); resp.StatusCode != http.StatusNotModified || len(body) != 0 {
		t.Errorf("If-Modified-Since: %d", resp.StatusCode)
	}
	if resp, body := get("Range: bytes=0-9", "If-Range: "+lastMod); resp.StatusCode != 206 || len(body) != 10 {
		t.Errorf("If-Range (fresh): %d", resp.StatusCode)
	}
	if resp, body := get("Range: bytes=0-9", "If-Range: Mon, 01 Jan 2001 00:00:00 GMT"); resp.StatusCode != 200 || len(body) != len(media) {
		t.Errorf("If-Range (stale): %d", resp.StatusCode)
	}
	// HEAD.
	req, _ := http.NewRequest("HEAD", r.srv.URL+pb.URL, nil)
	head, err := http.DefaultClient.Do(req)
	if err != nil || head.StatusCode != 200 || head.ContentLength != int64(len(media)) {
		t.Fatalf("HEAD: %v %v", head, err)
	}
	head.Body.Close()
}

func TestExportRoundTrip(t *testing.T) {
	r := newRig(t)
	ctx := ctxT(t)
	r.fake.AddVideo(app.Video{VideoID: "vid_1", Title: "第1讲：导论"})

	exp, err := r.c.CreateExport(ctx, app.ExportRequest{VideoID: "vid_1", Format: app.FormatMDRaw})
	if err != nil {
		t.Fatal(err)
	}
	if exp.ExportID == "" || exp.VideoID != "vid_1" || exp.Format != app.FormatMDRaw || exp.Filename != "第1讲：导论.md" ||
		exp.Bytes == 0 || len(exp.SHA256) != 64 || exp.Path == "" || !strings.HasSuffix(exp.DownloadURL, "/download") {
		t.Fatalf("CreateExport = %+v", exp)
	}
	_, err = r.c.CreateExport(ctx, app.ExportRequest{VideoID: "vid_1", Format: "pdf"})
	wantCode(t, err, app.CodeInvalidArgument)
	_, err = r.c.CreateExport(ctx, app.ExportRequest{VideoID: "vid_missing", Format: app.FormatMD})
	wantCode(t, err, app.CodeNotFound)

	rc, meta, err := r.c.OpenExport(ctx, exp.ExportID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	if err != nil || rc.Close() != nil {
		t.Fatalf("read export: %v", err)
	}
	sum := sha256.Sum256(data)
	if *meta != *exp || hex.EncodeToString(sum[:]) != exp.SHA256 || int64(len(data)) != exp.Bytes {
		t.Fatalf("OpenExport meta = %+v, %d bytes", meta, len(data))
	}

	// A large export is streamed; closing early is fine.
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<17) // 2 MiB
	bigExp := r.fake.AddExport("vid_1", app.FormatSRT, "big.srt", big)
	rc, meta, err = r.c.OpenExport(ctx, bigExp.ExportID)
	if err != nil || meta.Bytes != int64(len(big)) {
		t.Fatalf("big export: %+v %v", meta, err)
	}
	head := make([]byte, 4096)
	if _, err := io.ReadFull(rc, head); err != nil || !bytes.Equal(head, big[:4096]) {
		t.Fatalf("partial read: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	rc, _, err = r.c.OpenExport(ctx, bigExp.ExportID)
	if err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(all, big) {
		t.Fatalf("full big export: %d bytes, %v", len(all), err)
	}

	_, _, err = r.c.OpenExport(ctx, "exp_missing")
	wantCode(t, err, app.CodeNotFound)
}

func TestSettingsSecretsLLMAndToolsRoundTrip(t *testing.T) {
	r := newRig(t)
	ctx := ctxT(t)
	r.fake.Settings.DataDir = "/fake/data"
	r.fake.Settings.Download.Mirrors = map[string]string{"huggingface.co": "https://hf-mirror.com"}

	s, err := r.c.GetSettings(ctx)
	if err != nil || !reflect.DeepEqual(*s, r.fake.Settings) {
		t.Fatalf("GetSettings = %+v %v", s, err)
	}
	s, err = r.c.PatchSettings(ctx, map[string]any{"llm": map[string]any{"model": "gpt-x", "base_url": "https://api.example/v1"}})
	if err != nil || s.LLM.Model != "gpt-x" || s.LLM.BaseURL != "https://api.example/v1" {
		t.Fatalf("PatchSettings = %+v %v", s, err)
	}
	call, _ := r.fake.LastCall("PatchSettings")
	if got := call.Args[0].(map[string]any)["llm"].(map[string]any)["model"]; got != "gpt-x" {
		t.Fatalf("patch seen by service: %v", call.Args[0])
	}
	_, err = r.c.PatchSettings(ctx, map[string]any{"llm": map[string]any{"nope": 1}})
	wantCode(t, err, app.CodeInvalidArgument)
	if _, err := r.c.PatchSettings(ctx, nil); err != nil {
		t.Fatalf("empty patch: %v", err)
	}

	// Secrets are write-only: set, then delete with an empty value.
	if err := r.c.SetSecret(ctx, app.SecretLLMAPIKey, "sk-secret-value-123456"); err != nil {
		t.Fatal(err)
	}
	if v, ok := r.fake.Secret(app.SecretLLMAPIKey); !ok || v != "sk-secret-value-123456" {
		t.Fatalf("stored secret = %q %v", v, ok)
	}
	if s, _ := r.c.GetSettings(ctx); !s.LLM.APIKeyConfigured {
		t.Fatal("api_key_configured not reflected")
	}
	if err := r.c.SetSecret(ctx, app.SecretLLMAPIKey, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.fake.Secret(app.SecretLLMAPIKey); ok {
		t.Fatal("secret not deleted")
	}
	wantCode(t, r.c.SetSecret(ctx, "other_secret", "x"), app.CodeInvalidArgument)

	// LLM test: a result with an embedded error, and a call-level error.
	r.fake.LLMTest = app.LLMTestResult{OK: false, LatencyMs: 12, Error: app.E(app.CodeModelAuthFailed, "鉴权失败")}
	res, err := r.c.TestLLM(ctx)
	if err != nil || res.OK || res.LatencyMs != 12 || res.Error == nil || res.Error.Code != app.CodeModelAuthFailed {
		t.Fatalf("TestLLM = %+v %v", res, err)
	}
	r.fake.SetError("TestLLM", app.E(app.CodeModelNotConfigured, "未配置模型"))
	_, err = r.c.TestLLM(ctx)
	if e := wantCode(t, err, app.CodeModelNotConfigured); !e.Retryable || !e.UserActionRequired {
		t.Fatalf("flags lost: %+v", e)
	}

	// Model listing: overrides travel in the body, errors keep their code.
	r.fake.LLMModels = []string{"m-large", "m-small"}
	ml, err := r.c.ListLLMModels(ctx, app.LLMModelsRequest{BaseURL: "https://api.example.com/v1", APIKey: "sk-typed-not-saved"})
	if err != nil || len(ml.Models) != 2 || ml.BaseURL != "https://api.example.com/v1" {
		t.Fatalf("ListLLMModels = %+v %v", ml, err)
	}
	r.fake.SetError("ListLLMModels", app.E(app.CodeModelAuthFailed, "鉴权失败"))
	_, err = r.c.ListLLMModels(ctx, app.LLMModelsRequest{})
	wantCode(t, err, app.CodeModelAuthFailed)

	// Tools.
	r.fake.Tools = app.ToolsStatus{
		Items: []app.ToolItem{
			{Name: "yt-dlp", Kind: app.ToolKindTool, Version: "2026.01.01", Status: app.ToolReady, Lazy: true, SizeBytes: 3 << 20},
			{Name: "whisper-small-q5_1", Kind: app.ToolKindModel, Status: app.ToolMissing, Required: true, DownloadBytes: 190 << 20},
		},
		Ready: false, MissingDownloadBytes: 190 << 20, DefaultASRModel: "whisper-small-q5_1",
	}
	tools, err := r.c.GetTools(ctx)
	if err != nil || !reflect.DeepEqual(*tools, r.fake.Tools) {
		t.Fatalf("GetTools = %+v %v", tools, err)
	}
	job, err := r.c.PrepareTools(ctx, app.PrepareToolsRequest{Items: []string{"whisper-small-q5_1"}})
	if err != nil || job.Kind != app.JobKindPrepareTools || job.VideoID != nil {
		t.Fatalf("PrepareTools = %+v %v", job, err)
	}
	call, _ = r.fake.LastCall("PrepareTools")
	if got := call.Args[0].(app.PrepareToolsRequest).Items; len(got) != 1 || got[0] != "whisper-small-q5_1" {
		t.Fatalf("items = %v", got)
	}
	again, err := r.c.PrepareTools(ctx, app.PrepareToolsRequest{})
	if err != nil || again.JobID != job.JobID {
		t.Fatalf("second PrepareTools should return the active job: %+v %v", again, err)
	}
	r.fake.SetError("PrepareTools", app.E(app.CodeNetworkError, "下载失败"))
	_, err = r.c.PrepareTools(ctx, app.PrepareToolsRequest{})
	wantCode(t, err, app.CodeNetworkError)

	tools, err = r.c.UpdateTool(ctx, "yt-dlp")
	if err != nil || tools.Items[0].Version != "updated" {
		t.Fatalf("UpdateTool = %+v %v", tools, err)
	}
	_, err = r.c.UpdateTool(ctx, "nope")
	wantCode(t, err, app.CodeNotFound)
	tools, err = r.c.ImportTool(ctx, app.ToolImportRequest{Name: "whisper-small-q5_1", Path: "/Users/me/ggml-small-q5_1.bin"})
	if err != nil || tools.Items[1].Status != app.ToolReady || tools.Items[1].Path != "/Users/me/ggml-small-q5_1.bin" {
		t.Fatalf("ImportTool = %+v %v", tools, err)
	}
	r.fake.SetError("ImportTool", app.E(app.CodeChecksumMismatch, "校验和不匹配"))
	_, err = r.c.ImportTool(ctx, app.ToolImportRequest{Name: "whisper-small-q5_1", Path: "/x"})
	wantCode(t, err, app.CodeChecksumMismatch)
	r.fake.SetError("GetTools", app.E(app.CodeInternal, "boom"))
	_, err = r.c.GetTools(ctx)
	wantCode(t, err, app.CodeInternal)
	r.fake.SetError("GetSettings", app.E(app.CodeInternal, "boom"))
	_, err = r.c.GetSettings(ctx)
	wantCode(t, err, app.CodeInternal)
}

func TestEveryErrorCodeSurvivesTheWire(t *testing.T) {
	r := newRig(t)
	for _, code := range append(app.KnownCodes(), app.CodeUnauthorized) {
		sent := app.E(code, "消息 %s", code).WithHint("提示").WithStage(app.StageProbe).WithDetail("n", float64(3))
		r.fake.SetError("GetVideo", sent)
		_, err := r.c.GetVideo(ctxT(t), "vid_1")
		got := wantCode(t, err, code)
		if got.Message != sent.Message || got.Hint != "提示" || got.Stage != app.StageProbe || got.Details["n"] != float64(3) ||
			got.Retryable != sent.Retryable || got.UserActionRequired != sent.UserActionRequired {
			t.Fatalf("%s: got %+v", code, got)
		}
	}
}

func TestConnectionFailuresAreEngineUnavailable(t *testing.T) {
	r := newRig(t)
	url := r.srv.URL
	r.srv.CloseClientConnections()
	r.srv.Close()
	c := New(Options{BaseURL: url, Token: rtToken})
	ctx := ctxT(t)

	_, err := c.EngineInfo(ctx)
	if e := wantCode(t, err, app.CodeEngineUnavailable); !e.Retryable || errors.Unwrap(e) == nil {
		t.Fatalf("error = %+v", e)
	}
	_, err = c.CreateUpload(ctx, "a.mp4", 3, strings.NewReader("abc"))
	wantCode(t, err, app.CodeEngineUnavailable)
	_, _, err = c.OpenExport(ctx, "exp_1")
	wantCode(t, err, app.CodeEngineUnavailable)
	_, _, err = c.Subscribe(ctx, 0)
	wantCode(t, err, app.CodeEngineUnavailable)
	wantCode(t, c.Shutdown(ctx, false), app.CodeEngineUnavailable)

	// Something that is not an engine at all.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nginx says no", http.StatusBadGateway)
	}))
	defer other.Close()
	_, err = NewWithToken(other.URL, "x").GetTools(ctx)
	wantCode(t, err, app.CodeInternal)
}
