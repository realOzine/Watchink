package llm

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"watchink/internal/app"
)

// ErrContextOverflow is the cause of an error returned when the endpoint
// rejected the request because the prompt exceeds the model's context window.
var ErrContextOverflow = errors.New("llm: context length exceeded")

// IsContextOverflow reports whether err means "the input does not fit the
// model's context window". Callers react by splitting their input (§9.2).
func IsContextOverflow(err error) bool { return errors.Is(err, ErrContextOverflow) }

type errKind int

const (
	kindCancelled   errKind = iota // caller's context ended / gate refused
	kindAuth                       // 401, 403
	kindNotFound                   // 404 or a "model not found" body
	kindRateLimit                  // 429
	kindServer                     // 5xx
	kindNetwork                    // timeout, connection error
	kindBadResponse                // 2xx with an unusable body
	kindOverflow                   // context length exceeded
	kindFormat                     // response_format level not supported
	kindBadRequest                 // any other 4xx
)

// callErr is the internal classification of one failed attempt.
type callErr struct {
	kind          errKind
	status        int
	body          string // redacted excerpt
	retryAfter    time.Duration
	hasRetryAfter bool
	cause         error
	// timedOut is set when our own per-attempt timeout ended the attempt.
	timedOut bool
}

func (e *callErr) retryable() bool {
	switch e.kind {
	case kindRateLimit, kindServer, kindNetwork, kindBadResponse:
		return true
	}
	return false
}

var overflowMarkers = []string{
	"context_length_exceeded", "context length", "maximum context", "context window",
	"too many tokens", "prompt is too long", "reduce the length", "token limit",
	"exceeds the maximum", "input is too long", "上下文长度",
}

var modelMissingMarkers = []string{
	"model_not_found", "model not found", "no such model", "unknown model",
	"invalid model", "模型不存在",
}

var formatMarkers = []string{
	"response_format", "json_schema", "json_object", "schema", "unsupported", "not supported",
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// classify maps a non-2xx status and its body to an error kind. sentFormat
// tells whether the request carried a response_format, which is the only case
// in which a 400 can mean "this structured-output level is not supported".
func classify(status int, body string, sentFormat bool) errKind {
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return kindAuth
	case status == http.StatusNotFound:
		return kindNotFound
	case status == http.StatusTooManyRequests:
		return kindRateLimit
	case status == http.StatusRequestTimeout:
		return kindNetwork
	case status >= 500:
		return kindServer
	case status == http.StatusRequestEntityTooLarge:
		return kindOverflow
	}
	// Order matters: an overflow message may also mention "schema" or
	// "unsupported", and must never be mistaken for a format rejection.
	if containsAny(lower, overflowMarkers) {
		return kindOverflow
	}
	if containsAny(lower, modelMissingMarkers) ||
		(strings.Contains(lower, "model") && strings.Contains(lower, "does not exist")) {
		return kindNotFound
	}
	if sentFormat && (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) &&
		containsAny(lower, formatMarkers) {
		return kindFormat
	}
	return kindBadRequest
}

// appError converts the classification into the contract error (§7.7).
func (e *callErr) appError(attempts int) error {
	var out *app.Error
	switch e.kind {
	case kindCancelled:
		return e.cause
	case kindAuth:
		out = app.E(app.CodeModelAuthFailed, "在线模型鉴权失败（HTTP %d）", e.status).
			WithHint("在设置中更新 API Key 后重试该任务")
	case kindNotFound:
		out = app.E(app.CodeModelNotFound, "在线模型不存在或接口地址有误（HTTP %d）", e.status).
			WithHint("检查设置中的模型名和 base_url（通常以 /v1 结尾）")
	case kindRateLimit:
		out = app.E(app.CodeModelRateLimited, "在线模型持续限流（HTTP 429），自动重试已耗尽").
			WithHint("稍后重试该任务，或降低 llm.max_concurrency")
	case kindServer:
		out = app.E(app.CodeModelUnavailable, "在线模型服务异常（HTTP %d），自动重试已耗尽", e.status).
			WithHint("稍后重试该任务")
	case kindNetwork:
		out = app.E(app.CodeModelUnavailable, "连接在线模型失败或超时，自动重试已耗尽").
			WithHint("检查网络、代理和 base_url 后重试该任务")
	case kindBadResponse:
		out = app.E(app.CodeModelUnavailable, "在线模型返回了无法识别的响应").
			WithHint("确认 base_url 指向 OpenAI 兼容的 Chat Completions 接口")
	case kindOverflow:
		out = app.E(app.CodeModelOutputInvalid, "输入超出了模型的上下文长度").
			WithHint("调小 note.chunk_max_tokens，或换用上下文更长的模型").
			WithCause(ErrContextOverflow)
	case kindFormat:
		out = app.E(app.CodeModelUnavailable, "在线模型不支持当前的结构化输出方式（HTTP %d）", e.status).
			WithHint("把 llm.structured_output 设为 auto 后重试")
	default:
		out = app.E(app.CodeModelUnavailable, "在线模型拒绝了请求（HTTP %d）", e.status).
			WithHint("检查模型名、base_url 和模型参数是否受该接口支持")
	}
	if e.status != 0 {
		out.WithDetail("http_status", e.status)
	}
	if e.body != "" {
		out.WithDetail("body", e.body)
	}
	if attempts > 0 {
		out.WithDetail("attempts", attempts)
	}
	if e.cause != nil && e.kind != kindOverflow {
		out.WithCause(e.cause)
		if e.kind == kindNetwork {
			out.WithDetail("cause", redact(e.cause.Error(), ""))
		}
	}
	return out
}

var (
	bearerRe = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)
	skKeyRe  = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{6,}`)
)

// redact removes credentials from text that ends up in error details (§15).
func redact(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "***")
	}
	s = bearerRe.ReplaceAllString(s, "Bearer ***")
	return skKeyRe.ReplaceAllString(s, "sk-***")
}

// excerpt is a short, single-line, redacted piece of a response body.
func (c *client) excerpt(body string) string {
	body = strings.Join(strings.Fields(redact(body, c.cfg.APIKey)), " ")
	if r := []rune(body); len(r) > excerptRunes {
		body = string(r[:excerptRunes]) + "…"
	}
	return body
}
