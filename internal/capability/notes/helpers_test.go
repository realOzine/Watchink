package notes

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"watchink/internal/app"
	"watchink/internal/capability/llm"
	"watchink/internal/capability/llm/llmtest"
)

const testTranscriptID = "tr_01JA2M8Q0000000000000000AB"

var (
	zhSentence = "梯度下降法沿着损失函数的负梯度方向逐步更新模型参数直到收敛" // 29 CJK chars
	enSentence = "gradient descent updates the parameters along the negative gradient until it converges"
)

// mkSegs builds n contiguous segments of segMs each, separated by gapMs.
func mkSegs(n int, segMs, gapMs int64, text func(i int) string) []app.Segment {
	segs := make([]app.Segment, n)
	var t int64
	for i := range segs {
		segs[i] = app.Segment{SegmentID: app.SegmentID(testTranscriptID, i+1), Idx: i + 1, StartMs: t, EndMs: t + segMs, Text: text(i + 1)}
		t += segMs + gapMs
	}
	return segs
}

func zh(i int) string { return fmt.Sprintf("第%d句%s", i, zhSentence) }

// newFake starts a fake endpoint and a client with instant retries.
func newFake(t *testing.T) (*llmtest.Server, llm.Client) {
	t.Helper()
	llm.ResetStructuredOutputCache()
	fake := llmtest.New()
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	c, err := llm.New(llm.Config{BaseURL: ts.URL + "/v1", Model: "fake-model", APIKey: "sk-test-000000",
		Temperature: 0.2, RetryBaseDelay: time.Millisecond,
		// These tests exercise the notes package's own fallbacks (repair,
		// split); the client's parameter self-healing has its own tests.
		DisableTuning: true})
	if err != nil {
		t.Fatal(err)
	}
	return fake, c
}

func oneChunk(segs []app.Segment) Chunk {
	c := Chunk{Segments: segs}
	for _, s := range segs {
		c.Tokens += EstimateTokens(s.Text)
	}
	return c
}

// runPipeline runs chunk → map → reduce → render against the client.
func runPipeline(t *testing.T, c llm.Client, segs []app.Segment, title string, durMs int64, opt Options) ([]Chunk, []*ChunkResult, *ReduceResult, *Rendered) {
	t.Helper()
	ctx := context.Background()
	chunks := ChunkSegments(segs, opt)
	results, err := MapAll(ctx, c, chunks, MapAllOptions{VideoTitle: title, DurationMs: durMs})
	if err != nil {
		t.Fatalf("MapAll: %v", err)
	}
	red, err := Reduce(ctx, c, ReduceInput{VideoTitle: title, DurationMs: durMs, Chunks: results})
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	out, err := Render(RenderInput{VideoID: "vid_01JA2M8Q0000000000000000CD", NoteID: "note_01JA2M8Q0000000000000000EF",
		TranscriptID: testTranscriptID, Source: app.VideoSource{Type: app.SourceFile, URI: "/videos/lecture.mp4"},
		VideoTitle: title, DurationMs: durMs, Language: "zh", GeneratedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
		Model: "fake-model", Segments: segs, Chunks: results, Reduce: red})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return chunks, results, red, out
}

// checkPlacement asserts that every point of the chunks is in exactly one section.
func checkPlacement(t *testing.T, results []*ChunkResult, red *ReduceResult) {
	t.Helper()
	count := map[string]int{}
	for _, s := range red.Sections {
		if len(s.PointIDs) == 0 {
			t.Errorf("section %q has no points", s.Heading)
		}
		for _, id := range s.PointIDs {
			count[id]++
		}
	}
	total := 0
	for _, ch := range results {
		for _, p := range ch.Points {
			total++
			if count[p.ID] != 1 {
				t.Errorf("point %s appears %d times in sections", p.ID, count[p.ID])
			}
		}
	}
	if len(count) != total {
		t.Errorf("sections hold %d distinct points, chunks have %d", len(count), total)
	}
}

func mustContain(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("missing %q in:\n%s", sub, truncateRunes(s, 1500))
		}
	}
}
