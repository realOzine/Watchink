package notes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"watchink/internal/app"
	"watchink/internal/capability/llm"
)

// Limits of the map step.
const (
	mapMaxTokens     = 2500
	maxCitesPerPoint = 5
	maxCitesPerTerm  = 3
	maxDropRatio     = 0.3   // more dropped points than this triggers a repair
	minSplitMs       = 60000 // chunks are never split below one minute
	smallChunkCJK    = 200   // below both thresholds a chunk may have no points
	smallChunkWords  = 50
)

// Point kinds.
var pointKinds = []string{"concept", "definition", "method", "example", "conclusion", "caution", "other"}

// MapInput is the input of one map call.
type MapInput struct {
	VideoTitle string
	// ChunkIndex is the 1-based chunk number; it becomes the <n> of P<n>.<m>.
	ChunkIndex int
	// ChunkCount is the total number of chunks (0 if unknown; prompt only).
	ChunkCount int
	// DurationMs is the video duration; it only selects the time format.
	DurationMs int64
	Chunk      Chunk
}

// Point is one validated point of a chunk. Cites holds transcript segment
// idx values (the <idx> of S<idx>), ascending, all inside the chunk.
type Point struct {
	ID      string `json:"id"` // P<chunk>.<n>, assigned by the program
	Title   string `json:"title"`
	Detail  string `json:"detail"`
	Kind    string `json:"kind"`
	Cites   []int  `json:"cites"`
	StartMs int64  `json:"start_ms"` // start of the earliest cited segment
	EndMs   int64  `json:"end_ms"`   // end of the latest cited segment
}

// Term is one validated glossary entry of a chunk.
type Term struct {
	ID          string `json:"id"` // T<chunk>.<n>
	Term        string `json:"term"`
	Explanation string `json:"explanation"`
	Cites       []int  `json:"cites"`
	StartMs     int64  `json:"start_ms"`
}

// ChunkStats is the per-chunk contribution to coverage.
type ChunkStats struct {
	// Empty is true when the chunk legitimately produced no points.
	Empty            bool `json:"empty"`
	PointsDropped    int  `json:"points_dropped"`
	CitationsDropped int  `json:"citations_dropped"`
	RepairCalls      int  `json:"repair_calls"`
	SplitRetries     int  `json:"split_retries"`
	LLMCalls         int  `json:"llm_calls"`
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
}

// ChunkResult is the validated output of the map step for one chunk. It is
// plain JSON and is cached on disk as the map_chunk artifact.
type ChunkResult struct {
	PromptVersion string     `json:"prompt_version"`
	ChunkIndex    int        `json:"chunk_index"`
	Key           string     `json:"key"`
	FirstIdx      int        `json:"first_idx"`
	LastIdx       int        `json:"last_idx"`
	StartMs       int64      `json:"start_ms"`
	EndMs         int64      `json:"end_ms"`
	Summary       string     `json:"summary"`
	Points        []Point    `json:"points"`
	Terms         []Term     `json:"terms"`
	Stats         ChunkStats `json:"stats"`
}

// citeList decodes "cites" leniently: models sometimes answer [123] or a
// single string instead of ["S123"]. Validation still decides what is kept.
type citeList []string

func (c *citeList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*c = nil
		return nil
	}
	var items []json.RawMessage
	if len(b) > 0 && b[0] != '[' {
		items = []json.RawMessage{b}
	} else if err := json.Unmarshal(b, &items); err != nil {
		return err
	}
	out := make(citeList, 0, len(items))
	for _, it := range items {
		var s string
		if json.Unmarshal(it, &s) != nil {
			s = string(bytes.TrimSpace(it)) // number, or garbage that will not parse as a cite
		}
		out = append(out, s)
	}
	*c = out
	return nil
}

// mapOutput is the model-facing schema of the map step. The JSON
// Schema sent to the model is generated from these structs.
type mapOutput struct {
	ChunkSummary string     `json:"chunk_summary" desc:"本块主要内容，2–4 句"`
	Points       []mapPoint `json:"points"`
	Terms        []mapTerm  `json:"terms"`
}

type mapPoint struct {
	Title  string   `json:"title" desc:"不超过 30 字"`
	Detail string   `json:"detail" desc:"1–4 句说明"`
	Kind   string   `json:"kind" enum:"concept,definition,method,example,conclusion,caution,other"`
	Cites  citeList `json:"cites" desc:"1–5 个片段编号，形如 S123，只能来自可引用字幕"`
}

type mapTerm struct {
	Term        string   `json:"term" desc:"术语原文"`
	Explanation string   `json:"explanation" desc:"中文解释"`
	Cites       citeList `json:"cites" desc:"1–3 个片段编号，形如 S123"`
}

var mapSchema = llm.SchemaFor("lecture_chunk_points", mapOutput{})

type mapPromptData struct {
	Title       string
	Index       int
	Count       int
	Range       string
	Context     []string
	Citable     []string
	ExampleCite string
}

func segLine(s app.Segment, long bool) string {
	return fmt.Sprintf("S%d [%s–%s] %s", s.Idx, FormatTime(s.StartMs, long), FormatTime(s.EndMs, long), promptText(s.Text))
}

// buildMapPrompt renders the user message of a map call.
func buildMapPrompt(in MapInput, ch Chunk) string {
	long := IsLong(max(in.DurationMs, ch.EndMs()))
	d := mapPromptData{
		Title: promptText(in.VideoTitle),
		Index: in.ChunkIndex,
		Count: in.ChunkCount,
		Range: FormatTime(ch.StartMs(), long) + "–" + FormatTime(ch.EndMs(), long),
	}
	for _, s := range ch.Context {
		d.Context = append(d.Context, segLine(s, long))
	}
	for _, s := range ch.Segments {
		d.Citable = append(d.Citable, segLine(s, long))
	}
	if len(ch.Segments) > 0 {
		d.ExampleCite = "S" + strconv.Itoa(ch.Segments[0].Idx)
	}
	return renderPrompt("map_user.tmpl", d)
}

// parseCite accepts S123, s123, 123 and [S123].
func parseCite(s string) (int, bool) {
	s = strings.Trim(strings.TrimSpace(s), "[]【】 ")
	if len(s) > 0 && (s[0] == 'S' || s[0] == 's') {
		s = s[1:]
	}
	if s == "" || len(s) > 9 {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0
}

type problemKind int

const (
	problemJSON  problemKind = iota + 1 // not a decodable JSON object
	problemCites                        // too many points lost to invalid citations
	problemEmpty                        // no points for a chunk with real content
)

type mapProblem struct {
	kind   problemKind
	detail string
}

// validated is a chunk answer after the validation, before ids exist.
type validated struct {
	summary          string
	points           []Point
	terms            []Term
	pointsDropped    int
	citationsDropped int
}

// validCites keeps the unique cites that fall inside the citable range, in
// time order, at most limit of them; it returns how many were dropped.
func validCites(raw citeList, byIdx map[int]app.Segment, limit int) (kept []int, dropped int) {
	seen := map[int]bool{}
	for _, r := range raw {
		idx, ok := parseCite(r)
		if !ok {
			dropped++
			continue
		}
		if _, inRange := byIdx[idx]; !inRange {
			dropped++
			continue
		}
		if seen[idx] {
			continue // a repeated cite is not an invalid one
		}
		if len(kept) >= limit {
			dropped++
			continue
		}
		seen[idx] = true
		kept = append(kept, idx)
	}
	sort.Ints(kept)
	return kept, dropped
}

// validateMap applies the rules to one model answer.
func validateMap(content string, ch Chunk) (*validated, *mapProblem) {
	var out mapOutput
	if err := decodeJSON(content, &out); err != nil {
		return nil, &mapProblem{problemJSON, truncateRunes(err.Error(), 160)}
	}
	byIdx := make(map[int]app.Segment, len(ch.Segments))
	for _, s := range ch.Segments {
		byIdx[s.Idx] = s
	}
	v := &validated{summary: truncateRunes(oneLine(out.ChunkSummary), 600)}
	for _, p := range out.Points {
		cites, dropped := validCites(p.Cites, byIdx, maxCitesPerPoint)
		v.citationsDropped += dropped
		title, detail := truncateRunes(oneLine(p.Title), 80), truncateRunes(oneLine(p.Detail), 800)
		if len(cites) == 0 || (title == "" && detail == "") {
			v.pointsDropped++
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(p.Kind))
		if !contains(pointKinds, kind) {
			kind = "other"
		}
		pt := Point{Title: title, Detail: detail, Kind: kind, Cites: cites,
			StartMs: byIdx[cites[0]].StartMs, EndMs: byIdx[cites[0]].EndMs}
		for _, c := range cites[1:] {
			pt.StartMs = min(pt.StartMs, byIdx[c].StartMs)
			pt.EndMs = max(pt.EndMs, byIdx[c].EndMs)
		}
		v.points = append(v.points, pt)
	}
	for _, t := range out.Terms {
		cites, dropped := validCites(t.Cites, byIdx, maxCitesPerTerm)
		v.citationsDropped += dropped
		term := truncateRunes(oneLine(t.Term), 60)
		if len(cites) == 0 || term == "" {
			continue
		}
		v.terms = append(v.terms, Term{Term: term, Explanation: truncateRunes(oneLine(t.Explanation), 300),
			Cites: cites, StartMs: byIdx[cites[0]].StartMs})
	}
	if total := len(out.Points); total > 0 && float64(v.pointsDropped)/float64(total) > maxDropRatio {
		return v, &mapProblem{problemCites, fmt.Sprintf("%d 个要点中有 %d 个因为没有有效引用被丢弃", total, v.pointsDropped)}
	}
	if len(v.points) == 0 && !isSmallChunk(ch) {
		return v, &mapProblem{problemEmpty, "本块有实质内容，但没有给出任何要点"}
	}
	return v, nil
}

// isSmallChunk reports whether the chunk has so little speech that an answer
// without points is acceptable.
func isSmallChunk(ch Chunk) bool {
	cjk, words := 0, 0
	for _, s := range ch.Segments {
		c, w := countText(s.Text)
		cjk += c
		words += w
	}
	return cjk < smallChunkCJK && words < smallChunkWords
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func repairText(p *mapProblem, ch Chunk) string {
	data := map[string]string{}
	first, last := ch.Segments[0].Idx, ch.Segments[len(ch.Segments)-1].Idx
	switch p.kind {
	case problemJSON:
		data["Problem"] = "输出不是合法的 JSON（" + p.detail + "）。"
		data["Advice"] = ""
	case problemCites:
		data["Problem"] = p.detail + "。"
		data["Advice"] = fmt.Sprintf("每个要点的 cites 必须是 1–5 个 CITABLE 块中真实存在的编号（本块的编号范围是 S%d 到 S%d），不能引用 CONTEXT 块。", first, last)
	default:
		data["Problem"] = p.detail + "。"
		data["Advice"] = fmt.Sprintf("请至少整理出一个要点，并为它标注 S%d 到 S%d 之间的出处编号。", first, last)
	}
	return "\n\n" + renderPrompt("repair.tmpl", data)
}

func (p *mapProblem) err(in MapInput, ch Chunk) error {
	var e *app.Error
	if p.kind == problemCites {
		e = app.E(app.CodeCitationInvalid, "第 %d 块的要点经过一次修复后，无效引用的比例仍然过高", in.ChunkIndex)
	} else {
		e = app.E(app.CodeModelOutputInvalid, "第 %d 块的模型输出经过一次修复后仍不合法", in.ChunkIndex)
	}
	return e.WithHint("重试该任务；如果反复出现，换用更可靠的模型").
		WithDetail("chunk", in.ChunkIndex).WithDetail("chunk_key", ch.Key()).WithDetail("problem", p.detail)
}

// mapOnce runs one chunk through the model, with at most one repair call.
func mapOnce(ctx context.Context, c llm.Client, st *callStats, in MapInput, ch Chunk) (*validated, error) {
	system := renderPrompt("map_system.tmpl", nil)
	user := buildMapPrompt(in, ch)
	content, err := ask(ctx, c, st, system, user, mapSchema, mapMaxTokens)
	if err != nil {
		return nil, err
	}
	v, prob := validateMap(content, ch)
	if prob == nil {
		return v, nil
	}
	st.repairs++
	content, err = ask(ctx, c, st, system, user+repairText(prob, ch), mapSchema, mapMaxTokens)
	if err != nil {
		return nil, err
	}
	if v, prob = validateMap(content, ch); prob != nil {
		return nil, prob.err(in, ch)
	}
	return v, nil
}

// splitChunk halves a chunk at the segment boundary closest to its temporal
// midpoint. It refuses when a half would be shorter than one minute.
func splitChunk(ch Chunk) (a, b Chunk, ok bool) {
	n := len(ch.Segments)
	if n < 2 || ch.EndMs()-ch.StartMs() < 2*minSplitMs {
		return a, b, false
	}
	mid := ch.StartMs() + (ch.EndMs()-ch.StartMs())/2
	k := 1
	for i := 2; i < n; i++ {
		if abs64(ch.Segments[i].StartMs-mid) < abs64(ch.Segments[k].StartMs-mid) {
			k = i
		}
	}
	a = Chunk{Context: ch.Context, Segments: ch.Segments[:k:k]}
	b = Chunk{Context: trailingContext(a.Segments, k), Segments: ch.Segments[k:]}
	for _, s := range a.Segments {
		a.Tokens += EstimateTokens(s.Text)
	}
	b.Tokens = ch.Tokens - a.Tokens
	return a, b, true
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// mapPiece maps a chunk, splitting it in half recursively when the input does
// not fit or the output is truncated. splits counts the splits performed.
func mapPiece(ctx context.Context, c llm.Client, st *callStats, in MapInput, ch Chunk, splits *int) (*validated, error) {
	v, err := mapOnce(ctx, c, st, in, ch)
	if !errors.Is(err, errNeedSplit) {
		return v, err
	}
	a, b, ok := splitChunk(ch)
	if !ok {
		return nil, app.E(app.CodeModelOutputInvalid, "第 %d 块拆分到最小长度后，模型仍无法完整处理", in.ChunkIndex).
			WithHint("已自动提高并取消输出上限、并把该块拆到最小仍然不行：请换用输出上限更大的模型后重试").
			WithDetail("chunk", in.ChunkIndex).WithDetail("chunk_key", ch.Key())
	}
	*splits++
	va, err := mapPiece(ctx, c, st, in, a, splits)
	if err != nil {
		return nil, err
	}
	vb, err := mapPiece(ctx, c, st, in, b, splits)
	if err != nil {
		return nil, err
	}
	return &validated{
		summary:          strings.TrimSpace(va.summary + " " + vb.summary),
		points:           append(va.points, vb.points...),
		terms:            append(va.terms, vb.terms...),
		pointsDropped:    va.pointsDropped + vb.pointsDropped,
		citationsDropped: va.citationsDropped + vb.citationsDropped,
	}, nil
}

// MapChunk runs the map step for one chunk: one model call, strict
// validation of every citation, at most one repair call, and recursive
// halving when the model cannot take the chunk in one piece.
//
// Errors are *app.Error (MODEL_OUTPUT_INVALID, CITATION_INVALID or a model
// transport code), or ctx.Err() on cancellation.
func MapChunk(ctx context.Context, c llm.Client, in MapInput) (*ChunkResult, error) {
	ch := in.Chunk
	res := &ChunkResult{PromptVersion: PromptVersion, ChunkIndex: in.ChunkIndex, Key: ch.Key(),
		StartMs: ch.StartMs(), EndMs: ch.EndMs(), Points: []Point{}, Terms: []Term{}}
	if len(ch.Segments) == 0 {
		res.Stats.Empty = true
		return res, nil
	}
	res.FirstIdx, res.LastIdx = ch.Segments[0].Idx, ch.Segments[len(ch.Segments)-1].Idx

	var st callStats
	splits := 0
	v, err := mapPiece(ctx, c, &st, in, ch, &splits)
	if err != nil {
		return nil, err
	}
	res.Summary = v.summary
	for i, p := range v.points {
		p.ID = fmt.Sprintf("P%d.%d", in.ChunkIndex, i+1)
		res.Points = append(res.Points, p)
	}
	seenTerm := map[string]bool{}
	for _, t := range v.terms {
		if key := strings.ToLower(t.Term); !seenTerm[key] {
			seenTerm[key] = true
			t.ID = fmt.Sprintf("T%d.%d", in.ChunkIndex, len(res.Terms)+1)
			res.Terms = append(res.Terms, t)
		}
	}
	res.Stats = ChunkStats{
		Empty:            len(res.Points) == 0,
		PointsDropped:    v.pointsDropped,
		CitationsDropped: v.citationsDropped,
		RepairCalls:      st.repairs,
		SplitRetries:     splits,
		LLMCalls:         st.calls,
		PromptTokens:     st.promptTokens,
		CompletionTokens: st.completionTokens,
	}
	return res, nil
}

// MapAllOptions tunes MapAll.
type MapAllOptions struct {
	VideoTitle string
	DurationMs int64
	// Concurrency is the number of parallel map calls; 0 means 3.
	Concurrency int
	// Cached may return an already computed result for chunk i (0-based);
	// nil means "call the model". Used for per-chunk artifact reuse.
	Cached func(i int, c Chunk) *ChunkResult
	// OnResult is called (serially) for every freshly computed chunk result.
	OnResult func(i int, r *ChunkResult) error
}

// MapAll maps every chunk with bounded parallelism and returns the results in
// chunk order. The first error cancels the remaining calls.
func MapAll(ctx context.Context, c llm.Client, chunks []Chunk, opt MapAllOptions) ([]*ChunkResult, error) {
	workers := opt.Concurrency
	if workers <= 0 {
		workers = 3
	}
	out := make([]*ChunkResult, len(chunks))
	var todo []int
	for i, ch := range chunks {
		if opt.Cached != nil {
			out[i] = opt.Cached(i, ch)
		}
		if out[i] == nil {
			todo = append(todo, i)
		}
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	jobs := make(chan int)
	for w := 0; w < min(workers, len(todo)); w++ {
		wg.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					continue // drain: an earlier chunk failed or the caller cancelled
				}
				r, err := MapChunk(ctx, c, MapInput{VideoTitle: opt.VideoTitle, ChunkIndex: i + 1,
					ChunkCount: len(chunks), DurationMs: opt.DurationMs, Chunk: chunks[i]})
				mu.Lock()
				if err == nil && firstErr == nil && opt.OnResult != nil {
					err = opt.OnResult(i, r)
				}
				if err == nil {
					out[i] = r
				} else if firstErr == nil {
					// Keep the real failure; the calls it cancels only
					// report context.Canceled.
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		})
	}
	for _, i := range todo {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if firstErr == nil {
		firstErr = parent.Err()
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
