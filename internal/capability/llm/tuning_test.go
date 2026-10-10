package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"watchink/internal/capability/llm"
)

// tuneServer is a scripted endpoint that records every request body.
type tuneServer struct {
	mu     sync.Mutex
	bodies []map[string]any
	handle func(n int, body map[string]any, w http.ResponseWriter)
}

func (s *tuneServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	n := len(s.bodies)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	s.handle(n, body, w)
}

func (s *tuneServer) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

func answer(w http.ResponseWriter, content, finish string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": finish}},
	})
}

func reject(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg}})
}

func num(body map[string]any, key string) int {
	f, _ := body[key].(float64)
	return int(f)
}

func tuneClient(t *testing.T, s *tuneServer, mod func(*llm.Config)) (llm.Client, *[]string) {
	t.Helper()
	llm.ResetStructuredOutputCache()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	var warnings []string
	var mu sync.Mutex
	cfg := llm.Config{BaseURL: ts.URL + "/v1", Model: "m", APIKey: testKey, Temperature: 0.2,
		RetryBaseDelay: time.Millisecond,
		Warn: func(msg string) {
			mu.Lock()
			warnings = append(warnings, msg)
			mu.Unlock()
		}}
	if mod != nil {
		mod(&cfg)
	}
	c, err := llm.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, &warnings
}

// A reasoning model that needs a larger budget: the client raises max_tokens
// until the answer is complete, and starts from that budget next time.
func TestTruncatedAnswerGetsLargerBudget(t *testing.T) {
	s := &tuneServer{handle: func(_ int, body map[string]any, w http.ResponseWriter) {
		if n := num(body, "max_tokens"); n > 0 && n < 8000 {
			answer(w, "", "length")
			return
		}
		answer(w, `{"ok":true}`, "stop")
	}}
	c, warnings := tuneClient(t, s, nil)
	req := llm.Request{User: "hi", MaxTokens: 2500}
	resp, err := c.Complete(context.Background(), req)
	if err != nil || resp.Truncated() || resp.Content != `{"ok":true}` {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	got := s.requests()
	if len(got) != 3 || num(got[0], "max_tokens") != 2500 || num(got[1], "max_tokens") != 5000 || num(got[2], "max_tokens") != 10000 {
		t.Fatalf("budgets = %v", got)
	}
	if len(*warnings) != 2 {
		t.Fatalf("warnings = %q", *warnings)
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := s.requests(); len(got) != 4 || num(got[3], "max_tokens") != 10000 {
		t.Fatalf("the learned budget was not reused: %v", got[len(got)-1])
	}
}

// When no budget is enough the limit is dropped, and a still-truncated answer
// is returned as truncated so the caller can split its input.
func TestStillTruncatedAfterDroppingTheLimit(t *testing.T) {
	s := &tuneServer{handle: func(_ int, _ map[string]any, w http.ResponseWriter) { answer(w, "{", "length") }}
	c, _ := tuneClient(t, s, nil)
	resp, err := c.Complete(context.Background(), llm.Request{User: "hi", MaxTokens: 1000})
	if err != nil || !resp.Truncated() {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	got := s.requests()
	if len(got) != 4 {
		t.Fatalf("requests = %d, want 4 (1000, 2000, 4000, none)", len(got))
	}
	if _, has := got[3]["max_tokens"]; has {
		t.Fatalf("last request still limits the output: %v", got[3])
	}
}

func TestRejectedParametersAreAdjusted(t *testing.T) {
	t.Run("max_completion_tokens", func(t *testing.T) {
		s := &tuneServer{handle: func(_ int, body map[string]any, w http.ResponseWriter) {
			if _, has := body["max_tokens"]; has {
				reject(w, "Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.")
				return
			}
			answer(w, `{"ok":true}`, "stop")
		}}
		c, warnings := tuneClient(t, s, nil)
		resp, err := c.Complete(context.Background(), ping())
		if err != nil {
			t.Fatal(err)
		}
		got := s.requests()
		// The rejection mentions "unsupported": it must not be taken for
		// an unsupported response_format, so the level stays json_schema.
		if len(got) != 2 || num(got[1], "max_completion_tokens") == 0 || resp.StructuredOutput != llm.LevelJSONSchema {
			t.Fatalf("requests=%v level=%s", got, resp.StructuredOutput)
		}
		if len(*warnings) != 1 || !strings.Contains((*warnings)[0], "max_completion_tokens") {
			t.Fatalf("warnings = %q", *warnings)
		}
	})
	t.Run("temperature", func(t *testing.T) {
		s := &tuneServer{handle: func(_ int, body map[string]any, w http.ResponseWriter) {
			if _, has := body["temperature"]; has {
				reject(w, "Unsupported value: 'temperature' does not support 0.2 with this model.")
				return
			}
			answer(w, "ok", "stop")
		}}
		c, _ := tuneClient(t, s, nil)
		if _, err := c.Complete(context.Background(), llm.Request{User: "hi"}); err != nil {
			t.Fatal(err)
		}
		if got := s.requests(); len(got) != 2 {
			t.Fatalf("requests = %d", len(got))
		}
	})
	t.Run("max_tokens above the endpoint limit", func(t *testing.T) {
		s := &tuneServer{handle: func(_ int, body map[string]any, w http.ResponseWriter) {
			if num(body, "max_tokens") > 4096 {
				reject(w, "Invalid max_tokens value, the valid range of max_tokens is [1, 4096]")
				return
			}
			answer(w, "ok", "stop")
		}}
		c, _ := tuneClient(t, s, nil)
		if _, err := c.Complete(context.Background(), llm.Request{User: "hi", MaxTokens: 5000}); err != nil {
			t.Fatal(err)
		}
		if got := s.requests(); len(got) != 2 || num(got[1], "max_tokens") != 4096 {
			t.Fatalf("requests = %v", got)
		}
	})
	t.Run("unrelated 400 is not retried", func(t *testing.T) {
		s := &tuneServer{handle: func(_ int, _ map[string]any, w http.ResponseWriter) { reject(w, "request rejected by policy") }}
		c, _ := tuneClient(t, s, nil)
		if _, err := c.Complete(context.Background(), llm.Request{User: "hi", MaxTokens: 100}); err == nil {
			t.Fatal("want an error")
		}
		if got := s.requests(); len(got) != 1 {
			t.Fatalf("requests = %d", len(got))
		}
	})
}

// A slow model gets a longer timeout on the next attempt instead of timing
// out five times in a row.
func TestTimeoutGrows(t *testing.T) {
	s := &tuneServer{handle: func(n int, _ map[string]any, w http.ResponseWriter) {
		time.Sleep(120 * time.Millisecond)
		answer(w, "ok", "stop")
	}}
	c, warnings := tuneClient(t, s, func(c *llm.Config) { c.Timeout = 80 * time.Millisecond })
	if _, err := c.Complete(context.Background(), llm.Request{User: "hi"}); err != nil {
		t.Fatalf("err = %v (warnings %q)", err, *warnings)
	}
	if len(*warnings) != 1 {
		t.Fatalf("warnings = %q", *warnings)
	}
}
