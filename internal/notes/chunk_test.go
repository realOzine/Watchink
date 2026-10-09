package notes

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"watchink/internal/app"
)

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"   ", 0},
		{"你好世界", 4},
		{"hello world", 3},             // ceil(2 × 1.3)
		{"one two three four five", 7}, // ceil(5 × 1.3)
		{"学习 machine learning 方法", 7},  // 4 CJK + ceil(2 × 1.3)
		{"こんにちは", 5},
		{"한국어", 3},
		{"GPU加速", 4}, // 2 CJK + ceil(1 × 1.3)
	}
	for _, tc := range cases {
		if got := EstimateTokens(tc.in); got != tc.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// checkPartition asserts the chunks cover segs exactly once, in order.
func checkPartition(t *testing.T, segs []app.Segment, chunks []Chunk) {
	t.Helper()
	next := 0
	for ci, c := range chunks {
		if len(c.Segments) == 0 {
			t.Fatalf("chunk %d is empty", ci)
		}
		tokens := 0
		for _, s := range c.Segments {
			if next >= len(segs) || s.Idx != segs[next].Idx {
				t.Fatalf("chunk %d: segment idx %d out of order", ci, s.Idx)
			}
			tokens += EstimateTokens(s.Text)
			next++
		}
		if tokens != c.Tokens {
			t.Errorf("chunk %d: Tokens = %d, want %d", ci, c.Tokens, tokens)
		}
		if ci == 0 && len(c.Context) != 0 {
			t.Errorf("first chunk must have no context")
		}
		if ci > 0 {
			prev := chunks[ci-1].Segments
			if len(c.Context) == 0 {
				t.Errorf("chunk %d has no context", ci)
			}
			// Context is a suffix of the previous chunk and never citable.
			if !reflect.DeepEqual(c.Context, prev[len(prev)-len(c.Context):]) {
				t.Errorf("chunk %d: context is not the tail of the previous chunk", ci)
			}
			if span := prev[len(prev)-1].EndMs - c.Context[0].StartMs; len(c.Context) > 1 && span > contextMs {
				t.Errorf("chunk %d: context spans %d ms", ci, span)
			}
		}
	}
	if next != len(segs) {
		t.Fatalf("chunks cover %d of %d segments", next, len(segs))
	}
}

func sizes(chunks []Chunk) []int {
	out := make([]int, len(chunks))
	for i, c := range chunks {
		out[i] = len(c.Segments)
	}
	return out
}

func TestChunkEdgeCases(t *testing.T) {
	if got := ChunkSegments(nil, Options{}); got != nil {
		t.Fatalf("empty transcript: %v", got)
	}
	one := mkSegs(1, 5000, 0, zh)
	got := ChunkSegments(one, Options{})
	if len(got) != 1 || got[0].Key() != "S1-S1" || got[0].StartMs() != 0 || got[0].EndMs() != 5000 {
		t.Fatalf("single segment: %+v", got)
	}
}

func TestChunkDurationLimit(t *testing.T) {
	segs := mkSegs(100, 10000, 0, func(int) string { return "short" })
	chunks := ChunkSegments(segs, Options{MaxDuration: 5 * time.Minute})
	checkPartition(t, segs, chunks)
	if want := []int{30, 30, 30, 10}; !reflect.DeepEqual(sizes(chunks), want) {
		t.Fatalf("sizes = %v, want %v", sizes(chunks), want)
	}
	// 10 s segments: the 30 s context is the last three segments.
	if len(chunks[1].Context) != 3 || chunks[1].Context[0].Idx != 28 {
		t.Fatalf("context = %+v", chunks[1].Context)
	}
	if chunks[1].Key() != "S31-S60+ctxS28" {
		t.Fatalf("key = %q", chunks[1].Key())
	}
}

func TestChunkTokenLimitAndDefaults(t *testing.T) {
	hundred := strings.Repeat("字", 100)
	segs := mkSegs(95, 1000, 0, func(int) string { return hundred })
	chunks := ChunkSegments(segs, Options{MaxTokens: 1000})
	checkPartition(t, segs, chunks)
	for i, c := range chunks {
		if c.Tokens > 1000 {
			t.Errorf("chunk %d has %d tokens", i, c.Tokens)
		}
	}
	if len(chunks) != 10 {
		t.Fatalf("chunks = %d, want 10", len(chunks))
	}
	// Defaults: 3000 tokens / 10 minutes.
	chunks = ChunkSegments(segs, Options{})
	if want := []int{30, 30, 30, 5}; !reflect.DeepEqual(sizes(chunks), want) {
		t.Fatalf("default sizes = %v, want %v", sizes(chunks), want)
	}
}

func TestChunkPrefersPauses(t *testing.T) {
	// 10 s segments; limit 5 min = 30 segments. Pauses of 2 s after segment 5
	// (front half: ignored) and after segment 25 (back half: preferred).
	segs := mkSegs(60, 10000, 0, func(int) string { return "short" })
	shift := func(from int, by int64) {
		for i := from; i < len(segs); i++ {
			segs[i].StartMs += by
			segs[i].EndMs += by
		}
	}
	shift(5, 2000)
	shift(25, 2000)
	chunks := ChunkSegments(segs, Options{MaxDuration: 5 * time.Minute})
	checkPartition(t, segs, chunks)
	if len(chunks[0].Segments) != 25 {
		t.Fatalf("first chunk has %d segments, want a cut at the pause after 25", len(chunks[0].Segments))
	}
	for i, c := range chunks {
		if d := c.EndMs() - c.StartMs(); d > 5*60*1000 {
			t.Errorf("chunk %d lasts %d ms", i, d)
		}
	}
	// A pause shorter than 1.5 s is not a preferred cut point.
	segs = mkSegs(60, 10000, 0, func(int) string { return "short" })
	for i := 25; i < len(segs); i++ {
		segs[i].StartMs += 1400
		segs[i].EndMs += 1400
	}
	if n := len(ChunkSegments(segs, Options{MaxDuration: 5 * time.Minute})[0].Segments); n != 29 {
		t.Fatalf("first chunk has %d segments, want 29 (limit cut)", n)
	}
}

func TestChunkNeverSplitsASegment(t *testing.T) {
	segs := mkSegs(5, 10000, 0, func(i int) string {
		if i == 3 {
			return strings.Repeat("长", 5000) // far above the token limit
		}
		return "short"
	})
	segs[1].EndMs = segs[1].StartMs + 20*60*1000 // far above the duration limit
	for i := 2; i < len(segs); i++ {
		segs[i].StartMs += 20 * 60 * 1000
		segs[i].EndMs += 20 * 60 * 1000
	}
	chunks := ChunkSegments(segs, Options{})
	checkPartition(t, segs, chunks)
	if want := []int{1, 1, 1, 2}; !reflect.DeepEqual(sizes(chunks), want) {
		t.Fatalf("sizes = %v, want %v", sizes(chunks), want)
	}
	// An oversized previous segment is still offered as context (at least one).
	if len(chunks[3].Context) != 1 || chunks[3].Context[0].Idx != 3 {
		t.Fatalf("context = %+v", chunks[3].Context)
	}
}

func TestChunkDeterministicAndOrderInsensitive(t *testing.T) {
	segs := mkSegs(400, 7000, 300, zh)
	a := ChunkSegments(segs, Options{})
	b := ChunkSegments(segs, Options{})
	if !reflect.DeepEqual(a, b) {
		t.Fatal("chunking is not deterministic")
	}
	rev := make([]app.Segment, len(segs))
	for i, s := range segs {
		rev[len(segs)-1-i] = s
	}
	c := ChunkSegments(rev, Options{})
	if !reflect.DeepEqual(a, c) {
		t.Fatal("chunking depends on input order")
	}
	if segs[0].Idx != 1 || rev[0].Idx != 400 {
		t.Fatal("input slices were modified")
	}
	keys := map[string]bool{}
	for _, ch := range a {
		if keys[ch.Key()] {
			t.Fatalf("duplicate key %s", ch.Key())
		}
		keys[ch.Key()] = true
	}
	checkPartition(t, segs, a)
}

func TestFormatTime(t *testing.T) {
	cases := []struct {
		ms   int64
		long bool
		want string
	}{
		{0, false, "00:00"}, {59999, false, "00:59"}, {61000, false, "01:01"}, {3599000, false, "59:59"},
		{0, true, "0:00:00"}, {83000, true, "0:01:23"}, {3753000, true, "1:02:33"}, {-5, false, "00:00"},
		{36000000, true, "10:00:00"},
	}
	for _, tc := range cases {
		if got := FormatTime(tc.ms, tc.long); got != tc.want {
			t.Errorf("FormatTime(%d, %v) = %q, want %q", tc.ms, tc.long, got, tc.want)
		}
	}
	if IsLong(3599999) || !IsLong(3600000) {
		t.Error("IsLong boundary wrong")
	}
}

// TestOneLineFastPath pins the shortcut to the full rewrite: a string is
// returned untouched only when the rewrite would not change it.
func TestOneLineFastPath(t *testing.T) {
	for _, s := range []string{
		"", "a", "两个 词", "a b c", " leading", "trailing ", "double  space", "tab\there", "line\nbreak",
		"nbsp\u00a0here", "ctrl\x07bell", "bad\xffutf8", "ideographic\u3000space", "emoji 🙂 ok",
	} {
		if got := isOneLine(s); got != (rewriteOneLine(s) == s) {
			t.Errorf("isOneLine(%q) = %v, but the rewrite gives %q", s, got, rewriteOneLine(s))
		}
	}
}
