package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"watchink/internal/app"
	"watchink/internal/capability/llm"
	"watchink/internal/capability/llm/llmtest"
)

const testKey = "sk-test-SECRETSECRET1234"

type out struct {
	OK bool `json:"ok"`
}

func newClient(t *testing.T, url string, mod func(*llm.Config)) llm.Client {
	t.Helper()
	llm.ResetStructuredOutputCache()
	cfg := llm.Config{BaseURL: url, Model: "fake-model", APIKey: testKey, RetryBaseDelay: time.Millisecond, Temperature: 0.2}
	if mod != nil {
		mod(&cfg)
	}
	c, err := llm.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func start(t *testing.T) (*llmtest.Server, string) {
	t.Helper()
	fake := llmtest.New()
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	return fake, ts.URL + "/v1"
}

func ping() llm.Request {
	return llm.Request{System: "sys", User: llm.PingMarker + "\nping", Schema: llm.SchemaFor("ping", out{}), MaxTokens: 64}
}

func TestCompleteBasics(t *testing.T) {
	fake, url := start(t)
	temp := 0.7
	c := newClient(t, url+"/", nil) // trailing slash tolerated
	req := ping()
	req.Temperature = &temp
	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"ok":true}` || resp.FinishReason != "stop" || resp.StructuredOutput != llm.LevelJSONSchema {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Usage.TotalTokens == 0 || resp.Truncated() {
		t.Fatalf("usage/truncated wrong: %+v", resp)
	}
	r := fake.Requests()[0]
	if r.Path != "/v1/chat/completions" {
		t.Errorf("path = %q", r.Path)
	}
	if !r.HasKey || r.Model != "fake-model" || r.MaxTokens != 64 || r.Temperature == nil || *r.Temperature != 0.7 {
		t.Errorf("request fields wrong: %+v", r)
	}
	if r.ResponseFormat != "json_schema" || r.SchemaName != "ping" || r.System != "sys" {
		t.Errorf("format/system wrong: %+v", r)
	}
	if strings.Contains(r.User, "JSON Schema") {
		t.Errorf("schema must not be appended at json_schema level")
	}
}

func TestNoSchemaSendsNoResponseFormat(t *testing.T) {
	fake, url := start(t)
	fake.Enqueue(llmtest.Step{Kind: llmtest.KindContent, Content: "<think>hmm</think>```\nplain text\n```"})
	c := newClient(t, url, nil)
	resp, err := c.Complete(context.Background(), llm.Request{User: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if r := fake.Requests()[0]; r.ResponseFormat != "" || r.System != "" {
		t.Errorf("unexpected request: %+v", r)
	}
	// Fences are only stripped for schema requests; <think> always is.
	if resp.Content != "```\nplain text\n```" || resp.StructuredOutput != "" {
		t.Errorf("content = %q level = %q", resp.Content, resp.StructuredOutput)
	}
}

func TestStructuredOutputDegradationAndCache(t *testing.T) {
	fake, url := start(t)
	fake.Configure(llmtest.Script{RejectJSONSchema: true, RejectJSONObject: true})
	c := newClient(t, url, nil)
	resp, err := c.Complete(context.Background(), ping())
	if err != nil {
		t.Fatal(err)
	}
	if resp.StructuredOutput != llm.LevelPrompt {
		t.Fatalf("level = %q, want prompt", resp.StructuredOutput)
	}
	reqs := fake.Requests()
	if len(reqs) != 3 || reqs[0].ResponseFormat != "json_schema" || reqs[1].ResponseFormat != "json_object" || reqs[2].ResponseFormat != "" {
		t.Fatalf("degradation order wrong: %+v", reqs)
	}
	for _, r := range reqs[1:] {
		if !strings.Contains(r.User, `"additionalProperties":false`) {
			t.Errorf("schema not appended to prompt at level %q", r.ResponseFormat)
		}
	}
	// The degradation is cached per (base_url, model): a second client for
	// the same endpoint goes straight to prompt-only.
	c2, err := llm.New(llm.Config{BaseURL: url, Model: "fake-model", APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Complete(context.Background(), ping()); err != nil {
		t.Fatal(err)
	}
	if n := fake.Calls(""); n != 4 {
		t.Fatalf("calls = %d, want 4 (cached level)", n)
	}
	// A different model has its own cache entry.
	c3, _ := llm.New(llm.Config{BaseURL: url, Model: "other", APIKey: testKey})
	if _, err := c3.Complete(context.Background(), ping()); err != nil {
		t.Fatal(err)
	}
	if n := fake.Calls(""); n != 7 {
		t.Fatalf("calls = %d, want 7", n)
	}
}

func TestDegradeToJSONObject(t *testing.T) {
	fake, url := start(t)
	fake.Enqueue(llmtest.Step{Kind: llmtest.KindRejectJSONSchema})
	c := newClient(t, url, nil)
	resp, err := c.Complete(context.Background(), ping())
	if err != nil || resp.StructuredOutput != llm.LevelJSONObject {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestFixedLevelDoesNotDegrade(t *testing.T) {
	fake, url := start(t)
	fake.Configure(llmtest.Script{RejectJSONSchema: true})
	c := newClient(t, url, func(c *llm.Config) { c.StructuredOutput = llm.LevelJSONSchema })
	_, err := c.Complete(context.Background(), ping())
	if !app.IsCode(err, app.CodeModelUnavailable) || fake.Calls("") != 1 {
		t.Fatalf("err=%v calls=%d", err, fake.Calls(""))
	}
	c = newClient(t, url, func(c *llm.Config) { c.StructuredOutput = llm.LevelJSONObject })
	resp, err := c.Complete(context.Background(), ping())
	if err != nil || resp.StructuredOutput != llm.LevelJSONObject {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		steps    []llmtest.Step
		code     app.Code
		status   int
		calls    int
		overflow bool
	}{
		{"401", []llmtest.Step{{Kind: llmtest.KindAuthError}}, app.CodeModelAuthFailed, 401, 1, false},
		{"403", []llmtest.Step{{Kind: llmtest.KindStatus, Status: 403}}, app.CodeModelAuthFailed, 403, 1, false},
		{"404", []llmtest.Step{{Kind: llmtest.KindStatus, Status: 404}}, app.CodeModelNotFound, 404, 1, false},
		{"model not found body", []llmtest.Step{{Kind: llmtest.KindStatus, Status: 400,
			Body: `{"error":{"message":"The model 'x' does not exist","code":"model_not_found"}}`}}, app.CodeModelNotFound, 400, 1, false},
		{"429 exhausted", []llmtest.Step{{Kind: llmtest.KindRateLimit, Times: 5}}, app.CodeModelRateLimited, 429, 5, false},
		{"500 exhausted", []llmtest.Step{{Kind: llmtest.KindServerError, Times: 5}}, app.CodeModelUnavailable, 500, 5, false},
		{"connection errors", []llmtest.Step{{Kind: llmtest.KindDropConnection, Times: 5}}, app.CodeModelUnavailable, 0, 5, false},
		{"overflow", []llmtest.Step{{Kind: llmtest.KindContextOverflow}}, app.CodeModelOutputInvalid, 400, 1, true},
		{"other 400", []llmtest.Step{{Kind: llmtest.KindStatus, Status: 400, Body: `{"error":{"message":"request rejected by policy"}}`}},
			app.CodeModelUnavailable, 400, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, url := start(t)
			fake.Enqueue(tc.steps...)
			c := newClient(t, url, nil)
			_, err := c.Complete(context.Background(), ping())
			if !app.IsCode(err, tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
			if got := fake.Calls(""); got != tc.calls {
				t.Errorf("calls = %d, want %d", got, tc.calls)
			}
			if llm.IsContextOverflow(err) != tc.overflow {
				t.Errorf("IsContextOverflow = %v", !tc.overflow)
			}
			e := app.AsError(err)
			if tc.status != 0 && e.Details["http_status"] != tc.status {
				t.Errorf("http_status detail = %v", e.Details["http_status"])
			}
			raw, _ := json.Marshal(e)
			if strings.Contains(string(raw), testKey) || strings.Contains(err.Error(), testKey) {
				t.Errorf("API key leaked into error: %s", raw)
			}
		})
	}
}

func TestRetryThenSuccess(t *testing.T) {
	fake, url := start(t)
	fake.Enqueue(
		llmtest.Step{Kind: llmtest.KindServerError},
		llmtest.Step{Kind: llmtest.KindDropConnection},
		llmtest.Step{Kind: llmtest.KindRateLimit, RetryAfter: "7"},
		llmtest.Step{Kind: llmtest.KindContent, Content: "not even json", Status: 0},
	)
	var mu sync.Mutex
	var sleeps []time.Duration
	c := newClient(t, url, func(c *llm.Config) {
		c.RetryBaseDelay = time.Second
		c.Sleep = func(_ context.Context, d time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			sleeps = append(sleeps, d)
			return nil
		}
	})
	resp, err := c.Complete(context.Background(), ping())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "not even json" {
		t.Fatalf("content = %q", resp.Content)
	}
	if len(sleeps) != 3 {
		t.Fatalf("sleeps = %v", sleeps)
	}
	in := func(d, lo, hi time.Duration) bool { return d >= lo && d <= hi }
	// Exponential from 1 s with ±25 % jitter; 429 honours Retry-After exactly.
	if !in(sleeps[0], 750*time.Millisecond, 1250*time.Millisecond) || !in(sleeps[1], 1500*time.Millisecond, 2500*time.Millisecond) {
		t.Errorf("backoff out of range: %v", sleeps)
	}
	if sleeps[2] != 7*time.Second {
		t.Errorf("Retry-After not honoured: %v", sleeps[2])
	}
}

func TestBodyExcerptIsRedacted(t *testing.T) {
	fake, url := start(t)
	fake.Enqueue(llmtest.Step{Kind: llmtest.KindStatus, Status: 401,
		Body: `{"error":{"message":"Incorrect API key provided: ` + testKey + ` (Authorization: Bearer abc.def-123)"}}`})
	c := newClient(t, url, nil)
	_, err := c.Complete(context.Background(), ping())
	body, _ := app.AsError(err).Details["body"].(string)
	if body == "" || strings.Contains(body, "SECRET") || strings.Contains(body, "abc.def-123") || !strings.Contains(body, "***") {
		t.Fatalf("excerpt not redacted: %q", body)
	}
}

func TestTimeoutIsRetriedAndMapped(t *testing.T) {
	fake, url := start(t)
	fake.Enqueue(llmtest.Step{Kind: llmtest.KindSlow, DelayMs: 2000, Times: 2})
	c := newClient(t, url, func(c *llm.Config) { c.Timeout = 30 * time.Millisecond; c.MaxAttempts = 2 })
	_, err := c.Complete(context.Background(), ping())
	if !app.IsCode(err, app.CodeModelUnavailable) || fake.Calls("") != 2 {
		t.Fatalf("err=%v calls=%d", err, fake.Calls(""))
	}
}

func TestContextCancellation(t *testing.T) {
	fake, url := start(t)
	fake.Enqueue(llmtest.Step{Kind: llmtest.KindSlow, DelayMs: 5000})
	c := newClient(t, url, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	_, err := c.Complete(ctx, ping())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Cancellation during backoff also surfaces ctx.Err().
	fake.Enqueue(llmtest.Step{Kind: llmtest.KindServerError, Times: 5})
	c = newClient(t, url, func(c *llm.Config) { c.RetryBaseDelay = time.Hour })
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Complete(ctx, ping()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestAcquireGate(t *testing.T) {
	fake, url := start(t)
	fake.Enqueue(llmtest.Step{Kind: llmtest.KindServerError})
	var acquired, released atomic.Int32
	c := newClient(t, url, func(c *llm.Config) {
		c.Acquire = func(context.Context) (func(), error) {
			acquired.Add(1)
			return func() { released.Add(1) }, nil
		}
	})
	if _, err := c.Complete(context.Background(), ping()); err != nil {
		t.Fatal(err)
	}
	if acquired.Load() != 2 || released.Load() != 2 {
		t.Fatalf("acquired=%d released=%d, want 2/2 (one per attempt)", acquired.Load(), released.Load())
	}
	gateErr := errors.New("gate closed")
	c = newClient(t, url, func(c *llm.Config) {
		c.Acquire = func(context.Context) (func(), error) { return nil, gateErr }
	})
	if _, err := c.Complete(context.Background(), ping()); !errors.Is(err, gateErr) {
		t.Fatalf("err = %v", err)
	}
}

func TestNotConfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  llm.Config
		code app.Code
	}{
		{"no base url", llm.Config{Model: "m", APIKey: "k"}, app.CodeModelNotConfigured},
		{"bad base url", llm.Config{BaseURL: "api.example.com/v1", Model: "m", APIKey: "k"}, app.CodeModelNotConfigured},
		{"no model", llm.Config{BaseURL: "https://api.example.com/v1", APIKey: "k"}, app.CodeModelNotConfigured},
		{"no key", llm.Config{BaseURL: "https://api.example.com/v1", Model: "m"}, app.CodeModelNotConfigured},
		{"bad level", llm.Config{BaseURL: "https://api.example.com/v1", Model: "m", APIKey: "k", StructuredOutput: "xml"}, app.CodeInvalidArgument},
		{"loopback without key", llm.Config{BaseURL: "http://127.0.0.1:11434/v1", Model: "m"}, ""},
		{"localhost without key", llm.Config{BaseURL: "http://localhost:1234/v1", Model: "m"}, ""},
		{"ipv6 loopback without key", llm.Config{BaseURL: "http://[::1]:1234/v1", Model: "m"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := llm.New(tc.cfg)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !app.IsCode(err, tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
}

func TestLoopbackWithoutKeySendsNoAuthorization(t *testing.T) {
	fake, url := start(t)
	c := newClient(t, url, func(c *llm.Config) { c.APIKey = "" })
	if _, err := c.Complete(context.Background(), ping()); err != nil {
		t.Fatal(err)
	}
	if fake.Requests()[0].HasKey {
		t.Fatal("Authorization header sent without a key")
	}
}

func TestFinishReasonLengthAndCleaning(t *testing.T) {
	fake, url := start(t)
	fake.Enqueue(llmtest.Step{Kind: llmtest.KindLength}, llmtest.Step{Kind: llmtest.KindThinkFenced})
	c := newClient(t, url, func(c *llm.Config) { c.DisableTuning = true })
	resp, err := c.Complete(context.Background(), ping())
	if err != nil || !resp.Truncated() {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	resp, err = c.Complete(context.Background(), ping())
	if err != nil || resp.Content != `{"ok":true}` {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestContentPartsAnd200ErrorBody(t *testing.T) {
	var n atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"error":{"message":"maximum context length exceeded"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"{\"ok\":"},{"type":"text","text":"true}"}]},"finish_reason":"stop"}]}`))
	}))
	defer ts.Close()
	c := newClient(t, ts.URL, nil)
	if _, err := c.Complete(context.Background(), ping()); !llm.IsContextOverflow(err) {
		t.Fatalf("err = %v, want overflow", err)
	}
	resp, err := c.Complete(context.Background(), ping())
	if err != nil || resp.Content != `{"ok":true}` {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestProbe(t *testing.T) {
	fake, url := start(t)
	llm.ResetStructuredOutputCache()
	fake.Configure(llmtest.Script{RejectJSONSchema: true, APIKey: testKey})
	cfg := llm.Config{BaseURL: url, Model: "fake-model", APIKey: testKey, RetryBaseDelay: time.Millisecond}
	res := llm.Test(context.Background(), cfg)
	if !res.OK || res.StructuredOutput != llm.LevelJSONObject || res.Model != "fake-model" || res.Error != nil {
		t.Fatalf("unexpected result: %+v", res)
	}
	cfg.APIKey = "sk-wrong-key-000000"
	res = llm.Test(context.Background(), cfg)
	if res.OK || res.Error == nil || res.Error.Code != app.CodeModelAuthFailed {
		t.Fatalf("unexpected result: %+v", res)
	}
	res = llm.Test(context.Background(), llm.Config{})
	if res.OK || res.Error.Code != app.CodeModelNotConfigured {
		t.Fatalf("unexpected result: %+v", res)
	}
	fake.Enqueue(llmtest.Step{Kind: llmtest.KindContent, Content: "Sure! ok"})
	cfg.APIKey = testKey
	res = llm.Test(context.Background(), cfg)
	if res.OK || res.Error.Code != app.CodeModelOutputInvalid {
		t.Fatalf("unexpected result: %+v", res)
	}
}
