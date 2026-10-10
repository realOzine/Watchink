// Package client implements app.Remote over the engine's HTTP API and the
// engine discovery / spawn flow. The CLI, the MCP server
// and the desktop shell receive it as an app.Connector.
package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"watchink/internal/api"
	"watchink/internal/app"
)

// Per-request timeouts. They bound ordinary calls only; uploads, downloads
// and the event stream are limited by the caller's context, and a long poll
// gets its wait time on top.
const (
	defaultTimeout = 30 * time.Second
	// slowTimeout covers calls that do real work before answering: the LLM
	// test, a tool update download, importing a multi-gigabyte model file,
	// writing an export.
	slowTimeout = 15 * time.Minute
	// headerTimeout bounds the wait for response headers of a stream.
	headerTimeout = 30 * time.Second
	// longPollSlack is added to wait_seconds for a long poll.
	longPollSlack = 15 * time.Second
)

// Options configures New.
type Options struct {
	// BaseURL is the engine address, e.g. http://127.0.0.1:47823.
	BaseURL string
	// Token is the engine bearer token from engine.json.
	Token string
	// Requester fills ImportRequest.RequestedBy when the caller left it
	// empty: desktop / web / cli / mcp.
	Requester string
	// HTTPClient overrides the transport (tests). It must not set Timeout.
	HTTPClient *http.Client
	// StreamIdleTimeout reconnects the event stream when nothing (not even a
	// heartbeat) arrived for this long; zero means 45 s (3 heartbeats).
	StreamIdleTimeout time.Duration
}

// Client is an app.Remote backed by the HTTP API.
type Client struct {
	base       string
	token      string
	requester  string
	hc         *http.Client
	streamIdle time.Duration
}

var _ app.Remote = (*Client)(nil)

// New returns a client for a known engine address and token.
func New(opts Options) *Client {
	c := &Client{
		base:       strings.TrimRight(opts.BaseURL, "/"),
		token:      opts.Token,
		requester:  opts.Requester,
		hc:         opts.HTTPClient,
		streamIdle: opts.StreamIdleTimeout,
	}
	if c.hc == nil {
		c.hc = &http.Client{Transport: &http.Transport{
			// The engine is on loopback: never go through a proxy.
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     60 * time.Second,
			DisableCompression:  true,
		}}
	}
	if c.streamIdle <= 0 {
		c.streamIdle = 3 * api.DefaultHeartbeat
	}
	return c
}

// NewWithToken returns an app.Remote for a known engine address and token
// (tests and the desktop shell).
func NewWithToken(baseURL, token string) app.Remote {
	return New(Options{BaseURL: baseURL, Token: token})
}

// BaseURL implements app.Remote.
func (c *Client) BaseURL() string { return c.base }

// Token returns the engine bearer token. The desktop shell hands it to its
// WebView through the EngineInfo binding; never log it.
func (c *Client) Token() string { return c.token }

// Close implements app.Remote.
func (c *Client) Close() error {
	c.hc.CloseIdleConnections()
	return nil
}

func unavailable(err error) *app.Error {
	return app.E(app.CodeEngineUnavailable, "无法连接 Engine：%v", err).
		WithHint("Engine 可能已退出；请重试，或查看 <数据目录>/logs/engine.log。").
		WithCause(err)
}

// transportError maps a failed round trip: the caller's own cancellation is
// passed through, everything else means the engine is not reachable.
func transportError(parent context.Context, err error) error {
	if perr := parent.Err(); perr != nil {
		return perr
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return app.E(app.CodeEngineUnavailable, "Engine 请求超时").WithCause(err)
	}
	return unavailable(err)
}

// decodeError turns a non-2xx response into the *app.Error it carries.
func decodeError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env app.ErrorEnvelope
	if json.Unmarshal(body, &env) == nil && env.Error != nil && env.Error.Code != "" {
		return env.Error
	}
	code := app.CodeInternal
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		code = app.CodeUnauthorized
	case http.StatusNotFound:
		code = app.CodeNotFound
	case http.StatusBadRequest:
		code = app.CodeInvalidArgument
	}
	return app.E(code, "Engine 返回了 HTTP %d", resp.StatusCode)
}

func (c *Client) newRequest(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Request, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, app.E(app.CodeInvalidArgument, "无效的请求：%v", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// call performs one JSON request bounded by timeout and decodes the 2xx
// body into out (when non-nil).
func (c *Client) call(ctx context.Context, timeout time.Duration, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return app.E(app.CodeInvalidArgument, "无法编码请求：%v", err)
		}
		body = bytes.NewReader(b)
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := c.newRequest(rctx, method, path, q, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if perr := ctx.Err(); perr != nil {
			return perr
		}
		return app.E(app.CodeInternal, "无法解析 Engine 的响应：%v", err).WithCause(err)
	}
	return nil
}

// openStream sends a request whose body is consumed by the caller. Only the
// wait for the response headers is bounded; the returned cancel must be
// called when the body is done.
func (c *Client) openStream(ctx context.Context, req func(ctx context.Context) (*http.Request, error)) (*http.Response, context.CancelFunc, error) {
	sctx, cancel := context.WithCancel(ctx)
	r, err := req(sctx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	timer := time.AfterFunc(headerTimeout, cancel)
	resp, err := c.hc.Do(r)
	stopped := timer.Stop()
	if err != nil {
		cancel()
		if !stopped && ctx.Err() == nil {
			return nil, nil, app.E(app.CodeEngineUnavailable, "Engine 请求超时").WithCause(err)
		}
		return nil, nil, transportError(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := decodeError(resp)
		resp.Body.Close()
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}

func path(parts ...string) string {
	var b strings.Builder
	b.WriteString(app.APIPrefix)
	for _, p := range parts {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(p))
	}
	return b.String()
}

// EngineInfo implements app.Service.
func (c *Client) EngineInfo(ctx context.Context) (*app.EngineInfo, error) {
	var out app.EngineInfo
	if err := c.call(ctx, defaultTimeout, http.MethodGet, path("engine"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Import implements app.Service.
func (c *Client) Import(ctx context.Context, req app.ImportRequest) (*app.ImportResponse, error) {
	if req.RequestedBy == "" {
		req.RequestedBy = c.requester
	}
	var out app.ImportResponse
	if err := c.call(ctx, defaultTimeout, http.MethodPost, path("imports"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateUpload implements app.Service. The body is streamed; only ctx bounds
// the transfer.
func (c *Client) CreateUpload(ctx context.Context, filename string, size int64, body io.Reader) (*app.Upload, error) {
	req, err := c.newRequest(ctx, http.MethodPost, path("uploads"), nil, body)
	if err != nil {
		return nil, err
	}
	// net/http only infers the length of a few reader types; set it so the
	// engine knows the size up front, or use chunked encoding when unknown.
	if size >= 0 {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	} else {
		req.ContentLength = -1
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(api.HeaderFilename, url.PathEscape(filename))
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, decodeError(resp)
	}
	var out app.Upload
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, app.E(app.CodeInternal, "无法解析 Engine 的响应：%v", err).WithCause(err)
	}
	return &out, nil
}

// ListJobs implements app.Service.
func (c *Client) ListJobs(ctx context.Context, req app.ListJobsRequest) (*app.JobList, error) {
	q := url.Values{}
	setStr(q, "status", req.Status)
	setStr(q, "video_id", req.VideoID)
	setStr(q, "cursor", req.Cursor)
	setInt(q, "limit", req.Limit)
	var out app.JobList
	if err := c.call(ctx, defaultTimeout, http.MethodGet, path("jobs"), q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetJob implements app.Service; with WaitSeconds > 0 it is a long poll and
// the request timeout is extended by the wait.
func (c *Client) GetJob(ctx context.Context, req app.GetJobRequest) (*app.Job, error) {
	q := url.Values{}
	timeout := defaultTimeout
	if req.WaitSeconds > 0 {
		wait := min(req.WaitSeconds, app.MaxWaitSeconds)
		q.Set("wait_seconds", strconv.Itoa(wait))
		q.Set("after_rev", strconv.FormatInt(req.AfterRev, 10))
		timeout = time.Duration(wait)*time.Second + longPollSlack
	}
	var out app.Job
	if err := c.call(ctx, timeout, http.MethodGet, path("jobs", req.JobID), q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelJob implements app.Service.
func (c *Client) CancelJob(ctx context.Context, jobID string) (*app.Job, error) {
	var out app.Job
	if err := c.call(ctx, defaultTimeout, http.MethodPost, path("jobs", jobID, "cancel"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RetryJob implements app.Service.
func (c *Client) RetryJob(ctx context.Context, jobID string) (*app.Job, error) {
	var out app.Job
	if err := c.call(ctx, defaultTimeout, http.MethodPost, path("jobs", jobID, "retry"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListVideos implements app.Service.
func (c *Client) ListVideos(ctx context.Context, req app.ListVideosRequest) (*app.VideoList, error) {
	q := url.Values{}
	setStr(q, "cursor", req.Cursor)
	setStr(q, "title_contains", req.TitleContains)
	setInt(q, "limit", req.Limit)
	var out app.VideoList
	if err := c.call(ctx, defaultTimeout, http.MethodGet, path("videos"), q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetVideo implements app.Service.
func (c *Client) GetVideo(ctx context.Context, videoID string) (*app.Video, error) {
	var out app.Video
	if err := c.call(ctx, defaultTimeout, http.MethodGet, path("videos", videoID), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetNote implements app.Service.
func (c *Client) GetNote(ctx context.Context, req app.GetNoteRequest) (*app.Note, error) {
	q := url.Values{}
	setStr(q, "format", req.Format)
	setStr(q, "note_id", req.NoteID)
	setInt(q, "offset", req.Offset)
	setInt(q, "max_chars", req.MaxChars)
	var out app.Note
	if err := c.call(ctx, defaultTimeout, http.MethodGet, path("videos", req.VideoID, "note"), q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTranscript implements app.Service. It always asks for the JSON
// representation, so srt / vtt / txt results keep their paging metadata
// with the rendered text in Content.
func (c *Client) GetTranscript(ctx context.Context, req app.GetTranscriptRequest) (*app.Transcript, error) {
	if len(req.SegmentIDs) > app.MaxSegmentIDs {
		return nil, app.E(app.CodeInvalidArgument, "segment_ids 最多 %d 个", app.MaxSegmentIDs)
	}
	q := url.Values{}
	setStr(q, "format", req.Format)
	if req.StartMs != nil {
		q.Set("start_ms", strconv.FormatInt(*req.StartMs, 10))
	}
	if req.EndMs != nil {
		q.Set("end_ms", strconv.FormatInt(*req.EndMs, 10))
	}
	if len(req.SegmentIDs) > 0 {
		q.Set("segment_ids", strings.Join(req.SegmentIDs, ","))
	}
	if req.MaxChars != 0 {
		// Negative means "no limit" (export) and must reach the engine.
		q.Set("max_chars", strconv.Itoa(req.MaxChars))
	}
	var out app.Transcript
	if err := c.call(ctx, defaultTimeout, http.MethodGet, path("videos", req.VideoID, "transcript"), q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPlayback implements app.Service.
func (c *Client) GetPlayback(ctx context.Context, videoID string) (*app.Playback, error) {
	var out app.Playback
	if err := c.call(ctx, defaultTimeout, http.MethodGet, path("videos", videoID, "playback"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateExport implements app.Service.
func (c *Client) CreateExport(ctx context.Context, req app.ExportRequest) (*app.Export, error) {
	var out app.Export
	if err := c.call(ctx, slowTimeout, http.MethodPost, path("videos", req.VideoID, "exports"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// streamBody releases the stream context when the body is closed.
type streamBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *streamBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// OpenExport implements app.Service: it streams the download; the caller
// closes the reader.
func (c *Client) OpenExport(ctx context.Context, exportID string) (io.ReadCloser, *app.Export, error) {
	resp, cancel, err := c.openStream(ctx, func(ctx context.Context) (*http.Request, error) {
		return c.newRequest(ctx, http.MethodGet, path("exports", exportID, "download"), nil, nil)
	})
	if err != nil {
		return nil, nil, err
	}
	exp := &app.Export{ExportID: exportID, Bytes: resp.ContentLength}
	if raw := resp.Header.Get(api.HeaderExport); raw != "" {
		if b, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
			var meta app.Export
			if json.Unmarshal(b, &meta) == nil {
				exp = &meta
			}
		}
	}
	return &streamBody{ReadCloser: resp.Body, cancel: cancel}, exp, nil
}

// GetSettings implements app.Service.
func (c *Client) GetSettings(ctx context.Context) (*app.Settings, error) {
	var out app.Settings
	if err := c.call(ctx, defaultTimeout, http.MethodGet, path("settings"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PatchSettings implements app.Service.
func (c *Client) PatchSettings(ctx context.Context, patch map[string]any) (*app.Settings, error) {
	if patch == nil {
		patch = map[string]any{}
	}
	var out app.Settings
	if err := c.call(ctx, defaultTimeout, http.MethodPatch, path("settings"), nil, patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetSecret implements app.Service.
func (c *Client) SetSecret(ctx context.Context, name, value string) error {
	body := struct {
		Value string `json:"value"`
	}{value}
	return c.call(ctx, defaultTimeout, http.MethodPut, path("settings", "secrets", name), nil, body, nil)
}

// TestLLM implements app.Service.
func (c *Client) TestLLM(ctx context.Context) (*app.LLMTestResult, error) {
	var out app.LLMTestResult
	if err := c.call(ctx, slowTimeout, http.MethodPost, path("settings", "llm", "test"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListLLMModels implements app.Service.
func (c *Client) ListLLMModels(ctx context.Context, req app.LLMModelsRequest) (*app.LLMModelList, error) {
	var out app.LLMModelList
	if err := c.call(ctx, defaultTimeout, http.MethodPost, path("settings", "llm", "models"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTools implements app.Service.
func (c *Client) GetTools(ctx context.Context) (*app.ToolsStatus, error) {
	var out app.ToolsStatus
	if err := c.call(ctx, defaultTimeout, http.MethodGet, path("tools"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PrepareTools implements app.Service.
func (c *Client) PrepareTools(ctx context.Context, req app.PrepareToolsRequest) (*app.Job, error) {
	if req.Items == nil {
		req.Items = []string{}
	}
	var out app.Job
	if err := c.call(ctx, defaultTimeout, http.MethodPost, path("tools", "prepare"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateTool implements app.Service.
func (c *Client) UpdateTool(ctx context.Context, name string) (*app.ToolsStatus, error) {
	body := struct {
		Name string `json:"name"`
	}{name}
	var out app.ToolsStatus
	if err := c.call(ctx, slowTimeout, http.MethodPost, path("tools", "update"), nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImportTool implements app.Service.
func (c *Client) ImportTool(ctx context.Context, req app.ToolImportRequest) (*app.ToolsStatus, error) {
	var out app.ToolsStatus
	if err := c.call(ctx, slowTimeout, http.MethodPost, path("tools", "import"), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Shutdown implements app.Remote. It returns once the engine has accepted
// the request; the engine exits shortly afterwards.
func (c *Client) Shutdown(ctx context.Context, force bool) error {
	body := struct {
		Force bool `json:"force"`
	}{force}
	return c.call(ctx, defaultTimeout, http.MethodPost, path("engine", "shutdown"), nil, body, nil)
}

func setStr(q url.Values, key, v string) {
	if v != "" {
		q.Set(key, v)
	}
}

func setInt(q url.Values, key string, v int) {
	if v != 0 {
		q.Set(key, strconv.Itoa(v))
	}
}
