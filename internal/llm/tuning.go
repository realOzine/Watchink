package llm

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Self-healing request parameters.
//
// OpenAI-compatible endpoints disagree on details that have nothing to do
// with the task: reasoning models spend the output budget on thinking and
// return finish_reason=length with little or no content, some models only
// accept max_completion_tokens, some reject a temperature, some cap
// max_tokens below what we ask for, and slow models need more than the
// default timeout. None of these should fail a job: the client adjusts the
// parameter, reports a warning and tries again, and remembers what worked
// per (base_url, model) for the lifetime of the process.

const (
	// maxAdaptRounds bounds the adjustments made within one Complete call.
	maxAdaptRounds = 6
	// maxTokenScale is the largest multiplier tried before max_tokens is
	// dropped altogether (the server default is then the model's maximum).
	maxTokenScale = 4
	// maxTimeoutScale caps the automatic timeout growth (4 × 120 s = 8 min).
	maxTimeoutScale = 4
	minTokenCap     = 64
)

// tuning is the set of adjustments in effect for one endpoint + model.
type tuning struct {
	// tokenScale multiplies Request.MaxTokens; 0 means 1.
	tokenScale int
	// noMaxTokens omits the output limit entirely.
	noMaxTokens bool
	// tokenCap is an upper bound the endpoint reported for the limit.
	tokenCap int
	// completionParam sends max_completion_tokens instead of max_tokens.
	completionParam bool
	// noTemperature omits the temperature.
	noTemperature bool
	// timeoutScale multiplies the per-attempt timeout; 0 means 1.
	timeoutScale int
}

// maxTokens returns the output limit to send for a request asking for want
// tokens; 0 means "send none".
func (t tuning) maxTokens(want int) int {
	if want <= 0 || t.noMaxTokens {
		return 0
	}
	n := want * max(t.tokenScale, 1)
	if t.tokenCap > 0 && n > t.tokenCap {
		n = t.tokenCap
	}
	return n
}

func (t tuning) timeout(base time.Duration) time.Duration {
	return base * time.Duration(max(t.timeoutScale, 1))
}

// moreTokens returns the next larger output budget after a truncated answer,
// or ok=false when there is nothing larger to try.
func (t tuning) moreTokens(want int) (next tuning, note string, ok bool) {
	cur := t.maxTokens(want)
	if cur == 0 {
		return t, "", false // already unlimited
	}
	next = t
	if scale := max(t.tokenScale, 1) * 2; scale <= maxTokenScale {
		next.tokenScale = scale
		if n := next.maxTokens(want); n > cur {
			return next, fmt.Sprintf("输出在 %d token 处被截断，已把输出上限提高到 %d 后重试", cur, n), true
		}
	}
	if t.tokenCap > 0 {
		// The endpoint told us its ceiling; removing the limit cannot help.
		return t, "", false
	}
	next.noMaxTokens = true
	return next, fmt.Sprintf("输出在 %d token 处被截断，已取消输出上限（使用模型默认值）后重试", cur), true
}

// moreTime returns the next larger timeout after a timed-out attempt.
func (t tuning) moreTime(base time.Duration) (next tuning, note string, ok bool) {
	scale := max(t.timeoutScale, 1) * 2
	if scale > maxTimeoutScale {
		return t, "", false
	}
	next = t
	next.timeoutScale = scale
	return next, fmt.Sprintf("请求超过 %s 未完成，已把单次请求超时延长到 %s 后重试",
		t.timeout(base).Round(time.Second), next.timeout(base).Round(time.Second)), true
}

var numberRe = regexp.MustCompile(`\d{2,7}`)

// adaptToRejection inspects a 4xx body for a complaint about a request
// parameter we can change. sent is the output limit of the rejected request.
func (t tuning) adaptToRejection(body string, sent int) (next tuning, note string, ok bool) {
	lower := strings.ToLower(body)
	next = t
	switch {
	case strings.Contains(lower, "max_completion_tokens") && !t.completionParam && sent > 0:
		// e.g. "Unsupported parameter: 'max_tokens' … Use 'max_completion_tokens' instead."
		next.completionParam = true
		return next, "接口不接受 max_tokens，已改用 max_completion_tokens 后重试", true

	case (strings.Contains(lower, "max_tokens") || strings.Contains(lower, "max_completion_tokens") ||
		strings.Contains(lower, "max tokens") || strings.Contains(lower, "maximum tokens")) && sent > 0:
		// The limit is out of range: use the largest bound the message
		// names that is below what we sent, else halve.
		limit := 0
		for _, m := range numberRe.FindAllString(lower, -1) {
			if n, err := strconv.Atoi(m); err == nil && n >= minTokenCap && n < sent && n > limit {
				limit = n
			}
		}
		if limit == 0 {
			limit = sent / 2
		}
		if limit < minTokenCap {
			if t.noMaxTokens {
				return t, "", false
			}
			next.noMaxTokens = true
			return next, "接口不接受当前的输出上限，已取消输出上限后重试", true
		}
		next.tokenCap = limit
		return next, fmt.Sprintf("接口不接受 %d 的输出上限，已降到 %d 后重试", sent, limit), true

	case strings.Contains(lower, "temperature") && !t.noTemperature:
		next.noTemperature = true
		return next, "接口不接受当前的 temperature，已改用模型默认值后重试", true
	}
	return t, "", false
}

// tuningCache remembers the adjustments that worked, per (base_url, model).
var tuningCache = struct {
	sync.Mutex
	m map[string]tuning
}{m: map[string]tuning{}}

func cachedTuning(key string) tuning {
	tuningCache.Lock()
	defer tuningCache.Unlock()
	return tuningCache.m[key]
}

func rememberTuning(key string, t tuning) {
	tuningCache.Lock()
	defer tuningCache.Unlock()
	tuningCache.m[key] = t
}

func resetTuningCache() {
	tuningCache.Lock()
	defer tuningCache.Unlock()
	tuningCache.m = map[string]tuning{}
}
