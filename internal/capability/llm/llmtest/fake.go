// Package llmtest provides a scriptable fake OpenAI-compatible Chat
// Completions server. It is used by unit tests and by
// end-to-end runs that must not need a real API key.
//
// By default it recognises the prompts produced by internal/capability/notes (by their
// first-line task marker and their delimited blocks) and answers with valid,
// plausible JSON that only cites ids present in the prompt. A queue of Steps
// overrides the behaviour of individual calls to inject failures.
package llmtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Task names, as found in the "[watchink:task=<name>]" first line of a
// user prompt.
const (
	TaskMap         = "map"
	TaskReduce      = "reduce"
	TaskReduceGroup = "reduce_group"
	TaskReduceFinal = "reduce_final"
	TaskPing        = "ping"
	TaskOther       = "other"
)

// Step kinds.
const (
	KindOK               = "ok"                 // default behaviour (useful with delay_ms or match)
	KindInvalidJSON      = "invalid_json"       // 200 with content that is not JSON
	KindStatus           = "status"             // arbitrary Status / Body / RetryAfter
	KindAuthError        = "auth_error"         // 401
	KindRateLimit        = "rate_limit"         // 429, honours RetryAfter
	KindServerError      = "server_error"       // 500
	KindContextOverflow  = "context_overflow"   // 400 context_length_exceeded
	KindLength           = "length"             // 200, truncated content, finish_reason=length
	KindRejectJSONSchema = "reject_json_schema" // 400 if the call uses json_schema, else ok
	KindRejectJSONObject = "reject_json_object" // 400 if the call uses json_object, else ok
	KindBadCites         = "bad_cites"          // every id cited is out of range
	KindMixedCites       = "mixed_cites"        // each point cites one valid and one invalid id
	KindEmptyPoints      = "empty_points"       // map answer without points
	KindOmitPoints       = "omit_points"        // reduce answer that leaves every other point unplaced
	KindThinkFenced      = "think_fenced"       // valid answer wrapped in <think> and a code fence
	KindContent          = "content"            // literal Content (and optional FinishReason)
	KindDropConnection   = "drop_connection"    // close the TCP connection without answering
	KindSlow             = "slow"               // alias of ok; use with delay_ms
)

// Step scripts the behaviour of one call (or of Times consecutive matching
// calls).
type Step struct {
	Kind string `json:"kind"`
	// Match restricts the step to one task (map / reduce / reduce_group /
	// reduce_final / ping); empty matches any call.
	Match string `json:"match,omitempty"`
	// Times is how many calls the step serves; 0 means 1.
	Times        int    `json:"times,omitempty"`
	Status       int    `json:"status,omitempty"`
	RetryAfter   string `json:"retry_after,omitempty"`
	Body         string `json:"body,omitempty"`
	Content      string `json:"content,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
	DelayMs      int    `json:"delay_ms,omitempty"`
}

// Script is the JSON file format accepted by LoadScript and the fakellm
// command.
type Script struct {
	// APIKey, when set, makes the server answer 401 to any other bearer token.
	APIKey string `json:"api_key,omitempty"`
	// RejectJSONSchema / RejectJSONObject make the server reject that
	// response_format on every call (an endpoint without the feature).
	RejectJSONSchema bool `json:"reject_json_schema,omitempty"`
	RejectJSONObject bool `json:"reject_json_object,omitempty"`
	// OverflowAboveSegments > 0 answers "context length exceeded" to map
	// calls whose citable block has more lines than this.
	OverflowAboveSegments int `json:"overflow_above_segments,omitempty"`
	// LengthAboveSegments > 0 answers finish_reason=length to such map calls.
	LengthAboveSegments int    `json:"length_above_segments,omitempty"`
	Steps               []Step `json:"steps,omitempty"`
}

// Recorded describes one received chat completion call.
type Recorded struct {
	Path           string
	Authorization  string // "Bearer ***" style presence marker, never the key
	HasKey         bool
	Model          string
	Task           string
	ResponseFormat string // json_schema / json_object / "" (none)
	SchemaName     string
	System         string
	User           string
	MaxTokens      int
	Temperature    *float64
	StepKind       string // the scripted step applied, or "" for default
}

// Server is the fake endpoint. The zero value is not usable; call New.
type Server struct {
	mu       sync.Mutex
	script   Script
	requests []Recorded
}

// New returns a fake server with default behaviour.
func New() *Server { return &Server{} }

// Configure replaces the persistent options (everything in Script except the
// steps, which are appended to the queue).
func (s *Server) Configure(sc Script) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	steps := append(s.script.Steps, sc.Steps...)
	s.script = sc
	s.script.Steps = steps
	return s
}

// Enqueue appends scripted steps.
func (s *Server) Enqueue(steps ...Step) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script.Steps = append(s.script.Steps, steps...)
	return s
}

// LoadScript reads a Script JSON file and applies it with Configure.
func (s *Server) LoadScript(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var sc Script
	if err := json.Unmarshal(raw, &sc); err != nil {
		return fmt.Errorf("parse script %s: %w", path, err)
	}
	s.Configure(sc)
	return nil
}

// Requests returns a copy of every chat completion call received so far.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Recorded(nil), s.requests...)
}

// Calls counts received calls of a task ("" counts all).
func (s *Server) Calls(task string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if task == "" || r.Task == task {
			n++
		}
	}
	return n
}

// Pending is the number of scripted calls not yet consumed.
func (s *Server) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, st := range s.script.Steps {
		n += max(st.Times, 1)
	}
	return n
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	MaxTokens      int      `json:"max_tokens"`
	Temperature    *float64 `json:"temperature"`
	ResponseFormat *struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name string `json:"name"`
		} `json:"json_schema"`
	} `json:"response_format"`
}

var taskRe = regexp.MustCompile(`^\[watchink:task=([a-z_]+)\]`)

// ServeHTTP implements http.Handler. Any path ending in /chat/completions is
// served; /models answers a minimal model list.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	switch {
	case strings.HasSuffix(path, "/models") && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []map[string]any{{"id": "fake-model", "object": "model"}}})
		return
	case !strings.HasSuffix(path, "/chat/completions"):
		writeError(w, http.StatusNotFound, "not_found", "unknown path "+r.URL.Path)
		return
	case r.Method != http.MethodPost:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}
	rec := Recorded{Path: r.URL.Path, Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature, Task: TaskOther}
	auth := r.Header.Get("Authorization")
	key := strings.TrimPrefix(auth, "Bearer ")
	if auth != "" {
		rec.HasKey = true
		rec.Authorization = "Bearer ***"
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			rec.System = m.Content
		case "user":
			rec.User = m.Content
		}
	}
	if m := taskRe.FindStringSubmatch(rec.User); m != nil {
		rec.Task = m[1]
	}
	if req.ResponseFormat != nil {
		rec.ResponseFormat = req.ResponseFormat.Type
		if req.ResponseFormat.JSONSchema != nil {
			rec.SchemaName = req.ResponseFormat.JSONSchema.Name
		}
	}

	s.mu.Lock()
	cfg := s.script
	step, scripted := s.popStepLocked(rec.Task)
	if scripted {
		rec.StepKind = step.Kind
	}
	s.requests = append(s.requests, rec)
	s.mu.Unlock()

	if step.DelayMs > 0 {
		select {
		case <-time.After(time.Duration(step.DelayMs) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
	if cfg.APIKey != "" && key != cfg.APIKey {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "Incorrect API key provided.")
		return
	}
	rejectSchema := cfg.RejectJSONSchema || step.Kind == KindRejectJSONSchema
	rejectObject := cfg.RejectJSONObject || step.Kind == KindRejectJSONObject
	if rejectSchema && rec.ResponseFormat == "json_schema" {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"Invalid parameter: 'response_format' of type 'json_schema' is not supported with this model.")
		return
	}
	if rejectObject && rec.ResponseFormat == "json_object" {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"response_format json_object is unsupported by this server")
		return
	}

	switch step.Kind {
	case KindStatus:
		status := step.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		if step.RetryAfter != "" {
			w.Header().Set("Retry-After", step.RetryAfter)
		}
		if step.Body != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(step.Body))
			return
		}
		writeError(w, status, "scripted_error", fmt.Sprintf("scripted HTTP %d", status))
		return
	case KindAuthError:
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "Incorrect API key provided.")
		return
	case KindRateLimit:
		if step.RetryAfter != "" {
			w.Header().Set("Retry-After", step.RetryAfter)
		}
		writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "Rate limit reached, please slow down.")
		return
	case KindServerError:
		writeError(w, http.StatusInternalServerError, "server_error", "The server had an error while processing your request.")
		return
	case KindContextOverflow:
		writeOverflow(w)
		return
	case KindDropConnection:
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		writeError(w, http.StatusBadGateway, "server_error", "connection dropped")
		return
	}

	p := parsePrompt(rec.Task, rec.User)
	if rec.Task == TaskMap && step.Kind == "" {
		if cfg.OverflowAboveSegments > 0 && len(p.segs) > cfg.OverflowAboveSegments {
			writeOverflow(w)
			return
		}
		if cfg.LengthAboveSegments > 0 && len(p.segs) > cfg.LengthAboveSegments {
			step.Kind = KindLength
		}
	}

	content := p.answer(step.Kind)
	finish := "stop"
	switch step.Kind {
	case KindInvalidJSON:
		content = `{"chunk_summary": "这段输出不是合法的 JSON, "points": [`
	case KindLength:
		content = content[:len(content)/2]
		finish = "length"
	case KindThinkFenced:
		content = "<think>\n先想一想应该怎么整理这些内容……\n</think>\n\n```json\n" + content + "\n```"
	case KindContent:
		content = step.Content
	}
	if step.FinishReason != "" {
		finish = step.FinishReason
	}
	prompt := (len(rec.System) + len(rec.User)) / 3
	completion := len(content) / 3
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-fake",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": finish,
		}},
		"usage": map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion},
	})
}

// popStepLocked takes the first queued step that matches the task.
func (s *Server) popStepLocked(task string) (Step, bool) {
	for i := range s.script.Steps {
		st := s.script.Steps[i]
		if st.Match != "" && st.Match != task {
			continue
		}
		if st.Times > 1 {
			s.script.Steps[i].Times--
		} else {
			s.script.Steps = append(s.script.Steps[:i:i], s.script.Steps[i+1:]...)
		}
		return st, true
	}
	return Step{}, false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": msg, "type": "invalid_request_error", "code": code,
	}})
}

func writeOverflow(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, "context_length_exceeded",
		"This model's maximum context length is 8192 tokens. However, your messages resulted in 20480 tokens. Please reduce the length of the messages.")
}
