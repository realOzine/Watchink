package notes

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"strings"
	"text/template"

	"watchink/internal/app"
	"watchink/internal/llm"
)

// Prompt templates are embedded; v0 has no user-defined templates.
// Any change to them must bump PromptVersion.
//
//go:embed prompts/*.tmpl
var promptFS embed.FS

var prompts = template.Must(template.ParseFS(promptFS, "prompts/*.tmpl"))

func renderPrompt(name string, data any) string {
	var b strings.Builder
	if err := prompts.ExecuteTemplate(&b, name, data); err != nil {
		// Templates and their data are both compiled in; a failure here is a
		// programming error caught by the tests.
		panic("notes: prompt template " + name + ": " + err.Error())
	}
	return strings.TrimSpace(b.String())
}

// promptText prepares untrusted text (transcript, titles, model output from an
// earlier step) for a prompt: one line, and unable to close a delimited block.
func promptText(s string) string {
	s = oneLine(s)
	s = strings.ReplaceAll(s, "<<<", "‹‹‹")
	return strings.ReplaceAll(s, ">>>", "›››")
}

// errNeedSplit means the request did not fit (context overflow) or the answer
// was cut off (finish_reason=length): the caller must retry with less input.
var errNeedSplit = errors.New("notes: model input or output too large")

// callStats accumulates model usage across the calls of one operation.
type callStats struct {
	calls, repairs                 int
	promptTokens, completionTokens int
}

// ask performs one completion and maps both "too large" conditions to
// errNeedSplit.
func ask(ctx context.Context, c llm.Client, st *callStats, system, user string, schema *llm.Schema, maxTokens int) (string, error) {
	st.calls++
	resp, err := c.Complete(ctx, llm.Request{System: system, User: user, Schema: schema, MaxTokens: maxTokens})
	if err != nil {
		if llm.IsContextOverflow(err) {
			return "", errNeedSplit
		}
		return "", err
	}
	st.promptTokens += resp.Usage.PromptTokens
	st.completionTokens += resp.Usage.CompletionTokens
	if resp.Truncated() {
		return "", errNeedSplit
	}
	return resp.Content, nil
}

// decodeJSON strictly decodes one JSON object. Content has already lost code
// fences and <think> blocks; as a last tolerance for prompt-only mode,
// text around the outermost braces is ignored.
func decodeJSON(content string, v any) error {
	content = strings.TrimSpace(content)
	err := json.Unmarshal([]byte(content), v)
	if err == nil {
		return nil
	}
	i, j := strings.IndexByte(content, '{'), strings.LastIndexByte(content, '}')
	if i >= 0 && j > i && (i > 0 || j < len(content)-1) {
		if json.Unmarshal([]byte(content[i:j+1]), v) == nil {
			return nil
		}
	}
	return err
}

// askJSON asks for a JSON object of type T, with one repair call when the
// answer cannot be decoded (the reduce-side counterpart of the rule).
func askJSON[T any](ctx context.Context, c llm.Client, st *callStats, system, user string, schema *llm.Schema, maxTokens int) (*T, error) {
	content, err := ask(ctx, c, st, system, user, schema, maxTokens)
	if err != nil {
		return nil, err
	}
	out := new(T)
	derr := decodeJSON(content, out)
	if derr == nil {
		return out, nil
	}
	st.repairs++
	repair := "\n\n" + renderPrompt("repair.tmpl", map[string]string{
		"Problem": "输出不是合法的 JSON（" + truncateRunes(derr.Error(), 120) + "）。",
		"Advice":  "",
	})
	content, err = ask(ctx, c, st, system, user+repair, schema, maxTokens)
	if err != nil {
		return nil, err
	}
	out = new(T)
	if derr = decodeJSON(content, out); derr != nil {
		return nil, app.E(app.CodeModelOutputInvalid, "模型输出经过一次修复后仍不是合法的 JSON").
			WithHint("重试该任务；如果反复出现，换用结构化输出更稳定的模型").
			WithDetail("parse_error", truncateRunes(derr.Error(), 200)).WithCause(derr)
	}
	return out, nil
}
