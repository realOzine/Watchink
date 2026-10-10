package llm

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCleanContent(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", ` {"a":1} `, `{"a":1}`},
		{"fenced json", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced bare", "```\n{\"a\":1}\n```\n", `{"a":1}`},
		{"prose around fence", "Here you go:\n```json\n{\"a\":1}\n```\nDone.", `{"a":1}`},
		{"think block", "<think>let me\nthink {\"x\":2}</think>\n{\"a\":1}", `{"a":1}`},
		{"thinking tag", "<THINKING>x</THINKING>{\"a\":1}", `{"a":1}`},
		{"think + fence", "<think>…</think>\n```json\n{\"a\":1}\n```", `{"a":1}`},
		{"closing tag only", "reasoning…</think>{\"a\":1}", `{"a":1}`},
		{"unclosed think", "{\"a\":1}<think>never ends", `{"a":1}`},
		{"unclosed fence", "```json\n{\"a\":", `{"a":`},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := CleanContent(tc.in); got != tc.want {
			t.Errorf("%s: CleanContent(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestSchemaFor(t *testing.T) {
	type item struct {
		Title string   `json:"title" desc:"标题 <30"`
		Kind  string   `json:"kind" enum:"a,b"`
		Cites []string `json:"cites"`
		N     int      `json:"n"`
		skip  int      //nolint:unused
		Omit  string   `json:"-"`
	}
	type root struct {
		Summary string  `json:"summary"`
		Items   []item  `json:"items"`
		Score   float64 `json:"score"`
		Done    *bool   `json:"done,omitempty"`
	}
	s := SchemaFor("root", root{})
	want := `{"type":"object","properties":{"summary":{"type":"string"},"items":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string","description":"标题 <30"},"kind":{"type":"string","enum":["a","b"]},"cites":{"type":"array","items":{"type":"string"}},"n":{"type":"integer"}},"required":["title","kind","cites","n"],"additionalProperties":false}},"score":{"type":"number"},"done":{"type":"boolean"}},"required":["summary","items","score","done"],"additionalProperties":false}`
	if string(s.Schema) != want || s.Name != "root" {
		t.Fatalf("schema mismatch:\n got %s\nwant %s", s.Schema, want)
	}
	if !json.Valid(s.Schema) {
		t.Fatal("schema is not valid JSON")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"3", 3 * time.Second, true},
		{"0.5", 500 * time.Millisecond, true},
		{"Fri, 09 Oct 2026 12:00:10 GMT", 10 * time.Second, true},
		{"Fri, 09 Oct 2026 11:00:00 GMT", 0, true},
		{"soon", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseRetryAfter(tc.in, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseRetryAfter(%q) = %v, %v", tc.in, got, ok)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		format bool
		want   errKind
	}{
		{400, `response_format is not supported`, true, kindFormat},
		{422, `Unknown field: json_schema`, true, kindFormat},
		{400, `unsupported parameter`, false, kindBadRequest},
		{400, `This model's maximum context length is 4096 tokens; json_schema`, true, kindOverflow},
		{413, ``, false, kindOverflow},
		{400, `model "abc" does not exist`, true, kindNotFound},
		{408, ``, false, kindNetwork},
		{503, ``, false, kindServer},
		{409, ``, false, kindBadRequest},
	}
	for _, tc := range cases {
		if got := classify(tc.status, tc.body, tc.format); got != tc.want {
			t.Errorf("classify(%d, %q, %v) = %v, want %v", tc.status, tc.body, tc.format, got, tc.want)
		}
	}
}
