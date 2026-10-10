package llm

import (
	"context"
	"encoding/json"
	"time"

	"watchink/internal/app"
)

type pingOutput struct {
	OK bool `json:"ok" desc:"固定为 true"`
}

// PingMarker is the first line of the connectivity-test prompt; the fake
// server in llmtest recognises it.
const PingMarker = "[watchink:task=ping]"

// Test sends one tiny structured request and reports whether the endpoint is
// reachable, the credentials work, and which structured-output level was
// accepted (POST /api/v1/settings/llm/test). It never returns nil.
func Test(ctx context.Context, cfg Config) *app.LLMTestResult {
	res := &app.LLMTestResult{Model: cfg.Model}
	if cfg.MaxAttempts == 0 {
		// A settings test should answer quickly instead of backing off for
		// half a minute.
		cfg.MaxAttempts = 2
	}
	start := time.Now()
	fail := func(err error) *app.LLMTestResult {
		res.LatencyMs = time.Since(start).Milliseconds()
		res.Error = app.AsError(err)
		if ctx.Err() != nil && res.Error.Code == app.CodeInternal {
			res.Error = app.E(app.CodeModelUnavailable, "测试请求被取消或超时").WithCause(err)
		}
		return res
	}
	c, err := New(cfg)
	if err != nil {
		return fail(err)
	}
	resp, err := c.Complete(ctx, Request{
		System:    "你是一个接口连通性测试助手，只输出 JSON。",
		User:      PingMarker + "\n请输出 JSON 对象 {\"ok\": true}。",
		Schema:    SchemaFor("ping", pingOutput{}),
		MaxTokens: 512,
	})
	if err != nil {
		return fail(err)
	}
	res.StructuredOutput = resp.StructuredOutput
	var out map[string]any
	if err := json.Unmarshal([]byte(resp.Content), &out); err != nil {
		return fail(app.E(app.CodeModelOutputInvalid, "模型可以连通，但没有按要求返回 JSON").
			WithHint("换用支持 JSON 输出的模型，或把 llm.structured_output 设为 prompt 后再试").
			WithDetail("structured_output", resp.StructuredOutput))
	}
	res.OK = true
	res.LatencyMs = time.Since(start).Milliseconds()
	return res
}
