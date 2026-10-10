// Package notes turns a transcript into a cited note: chunk → map → reduce →
// validate/complete → render → export.
//
// The model only ever proposes text and ids. Everything that must be true —
// citation targets, timestamps, that no valid point is silently lost — is
// computed and checked by this package.
package notes

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"watchink/internal/app"
)

// Versions that take part in fingerprints. Bump PromptVersion
// whenever a prompt template or the map/reduce validation changes, and
// RendererVersion whenever the rendered Markdown or Document changes.
const (
	PromptVersion   = "note-v0.1"
	RendererVersion = "render-v0.1"
)

// Chunking defaults and constants.
const (
	DefaultChunkMaxTokens   = 3000
	DefaultChunkMaxDuration = 10 * time.Minute

	cutGapMs  = 1500  // preferred cut points are silences at least this long
	contextMs = 30000 // trailing context taken from the previous chunk
)

// Options controls chunking (note.chunk_max_tokens, note.chunk_max_duration).
type Options struct {
	MaxTokens   int           // 0 means DefaultChunkMaxTokens
	MaxDuration time.Duration // 0 means DefaultChunkMaxDuration
}

// Chunk is a run of consecutive transcript segments handed to one map call.
type Chunk struct {
	// Context is the tail (about 30 s) of the previous chunk. It is shown to
	// the model for understanding only and must not be cited.
	Context []app.Segment `json:"context,omitempty"`
	// Segments are the citable segments, in transcript order.
	Segments []app.Segment `json:"segments"`
	// Tokens is the estimated token count of Segments.
	Tokens int `json:"tokens"`
}

// Key is a stable description of the chunk boundaries: the first and last
// citable idx and the first context idx. Together with the transcript ID it
// identifies the map input, so it is suitable for fingerprints.
func (c Chunk) Key() string {
	if len(c.Segments) == 0 {
		return "S0-S0"
	}
	key := fmt.Sprintf("S%d-S%d", c.Segments[0].Idx, c.Segments[len(c.Segments)-1].Idx)
	if len(c.Context) > 0 {
		key += fmt.Sprintf("+ctxS%d", c.Context[0].Idx)
	}
	return key
}

// StartMs is the start of the first citable segment.
func (c Chunk) StartMs() int64 {
	if len(c.Segments) == 0 {
		return 0
	}
	return c.Segments[0].StartMs
}

// EndMs is the end of the last citable segment.
func (c Chunk) EndMs() int64 {
	if len(c.Segments) == 0 {
		return 0
	}
	return c.Segments[len(c.Segments)-1].EndMs
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// countText returns the number of CJK characters and of whitespace-separated
// non-CJK words in s.
func countText(s string) (cjk, words int) {
	inWord := false
	for _, r := range s {
		switch {
		case isCJK(r):
			cjk++
			inWord = false
		case unicode.IsSpace(r):
			inWord = false
		default:
			if !inWord {
				words++
				inWord = true
			}
		}
	}
	return cjk, words
}

// EstimateTokens is the estimate: one per CJK character plus 1.3 per
// whitespace-separated word of other text. It is only used for budgeting.
func EstimateTokens(s string) int {
	cjk, words := countText(s)
	return cjk + int(math.Ceil(float64(words)*1.3))
}

// ChunkSegments splits a transcript into chunks. Segments are processed in
// Idx order; a segment is never split, so a single segment larger than a
// limit forms a chunk of its own. The result is a pure function of the input,
// which keeps chunk boundaries (and the fingerprints built on them) stable.
func ChunkSegments(in []app.Segment, opt Options) []Chunk {
	if len(in) == 0 {
		return nil
	}
	maxTok := opt.MaxTokens
	if maxTok <= 0 {
		maxTok = DefaultChunkMaxTokens
	}
	maxDur := opt.MaxDuration.Milliseconds()
	if maxDur <= 0 {
		maxDur = DefaultChunkMaxDuration.Milliseconds()
	}
	segs := append([]app.Segment(nil), in...)
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].Idx < segs[j].Idx })

	n := len(segs)
	prefix := make([]int, n+1) // prefix[i] = tokens of segs[:i]
	for i, s := range segs {
		prefix[i+1] = prefix[i] + EstimateTokens(s.Text)
	}

	var out []Chunk
	for i := 0; i < n; {
		// Extend as far as both limits allow; the first segment always fits.
		j := i + 1
		for j < n && prefix[j+1]-prefix[i] <= maxTok && segs[j].EndMs-segs[i].StartMs <= maxDur {
			j++
		}
		cut := j
		if j < n {
			// A limit was hit. Prefer the latest pause of ≥ 1.5 s, but only
			// in the back half of the budget so chunks do not become tiny.
			for k := j; k > i; k-- {
				if (prefix[k]-prefix[i])*2 < maxTok && (segs[k-1].EndMs-segs[i].StartMs)*2 < maxDur {
					break
				}
				if segs[k].StartMs-segs[k-1].EndMs >= cutGapMs {
					cut = k
					break
				}
			}
		}
		c := Chunk{Segments: segs[i:cut:cut], Tokens: prefix[cut] - prefix[i]}
		if i > 0 {
			c.Context = trailingContext(segs[:i], len(out[len(out)-1].Segments))
		}
		out = append(out, c)
		i = cut
	}
	return out
}

// trailingContext returns the last segments of prev that start within 30 s of
// its end (at least one), looking back at most limit segments.
func trailingContext(prev []app.Segment, limit int) []app.Segment {
	if len(prev) == 0 || limit <= 0 {
		return nil
	}
	end := prev[len(prev)-1].EndMs
	first := len(prev) - 1
	for first > len(prev)-limit && end-prev[first-1].StartMs <= contextMs {
		first--
	}
	return prev[first:len(prev):len(prev)]
}

// FormatTime renders a video-relative time: H:MM:SS when long (videos of an
// hour or more), otherwise MM:SS.
func FormatTime(ms int64, long bool) string {
	if ms < 0 {
		ms = 0
	}
	sec := ms / 1000
	if long {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%02d:%02d", sec/60, sec%60)
}

// IsLong reports whether times of a video of this duration use H:MM:SS.
func IsLong(durationMs int64) bool { return durationMs >= time.Hour.Milliseconds() }

// isOneLine reports whether s is already what oneLine would return: no
// control characters, no whitespace other than single interior spaces. ASR
// output almost always is, which lets the exporters skip the rewrite.
func isOneLine(s string) bool {
	prevSpace := true // a leading space is not allowed
	for _, r := range s {
		if r == ' ' {
			if prevSpace {
				return false
			}
			prevSpace = true
			continue
		}
		// RuneError: strings.Map would rewrite invalid UTF-8, so take the
		// slow path.
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
		prevSpace = false
	}
	return s == "" || !prevSpace
}

// oneLine collapses all whitespace (including newlines) and drops control
// characters, so a piece of text can never span lines.
func oneLine(s string) string {
	if isOneLine(s) {
		return s
	}
	return rewriteOneLine(s)
}

func rewriteOneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
