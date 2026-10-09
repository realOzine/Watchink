// Package llm is the OpenAI-compatible Chat Completions client used for note
// generation (docs/v0.md D-08, §9.8). It is written directly on net/http so
// that the exact request shape stays under our control: compatible endpoints
// differ a lot in how tolerant they are of extra fields.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"watchink/internal/app"
)

// Structured-output levels (llm.structured_output, §9.8).
const (
	LevelAuto       = "auto"
	LevelJSONSchema = "json_schema"
	LevelJSONObject = "json_object"
	LevelPrompt     = "prompt"
)

// Provider is the only provider name of v0 (D-08).
const Provider = "openai-compatible"

// FinishLength is the finish_reason of an output cut off by max_tokens.
const FinishLength = "length"

const (
	defaultTimeout     = 120 * time.Second
	defaultBaseDelay   = time.Second
	defaultMaxAttempts = 5
	maxRetryAfter      = 2 * time.Minute
	maxResponseBytes   = 8 << 20
	excerptRunes       = 300
)

// Schema is a named JSON Schema describing the expected output object.
type Schema struct {
	Name   string
	Schema json.RawMessage
}

// Request is one chat completion: a system prompt, a user prompt and, when the
// answer must be JSON, the schema of that JSON.
type Request struct {
	System string
	User   string
	// Schema, when set, asks for structured output at the best level the
	// endpoint supports.
	Schema *Schema
	// MaxTokens is sent as max_tokens; 0 leaves the server default.
	MaxTokens int
	// Temperature overrides Config.Temperature for this request.
	Temperature *float64
}

// Usage is the token usage reported by the server (zero when not reported).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is a completed chat completion.
type Response struct {
	// Content is the assistant text with <think>…</think> blocks removed and,
	// for schema requests, Markdown code fences stripped (§9.8).
	Content      string
	FinishReason string
	Usage        Usage
	// StructuredOutput is the level actually used: json_schema, json_object
	// or prompt. It is empty for requests without a schema.
	StructuredOutput string
}

// Truncated reports whether the output was cut off by the token limit
// (finish_reason == "length").
func (r *Response) Truncated() bool { return r != nil && r.FinishReason == FinishLength }

// Client completes chat requests.
type Client interface {
	Complete(ctx context.Context, req Request) (*Response, error)
}

// Config configures a Client.
type Config struct {
	// BaseURL is used exactly as given (no /v1 is appended); the request goes
	// to {BaseURL}/chat/completions.
	BaseURL string
	Model   string
	APIKey  string
	// StructuredOutput is auto (default) / json_schema / json_object / prompt.
	StructuredOutput string
	// Temperature is sent as-is (the settings default is 0.2).
	Temperature float64
	// Timeout bounds one HTTP attempt; 0 means 120 s.
	Timeout time.Duration
	// HTTPClient is optional (proxy support); its own Timeout should be 0.
	HTTPClient *http.Client
	// Acquire is an optional global concurrency gate, taken around every
	// HTTP attempt (not around backoff sleeps).
	Acquire func(ctx context.Context) (release func(), err error)

	// DisableTuning turns the self-healing of request parameters off
	// (tuning.go); tests of the callers' own fallbacks use it.
	DisableTuning bool
	// Warn, when set, receives a human-readable note each time the client
	// heals a request by adjusting a parameter (see tuning.go).
	Warn func(msg string)

	// MaxAttempts is the total number of attempts for retryable failures;
	// 0 means 5 (§9.8).
	MaxAttempts int
	// RetryBaseDelay is the first backoff delay; 0 means 1 s. Tests shrink it.
	RetryBaseDelay time.Duration
	// Sleep replaces the context-aware sleep between attempts (tests).
	Sleep func(ctx context.Context, d time.Duration) error
}

// Validate reports MODEL_NOT_CONFIGURED when base_url, model or the API key is
// missing. An empty key is allowed for loopback endpoints (local servers).
func Validate(cfg Config) error {
	notConfigured := func(what string) error {
		return app.E(app.CodeModelNotConfigured, "没有配置在线模型（缺少 %s）", what).
			WithHint("在设置中填写 base_url、模型名和 API Key 后重试")
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return notConfigured("base_url")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return app.E(app.CodeModelNotConfigured, "在线模型的 base_url 无效").
			WithHint("base_url 需要是 http:// 或 https:// 开头的地址，例如 https://api.openai.com/v1")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return notConfigured("model")
	}
	if cfg.APIKey == "" && !isLoopbackHost(u.Hostname()) {
		return notConfigured("API Key")
	}
	switch cfg.StructuredOutput {
	case "", LevelAuto, LevelJSONSchema, LevelJSONObject, LevelPrompt:
	default:
		return app.E(app.CodeInvalidArgument, "llm.structured_output 的取值无效：%s", cfg.StructuredOutput).
			WithHint("可选值：auto / json_schema / json_object / prompt")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type client struct {
	cfg         Config
	endpoint    string
	cacheKey    string
	http        *http.Client
	timeout     time.Duration
	baseDelay   time.Duration
	maxAttempts int
	sleep       func(ctx context.Context, d time.Duration) error
}

// New builds a Client; it fails with MODEL_NOT_CONFIGURED when the
// configuration is incomplete (see Validate).
func New(cfg Config) (Client, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	c := &client{
		cfg:         cfg,
		endpoint:    base + "/chat/completions",
		cacheKey:    base + "\x00" + cfg.Model,
		http:        cfg.HTTPClient,
		timeout:     cfg.Timeout,
		baseDelay:   cfg.RetryBaseDelay,
		maxAttempts: cfg.MaxAttempts,
		sleep:       cfg.Sleep,
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	if c.timeout <= 0 {
		c.timeout = defaultTimeout
	}
	if c.baseDelay <= 0 {
		c.baseDelay = defaultBaseDelay
	}
	if c.maxAttempts <= 0 {
		c.maxAttempts = defaultMaxAttempts
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	return c, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// levelCache remembers, per (base_url, model), the structured-output level the
// endpoint accepted, for the lifetime of the process (§9.8).
var levelCache = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// ResetStructuredOutputCache forgets every cached degradation (tests).
func ResetStructuredOutputCache() {
	levelCache.Lock()
	defer levelCache.Unlock()
	levelCache.m = map[string]string{}
	resetTuningCache()
}

func cachedLevel(key string) string {
	levelCache.Lock()
	defer levelCache.Unlock()
	return levelCache.m[key]
}

func rememberLevel(key, level string) {
	levelCache.Lock()
	defer levelCache.Unlock()
	levelCache.m[key] = level
}

var levelOrder = []string{LevelJSONSchema, LevelJSONObject, LevelPrompt}

// levels returns the structured-output levels to try, in order.
func (c *client) levels(req Request) []string {
	if req.Schema == nil {
		return []string{""}
	}
	switch c.cfg.StructuredOutput {
	case LevelJSONSchema, LevelJSONObject, LevelPrompt:
		return []string{c.cfg.StructuredOutput}
	}
	start := 0
	if cached := cachedLevel(c.cacheKey); cached != "" {
		for i, l := range levelOrder {
			if l == cached {
				start = i
			}
		}
	}
	return levelOrder[start:]
}

// Complete implements Client.
func (c *client) Complete(ctx context.Context, req Request) (*Response, error) {
	auto := c.cfg.StructuredOutput == "" || c.cfg.StructuredOutput == LevelAuto
	levels := c.levels(req)
	for i, level := range levels {
		resp, cerr, attempts := c.completeAtLevel(ctx, req, level)
		if cerr == nil {
			resp.StructuredOutput = level
			if req.Schema != nil {
				resp.Content = stripCodeFence(resp.Content)
				if auto {
					rememberLevel(c.cacheKey, level)
				}
			}
			return resp, nil
		}
		if cerr.kind == kindCancelled {
			return nil, cerr.cause
		}
		if cerr.kind == kindFormat && i+1 < len(levels) {
			// The endpoint rejected this response_format: degrade, and cache
			// the result so later calls skip the rejected level (§9.8).
			rememberLevel(c.cacheKey, levels[i+1])
			continue
		}
		return nil, cerr.appError(attempts)
	}
	return nil, app.E(app.CodeInternal, "没有可用的结构化输出级别")
}

// completeAtLevel sends the request at one structured-output level, healing
// parameter problems on the way: a truncated answer gets a larger output
// budget, a rejected parameter is renamed, lowered or dropped, and a timeout
// gets more time. What worked is remembered for the endpoint + model.
func (c *client) completeAtLevel(ctx context.Context, req Request, level string) (*Response, *callErr, int) {
	var t tuning
	if !c.cfg.DisableTuning {
		t = cachedTuning(c.cacheKey)
	}
	attempts := 0
	for round := 0; ; round++ {
		body, err := c.buildBody(req, level, t)
		if err != nil {
			return nil, &callErr{kind: kindBadRequest, cause: err}, attempts
		}
		resp, cerr, n := c.doWithRetry(ctx, body, &t)
		attempts += n
		if cerr == nil {
			if c.cfg.DisableTuning {
				return resp, nil, attempts
			}
			if resp.Truncated() && round < maxAdaptRounds {
				if next, note, ok := t.moreTokens(req.MaxTokens); ok {
					c.warn(note)
					t = next
					continue
				}
			}
			rememberTuning(c.cacheKey, t)
			return resp, nil, attempts
		}
		if (cerr.kind == kindBadRequest || cerr.kind == kindFormat) && round < maxAdaptRounds && !c.cfg.DisableTuning {
			// Checked before the structured-output degradation: a message
			// like "max_tokens is not supported" must not be mistaken for
			// an unsupported response_format.
			if next, note, ok := t.adaptToRejection(cerr.body, t.maxTokens(req.MaxTokens)); ok {
				c.warn(note)
				t = next
				continue
			}
		}
		return nil, cerr, attempts
	}
}

func (c *client) warn(msg string) {
	if c.cfg.Warn != nil && msg != "" {
		c.cfg.Warn(msg)
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type jsonSchemaFormat struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type responseFormat struct {
	Type       string            `json:"type"`
	JSONSchema *jsonSchemaFormat `json:"json_schema,omitempty"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	MaxCompletion  int             `json:"max_completion_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

func (c *client) buildBody(req Request, level string, t tuning) ([]byte, error) {
	user := req.User
	cr := chatRequest{Model: c.cfg.Model}
	if !t.noTemperature {
		temp := c.cfg.Temperature
		if req.Temperature != nil {
			temp = *req.Temperature
		}
		cr.Temperature = &temp
	}
	if n := t.maxTokens(req.MaxTokens); t.completionParam {
		cr.MaxCompletion = n
	} else {
		cr.MaxTokens = n
	}
	if req.Schema != nil {
		switch level {
		case LevelJSONSchema:
			name := req.Schema.Name
			if name == "" {
				name = "output"
			}
			cr.ResponseFormat = &responseFormat{Type: "json_schema",
				JSONSchema: &jsonSchemaFormat{Name: name, Strict: true, Schema: req.Schema.Schema}}
		case LevelJSONObject:
			cr.ResponseFormat = &responseFormat{Type: "json_object"}
			user += schemaInstruction(req.Schema)
		default:
			user += schemaInstruction(req.Schema)
		}
	}
	if req.System != "" {
		cr.Messages = append(cr.Messages, chatMessage{Role: "system", Content: req.System})
	}
	cr.Messages = append(cr.Messages, chatMessage{Role: "user", Content: user})
	return json.Marshal(cr)
}

// schemaInstruction is appended to the prompt below json_schema level so the
// model still knows the exact shape. It also contains the word "JSON", which
// some endpoints require for response_format json_object.
func schemaInstruction(s *Schema) string {
	return "\n\n只输出一个 JSON 对象，不要输出解释、前后缀或 Markdown 代码围栏。JSON 必须符合下面的 JSON Schema：\n" + string(s.Schema)
}

// doWithRetry performs the request with the §9.8 retry policy and returns the
// number of attempts made.
func (c *client) doWithRetry(ctx context.Context, body []byte, t *tuning) (*Response, *callErr, int) {
	var last *callErr
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, &callErr{kind: kindCancelled, cause: err}, attempt - 1
		}
		resp, cerr := c.doOnce(ctx, body, t.timeout(c.timeout))
		if cerr == nil {
			return resp, nil, attempt
		}
		if cerr.timedOut && !c.cfg.DisableTuning {
			// A slow (often reasoning) model: give the next attempt more
			// time instead of failing the same way again.
			if next, note, ok := t.moreTime(c.timeout); ok {
				c.warn(note)
				*t = next
				rememberTuning(c.cacheKey, mergeTimeout(cachedTuning(c.cacheKey), next))
			}
		}
		if err := ctx.Err(); err != nil {
			// The caller's context ended; report that, not the transport error.
			return nil, &callErr{kind: kindCancelled, cause: err}, attempt
		}
		last = cerr
		if !cerr.retryable() || attempt == c.maxAttempts {
			return nil, cerr, attempt
		}
		delay := c.backoff(attempt)
		if cerr.hasRetryAfter {
			delay = min(cerr.retryAfter, maxRetryAfter)
		}
		if err := c.sleep(ctx, delay); err != nil {
			return nil, &callErr{kind: kindCancelled, cause: err}, attempt
		}
	}
	return nil, last, c.maxAttempts
}

// backoff is exponential from the base delay with ±25 % jitter.
func (c *client) backoff(attempt int) time.Duration {
	d := c.baseDelay << (attempt - 1)
	return time.Duration(float64(d) * (0.75 + rand.Float64()*0.5))
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// mergeTimeout stores a learned timeout without touching the other fields,
// which are only committed once a request succeeded with them.
func mergeTimeout(stored, cur tuning) tuning {
	stored.timeoutScale = cur.timeoutScale
	return stored
}

func (c *client) doOnce(ctx context.Context, body []byte, timeout time.Duration) (*Response, *callErr) {
	if c.cfg.Acquire != nil {
		release, err := c.cfg.Acquire(ctx)
		if err != nil {
			return nil, &callErr{kind: kindCancelled, cause: err}
		}
		if release != nil {
			defer release()
		}
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(actx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &callErr{kind: kindBadRequest, cause: err}
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	if c.cfg.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	hresp, err := c.http.Do(hreq)
	if err != nil {
		return nil, &callErr{kind: kindNetwork, cause: transportCause(err), timedOut: actx.Err() != nil && ctx.Err() == nil}
	}
	defer hresp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, maxResponseBytes))
	if err != nil {
		return nil, &callErr{kind: kindNetwork, status: hresp.StatusCode, cause: transportCause(err), timedOut: actx.Err() != nil && ctx.Err() == nil}
	}
	if hresp.StatusCode < 200 || hresp.StatusCode > 299 {
		ce := &callErr{kind: classify(hresp.StatusCode, string(raw), bytes.Contains(body, []byte(`"response_format"`))),
			status: hresp.StatusCode, body: c.excerpt(string(raw))}
		if hresp.StatusCode == http.StatusTooManyRequests {
			ce.retryAfter, ce.hasRetryAfter = parseRetryAfter(hresp.Header.Get("Retry-After"), time.Now())
		}
		return nil, ce
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, &callErr{kind: kindBadResponse, status: hresp.StatusCode, body: c.excerpt(string(raw)), cause: err}
	}
	if len(cr.Choices) == 0 {
		ce := &callErr{kind: kindBadResponse, status: hresp.StatusCode, body: c.excerpt(string(raw))}
		if cr.Error != nil {
			// Some gateways report failures inside a 200 body.
			ce.kind = classify(http.StatusBadRequest, cr.Error.Message, bytes.Contains(body, []byte(`"response_format"`)))
		}
		return nil, ce
	}
	choice := cr.Choices[0]
	return &Response{
		Content:      stripThink(contentText(choice.Message.Content)),
		FinishReason: choice.FinishReason,
		Usage:        cr.Usage,
	}, nil
}

// transportCause drops the request URL from a transport error: a base URL may
// carry credentials in its userinfo or query string.
func transportCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// contentText accepts the plain-string form and the content-parts array form.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "" || p.Type == "text" || p.Type == "output_text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// parseRetryAfter understands both the delta-seconds and the HTTP-date form.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}
