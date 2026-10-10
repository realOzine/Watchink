package llm

import (
	"regexp"
	"strings"
)

var (
	thinkRe = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)
	fenceRe = regexp.MustCompile("(?s)```[A-Za-z0-9_-]*[ \t]*\r?\n(.*?)\r?\n?[ \t]*```")
)

// stripThink removes reasoning blocks some models emit inline. A block
// that is never closed has no usable answer after it, so it is dropped whole.
func stripThink(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	lower := strings.ToLower(s)
	// Some chat templates swallow the opening tag and leave only "</think>".
	if i := strings.LastIndex(lower, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
		lower = lower[i+len("</think>"):]
	}
	for _, open := range []string{"<think>", "<thinking>"} {
		if i := strings.Index(lower, open); i >= 0 {
			s = s[:i]
			lower = lower[:i]
		}
	}
	return strings.TrimSpace(s)
}

// stripCodeFence returns the body of the first Markdown code fence when the
// text contains one, otherwise the trimmed text.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if m := fenceRe.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	// An opening fence without a closing one (truncated output).
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			return strings.TrimSpace(s[i+1:])
		}
	}
	return s
}

// CleanContent applies the clean-up to raw model text: <think> blocks and
// Markdown code fences are removed. Response.Content is already cleaned; this
// is exported for callers that hold raw text from elsewhere.
func CleanContent(s string) string { return stripCodeFence(stripThink(s)) }
