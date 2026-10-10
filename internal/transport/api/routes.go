package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"watchink/internal/app"
)

// maxJSONBody bounds JSON request bodies; uploads are streamed and not
// subject to it.
const maxJSONBody = 1 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the {"error":{…}} envelope with the status that
// the contract assigns to the code.
func writeError(w http.ResponseWriter, err error) {
	e := app.AsError(err)
	writeJSON(w, app.HTTPStatus(e.Code), app.ErrorEnvelope{Error: e})
}

func invalid(format string, args ...any) *app.Error {
	return app.E(app.CodeInvalidArgument, format, args...)
}

// decodeBody parses a JSON object body. Unknown fields are rejected so that a
// misspelled field is not silently ignored.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) && allowEmpty {
			return nil
		}
		return invalid("请求体不是合法的 JSON：%v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("请求体只能包含一个 JSON 对象")
	}
	return nil
}

// params reads typed query parameters and remembers the first error.
type params struct {
	q   url.Values
	err *app.Error
}

func (p *params) fail(name, why string) {
	if p.err == nil {
		p.err = invalid("参数 %s %s", name, why).WithDetail("param", name)
	}
}

func (p *params) str(name string) string { return p.q.Get(name) }

func (p *params) has(name string) bool { return p.q.Get(name) != "" }

func (p *params) int(name string, def int) int {
	s := p.q.Get(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		p.fail(name, "必须是整数")
		return def
	}
	return n
}

func (p *params) int64(name string, def int64) int64 {
	s := p.q.Get(name)
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		p.fail(name, "必须是整数")
		return def
	}
	return n
}

func (p *params) int64Ptr(name string) *int64 {
	if !p.has(name) {
		return nil
	}
	n := p.int64(name, 0)
	if n < 0 {
		p.fail(name, "不能为负数")
	}
	return &n
}

// limit applies the paging rule: default 20, at most 100.
func (p *params) limit() int {
	n := p.int("limit", app.DefaultLimit)
	if n < 1 {
		p.fail("limit", "必须大于 0")
		return app.DefaultLimit
	}
	return min(n, app.MaxLimit)
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) engineInfo(w http.ResponseWriter, r *http.Request) {
	info, err := h.svc.EngineInfo(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if info == nil {
		info = &app.EngineInfo{}
	}
	out := *info
	if h.opts.EngineInfo != nil {
		h.opts.EngineInfo(&out)
	}
	writeJSON(w, http.StatusOK, &out)
}

func (h *Handler) engineShutdown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Force bool `json:"force"`
	}
	if err := decodeBody(w, r, &req, true); err != nil {
		writeError(w, err)
		return
	}
	if h.opts.OnShutdown == nil {
		writeError(w, app.E(app.CodeInternal, "此 Engine 不支持远程关闭"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "stopping", "force": req.Force})
	_ = http.NewResponseController(w).Flush()
	h.opts.OnShutdown(req.Force)
}

func (h *Handler) createImport(w http.ResponseWriter, r *http.Request) {
	var req app.ImportRequest
	if err := decodeBody(w, r, &req, false); err != nil {
		writeError(w, err)
		return
	}
	res, err := h.svc.Import(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// createUpload hands the request body to the service as a stream: uploads
// are multi-gigabyte videos and must never be buffered in memory.
func (h *Handler) createUpload(w http.ResponseWriter, r *http.Request) {
	raw := r.Header.Get(HeaderFilename)
	name, err := url.PathUnescape(raw)
	if err != nil {
		writeError(w, invalid("X-Filename 头不是合法的百分号编码"))
		return
	}
	// Keep only the last path element; browsers on Windows may send a path.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		writeError(w, invalid("缺少 X-Filename 头（上传的文件名）"))
		return
	}
	up, err := h.svc.CreateUpload(r.Context(), name, r.ContentLength, r.Body)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, up)
}

func (h *Handler) listJobs(w http.ResponseWriter, r *http.Request) {
	p := &params{q: r.URL.Query()}
	req := app.ListJobsRequest{
		Status:  p.str("status"),
		VideoID: p.str("video_id"),
		Limit:   p.limit(),
		Cursor:  p.str("cursor"),
	}
	if p.err != nil {
		writeError(w, p.err)
		return
	}
	res, err := h.svc.ListJobs(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) getJob(w http.ResponseWriter, r *http.Request) {
	p := &params{q: r.URL.Query()}
	req := app.GetJobRequest{
		JobID:       r.PathValue("job_id"),
		WaitSeconds: p.int("wait_seconds", 0),
		AfterRev:    p.int64("after_rev", 0),
	}
	if req.WaitSeconds < 0 {
		p.fail("wait_seconds", "不能为负数")
	}
	if p.err != nil {
		writeError(w, p.err)
		return
	}
	req.WaitSeconds = min(req.WaitSeconds, app.MaxWaitSeconds)
	ctx := r.Context()
	if req.WaitSeconds > 0 {
		var cancel func()
		ctx, cancel = h.withBase(ctx)
		defer cancel()
	}
	job, err := h.svc.GetJob(ctx, req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (h *Handler) cancelJob(w http.ResponseWriter, r *http.Request) {
	job, err := h.svc.CancelJob(r.Context(), r.PathValue("job_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (h *Handler) retryJob(w http.ResponseWriter, r *http.Request) {
	job, err := h.svc.RetryJob(r.Context(), r.PathValue("job_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (h *Handler) listVideos(w http.ResponseWriter, r *http.Request) {
	p := &params{q: r.URL.Query()}
	req := app.ListVideosRequest{
		Limit:         p.limit(),
		Cursor:        p.str("cursor"),
		TitleContains: p.str("title_contains"),
	}
	if p.err != nil {
		writeError(w, p.err)
		return
	}
	res, err := h.svc.ListVideos(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) getVideo(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.GetVideo(r.Context(), r.PathValue("video_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) getNote(w http.ResponseWriter, r *http.Request) {
	p := &params{q: r.URL.Query()}
	req := app.GetNoteRequest{
		VideoID:  r.PathValue("video_id"),
		Format:   p.str("format"),
		Offset:   p.int("offset", 0),
		MaxChars: p.int("max_chars", 0),
		NoteID:   p.str("note_id"),
	}
	if req.Offset < 0 {
		p.fail("offset", "不能为负数")
	}
	switch req.Format {
	case "", app.FormatMarkdown, app.FormatJSON:
	default:
		p.fail("format", "只能是 markdown 或 json")
	}
	if p.err != nil {
		writeError(w, p.err)
		return
	}
	note, err := h.svc.GetNote(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, note)
}

// transcriptTypes maps the text transcript formats to their media types.
var transcriptTypes = map[string]string{
	app.FormatSRT: "application/x-subrip; charset=utf-8",
	app.FormatVTT: "text/vtt; charset=utf-8",
	app.FormatTXT: "text/plain; charset=utf-8",
}

// getTranscript returns the app.Transcript JSON for format=json. For srt /
// vtt / txt it returns the rendered text as the body, unless the caller asks
// for `Accept: application/json` (internal/transport/client does, to keep the paging
// metadata), in which case the full DTO is returned with Content filled.
func (h *Handler) getTranscript(w http.ResponseWriter, r *http.Request) {
	p := &params{q: r.URL.Query()}
	req := app.GetTranscriptRequest{
		VideoID:  r.PathValue("video_id"),
		StartMs:  p.int64Ptr("start_ms"),
		EndMs:    p.int64Ptr("end_ms"),
		Format:   p.str("format"),
		MaxChars: p.int("max_chars", 0),
	}
	if ids := p.str("segment_ids"); ids != "" {
		for _, id := range strings.Split(ids, ",") {
			if id = strings.TrimSpace(id); id != "" {
				req.SegmentIDs = append(req.SegmentIDs, id)
			}
		}
		if len(req.SegmentIDs) > app.MaxSegmentIDs {
			p.fail("segment_ids", fmt.Sprintf("最多 %d 个", app.MaxSegmentIDs))
		}
	}
	textType, isText := transcriptTypes[req.Format]
	if !isText && req.Format != "" && req.Format != app.FormatJSON {
		p.fail("format", "只能是 json、srt、vtt 或 txt")
	}
	if p.err != nil {
		writeError(w, p.err)
		return
	}
	tr, err := h.svc.GetTranscript(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	if !isText || acceptsJSON(r) {
		writeJSON(w, http.StatusOK, tr)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", textType)
	hdr.Set("Cache-Control", "no-store")
	hdr.Set(HeaderTruncated, strconv.FormatBool(tr.Truncated))
	if tr.NextStartMs != nil {
		hdr.Set(HeaderNextStartMs, strconv.FormatInt(*tr.NextStartMs, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, tr.Content)
}

// acceptsJSON reports whether the client explicitly asked for JSON.
func acceptsJSON(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, _, _ := strings.Cut(part, ";")
		if strings.EqualFold(strings.TrimSpace(mt), "application/json") {
			return true
		}
	}
	return false
}

func (h *Handler) getPlayback(w http.ResponseWriter, r *http.Request) {
	pb, err := h.svc.GetPlayback(r.Context(), r.PathValue("video_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pb)
}

func (h *Handler) createExport(w http.ResponseWriter, r *http.Request) {
	var req app.ExportRequest
	if err := decodeBody(w, r, &req, false); err != nil {
		writeError(w, err)
		return
	}
	req.VideoID = r.PathValue("video_id")
	exp, err := h.svc.CreateExport(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, exp)
}

// exportTypes maps export formats to media types.
var exportTypes = map[string]string{
	app.FormatMD:    "text/markdown; charset=utf-8",
	app.FormatMDRaw: "text/markdown; charset=utf-8",
	app.FormatJSON:  "application/json; charset=utf-8",
	app.FormatSRT:   "application/x-subrip; charset=utf-8",
	app.FormatVTT:   "text/vtt; charset=utf-8",
	app.FormatTXT:   "text/plain; charset=utf-8",
}

func (h *Handler) downloadExport(w http.ResponseWriter, r *http.Request) {
	rc, exp, err := h.svc.OpenExport(r.Context(), r.PathValue("export_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer rc.Close()
	if exp == nil {
		exp = &app.Export{ExportID: r.PathValue("export_id")}
	}
	hdr := w.Header()
	ct := exportTypes[exp.Format]
	if ct == "" {
		ct = "application/octet-stream"
	}
	hdr.Set("Content-Type", ct)
	hdr.Set("Cache-Control", "no-store")
	name := exp.Filename
	if name == "" {
		name = exp.ExportID
	}
	hdr.Set("Content-Disposition", contentDisposition(name))
	if meta, err := json.Marshal(exp); err == nil {
		// The metadata rides in a header so OpenExport over HTTP is one call.
		hdr.Set(HeaderExport, base64.RawURLEncoding.EncodeToString(meta))
	}
	if exp.Bytes > 0 {
		hdr.Set("Content-Length", strconv.FormatInt(exp.Bytes, 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, rc)
}

// contentDisposition builds an attachment header with an ASCII fallback and
// the RFC 5987 / RFC 6266 UTF-8 form, so Chinese titles survive the download.
func contentDisposition(name string) string {
	var fallback, encoded strings.Builder
	for _, c := range name {
		if c < 0x20 || c == 0x7f || c > 0x7e || c == '"' || c == '\\' || c == '/' || c == '%' {
			fallback.WriteByte('_')
		} else {
			fallback.WriteRune(c)
		}
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		if isAttrChar(b) {
			encoded.WriteByte(b)
		} else {
			fmt.Fprintf(&encoded, "%%%02X", b)
		}
	}
	return `attachment; filename="` + fallback.String() + `"; filename*=UTF-8''` + encoded.String()
}

// isAttrChar is the attr-char set of RFC 5987 §3.2.1.
func isAttrChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return strings.IndexByte("!#$&+-.^_`|~", b) >= 0
}

func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc.GetSettings(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *Handler) patchSettings(w http.ResponseWriter, r *http.Request) {
	var patch map[string]any
	if err := decodeBody(w, r, &patch, false); err != nil {
		writeError(w, err)
		return
	}
	if patch == nil {
		writeError(w, invalid("请求体必须是 JSON 对象"))
		return
	}
	s, err := h.svc.PatchSettings(r.Context(), patch)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// setSecret is write-only: the value is never echoed back.
func (h *Handler) setSecret(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Value string `json:"value"`
	}
	if err := decodeBody(w, r, &req, false); err != nil {
		writeError(w, err)
		return
	}
	if err := h.svc.SetSecret(r.Context(), r.PathValue("name"), req.Value); err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) testLLM(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.TestLLM(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) listLLMModels(w http.ResponseWriter, r *http.Request) {
	var req app.LLMModelsRequest
	if err := decodeBody(w, r, &req, true); err != nil {
		writeError(w, err)
		return
	}
	res, err := h.svc.ListLLMModels(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) getTools(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.GetTools(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) prepareTools(w http.ResponseWriter, r *http.Request) {
	var req app.PrepareToolsRequest
	if err := decodeBody(w, r, &req, true); err != nil {
		writeError(w, err)
		return
	}
	job, err := h.svc.PrepareTools(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (h *Handler) updateTool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(w, r, &req, false); err != nil {
		writeError(w, err)
		return
	}
	res, err := h.svc.UpdateTool(r.Context(), req.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) importTool(w http.ResponseWriter, r *http.Request) {
	var req app.ToolImportRequest
	if err := decodeBody(w, r, &req, false); err != nil {
		writeError(w, err)
		return
	}
	res, err := h.svc.ImportTool(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// media serves the playback file. The signed `mt` token is the only
// credential: <video> elements cannot send an Authorization header, and the
// desktop WebView has no session cookie. The service validates the token.
func (h *Handler) media(w http.ResponseWriter, r *http.Request) {
	mt := r.URL.Query().Get("mt")
	if mt == "" {
		writeError(w, unauthorized("media_token", "缺少媒体令牌"))
		return
	}
	pf, err := h.svc.OpenPlayback(r.Context(), r.PathValue("video_id"), mt)
	if err != nil {
		writeError(w, err)
		return
	}
	defer pf.Content.Close()
	h.track(func() {
		if pf.Mime != "" {
			w.Header().Set("Content-Type", pf.Mime)
		}
		w.Header().Set("Cache-Control", "private, max-age=0")
		http.ServeContent(w, r, pf.Name, pf.ModTime, pf.Content)
	})
}
