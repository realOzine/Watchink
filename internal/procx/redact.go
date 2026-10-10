package procx

import (
	"regexp"
	"sort"
	"strings"
)

// redactMask replaces every masked value.
const redactMask = "***"

// The generic patterns below implement the rule that Authorization
// headers, API keys, tokens and login codes never reach logs or error details.
var (
	// "Authorization: Bearer abc", "authorization=Basic abc", "Authorization: abc".
	reAuthHeader = regexp.MustCompile(`(?i)(\bauthorization["']?\s*[:=]\s*["']?)(?:(bearer|basic|token)\s+)?[^\s"',;]+`)
	// A bare "Bearer abc" outside a header line.
	reBearer = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=\-]{6,}`)
	// OpenAI-style keys: sk-..., sk-proj-..., sk-ant-...
	reSKKey = regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`)
	// key=value / "key": "value" assignments of credential-like names.
	reAssign = regexp.MustCompile(`(?i)\b((?:[a-z0-9]+[_\-])*(?:api[_\-]?key|apikey|access[_\-]?token|refresh[_\-]?token|auth[_\-]?token|token|secret|password|passwd)["']?\s*[:=]\s*["']?)[^\s&"',;]+`)
	// Media token and login code carried as URL query values.
	reQuery = regexp.MustCompile(`(?i)([?&](?:mt|code|key|sig|signature)=)[^&\s"'#]+`)
)

// Redact masks the given secrets and well-known credential patterns in s:
// Authorization headers, Bearer tokens, sk-... keys, api_key=/token=
// assignments and the mt= / code= query values. It is safe to call on
// arbitrary tool output; text without credentials is returned unchanged.
func Redact(s string, secrets ...string) string {
	if s == "" {
		return s
	}
	// Longest first, so a secret that contains another one is fully masked.
	sorted := make([]string, 0, len(secrets))
	for _, sec := range secrets {
		// Very short values would mask unrelated text and are not secrets.
		if len(strings.TrimSpace(sec)) >= 4 {
			sorted = append(sorted, sec)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, sec := range sorted {
		s = strings.ReplaceAll(s, sec, redactMask)
	}
	s = reAuthHeader.ReplaceAllStringFunc(s, func(m string) string {
		sub := reAuthHeader.FindStringSubmatch(m)
		if sub[2] != "" {
			return sub[1] + sub[2] + " " + redactMask
		}
		return sub[1] + redactMask
	})
	s = reBearer.ReplaceAllString(s, "$1 "+redactMask)
	s = reSKKey.ReplaceAllString(s, "sk-"+redactMask)
	s = reAssign.ReplaceAllString(s, "${1}"+redactMask)
	s = reQuery.ReplaceAllString(s, "${1}"+redactMask)
	return s
}
