package notes

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"watchink/internal/app"
	"watchink/internal/llm"
)

// Limits of the reduce step (§9.3, §9.8).
const (
	reduceMaxTokens     = 4000
	twoLevelPoints      = 120   // more points than this → two-level reduce
	twoLevelTokens      = 12000 // or an estimated input above this
	groupMaxPoints      = 60    // points per group in the first level
	minGroupPoints      = 5     // groups are not halved below this
	maxOverviewPoints   = 8
	maxTakeawayPoints   = 3
	otherSectionHeading = "其他要点"
	fallbackNoteTitle   = "讲座笔记"
)

// ReduceInput is the input of the reduce step: titles, durations and the map
// results. Reduce never sees transcript segments (D-09).
type ReduceInput struct {
	VideoTitle string
	DurationMs int64
	Chunks     []*ChunkResult
}

// Overview is the note overview. Fallback is true when the model gave no
// usable overview and the text was assembled from the chunk summaries.
type Overview struct {
	Text     string   `json:"text"`
	PointIDs []string `json:"point_ids"`
	Fallback bool     `json:"fallback,omitempty"`
}

// Section is one chapter of the note. StartMs / EndMs are computed from the
// citations of its points, never taken from the model.
type Section struct {
	Heading  string   `json:"heading"`
	Summary  string   `json:"summary"`
	PointIDs []string `json:"point_ids"`
	StartMs  int64    `json:"start_ms"`
	EndMs    int64    `json:"end_ms"`
}

// Takeaway is one key conclusion backed by at least one point.
type Takeaway struct {
	Text     string   `json:"text"`
	PointIDs []string `json:"point_ids"`
}

// ReduceStats records what validation and completion had to do (§9.3, §9.6).
type ReduceStats struct {
	Points int `json:"points"`
	// PointsUnplaced counts valid points the model left out of every section
	// and the program put back.
	PointsUnplaced    int  `json:"points_unplaced"`
	UnknownPointIDs   int  `json:"unknown_point_ids"`
	DuplicatePointIDs int  `json:"duplicate_point_ids"`
	TakeawaysDropped  int  `json:"takeaways_dropped"`
	OverviewFallback  bool `json:"overview_fallback"`
	TwoLevel          bool `json:"two_level"`
	Groups            int  `json:"groups"`
	SectionsMerged    int  `json:"sections_merged"`
	RepairCalls       int  `json:"repair_calls"`
	LLMCalls          int  `json:"llm_calls"`
	PromptTokens      int  `json:"prompt_tokens"`
	CompletionTokens  int  `json:"completion_tokens"`
}

// ReduceResult is the validated and completed outline of the note. Every
// point of the input appears in exactly one section. It is plain JSON and is
// cached as the reduce artifact.
type ReduceResult struct {
	PromptVersion string      `json:"prompt_version"`
	Title         string      `json:"title"`
	Overview      Overview    `json:"overview"`
	Sections      []Section   `json:"sections"`
	Takeaways     []Takeaway  `json:"key_takeaways"`
	Stats         ReduceStats `json:"stats"`
}

// Model-facing schemas (§9.3).
type outOverview struct {
	Text     string   `json:"text" desc:"200–400 字概览"`
	PointIDs []string `json:"point_ids" desc:"支撑概览的要点编号，形如 P1.2"`
}

type outSection struct {
	Heading  string   `json:"heading" desc:"章节标题"`
	Summary  string   `json:"summary" desc:"1–3 句"`
	PointIDs []string `json:"point_ids" desc:"属于该章节的要点编号，形如 P1.2"`
}

type outTakeaway struct {
	Text     string   `json:"text" desc:"一句话结论"`
	PointIDs []string `json:"point_ids" desc:"支撑该结论的要点编号"`
}

type reduceOutput struct {
	Title        string        `json:"title" desc:"笔记标题"`
	Overview     outOverview   `json:"overview"`
	Sections     []outSection  `json:"sections"`
	KeyTakeaways []outTakeaway `json:"key_takeaways"`
}

type groupOutput struct {
	Sections []outSection `json:"sections"`
}

type outMerge struct {
	Heading    string   `json:"heading" desc:"合并后的章节标题"`
	Summary    string   `json:"summary" desc:"合并后的摘要，1–3 句"`
	SectionIDs []string `json:"section_ids" desc:"要合并的相邻章节编号，形如 G1.2"`
}

type finalOutput struct {
	Title        string        `json:"title" desc:"笔记标题"`
	Overview     outOverview   `json:"overview"`
	KeyTakeaways []outTakeaway `json:"key_takeaways"`
	Merges       []outMerge    `json:"merges"`
}

var (
	reduceSchema = llm.SchemaFor("lecture_note_outline", reduceOutput{})
	groupSchema  = llm.SchemaFor("lecture_note_sections", groupOutput{})
	finalSchema  = llm.SchemaFor("lecture_note_frame", finalOutput{})
)

// frame is the part of the outline that is not sections.
type frame struct {
	title     string
	overview  outOverview
	takeaways []outTakeaway
}

// group is a time-ordered run of points reduced together in the first level.
type group struct {
	points    []Point
	summaries []string
}

type reducer struct {
	ctx    context.Context
	c      llm.Client
	in     ReduceInput
	long   bool
	system string
	byID   map[string]Point
	order  map[string]int // point id → position in time/chunk order
	st     callStats
	stats  ReduceStats
}

// Reduce organises the points of all chunks into the note outline (§9.3) and
// then validates and completes the model's answer so that every valid point
// ends up in exactly one section. With many points it switches to the
// two-level reduce.
func Reduce(ctx context.Context, c llm.Client, in ReduceInput) (*ReduceResult, error) {
	r := &reducer{ctx: ctx, c: c, in: in, long: IsLong(in.DurationMs),
		system: renderPrompt("reduce_system.tmpl", nil), byID: map[string]Point{}, order: map[string]int{}}
	var all []Point
	for _, ch := range in.Chunks {
		if ch == nil {
			continue
		}
		for _, p := range ch.Points {
			if _, dup := r.byID[p.ID]; dup || p.ID == "" {
				return nil, app.E(app.CodeInternal, "要点编号重复或为空：%q", p.ID)
			}
			r.byID[p.ID] = p
			r.order[p.ID] = len(all)
			all = append(all, p)
		}
	}
	r.stats.Points = len(all)

	var sections []Section
	var fr frame
	if len(all) > 0 {
		whole := group{points: all, summaries: r.summaryLines(in.Chunks)}
		user := r.reducePrompt("reduce", whole, "")
		twoLevel := len(all) > twoLevelPoints || EstimateTokens(user) > twoLevelTokens
		if !twoLevel {
			out, err := askJSON[reduceOutput](ctx, c, &r.st, r.system, user, reduceSchema, reduceMaxTokens)
			switch {
			case errors.Is(err, errNeedSplit) && len(all) >= 2*minGroupPoints:
				// The single call did not fit this model after all.
				twoLevel = true
			case errors.Is(err, errNeedSplit):
				return nil, tooLargeError()
			case err != nil:
				return nil, err
			default:
				sections = r.complete(out.Sections, all)
				fr = frame{out.Title, out.Overview, out.KeyTakeaways}
			}
		}
		if twoLevel {
			var err error
			if sections, fr, err = r.twoLevel(); err != nil {
				return nil, err
			}
		}
	}

	res := &ReduceResult{PromptVersion: PromptVersion, Sections: sections, Takeaways: []Takeaway{}}
	if res.Sections == nil {
		res.Sections = []Section{}
	}
	for i := range res.Sections {
		r.setRange(&res.Sections[i])
	}
	res.Title = truncateRunes(oneLine(fr.title), 120)
	if res.Title == "" {
		res.Title = truncateRunes(oneLine(in.VideoTitle), 120)
	}
	if res.Title == "" {
		res.Title = fallbackNoteTitle
	}
	// The overview and every takeaway must be backed by at least one known
	// point, otherwise they are dropped (§9.3).
	res.Overview = Overview{Text: truncateRunes(oneLine(fr.overview.Text), 1500),
		PointIDs: r.knownIDs(fr.overview.PointIDs, maxOverviewPoints)}
	if res.Overview.Text == "" || len(res.Overview.PointIDs) == 0 {
		var parts []string
		for _, ch := range in.Chunks {
			if ch != nil && ch.Summary != "" {
				parts = append(parts, ch.Summary)
			}
		}
		res.Overview = Overview{Text: strings.Join(parts, " "), PointIDs: []string{}, Fallback: true}
		r.stats.OverviewFallback = true
	}
	for _, t := range fr.takeaways {
		text, ids := truncateRunes(oneLine(t.Text), 400), r.knownIDs(t.PointIDs, maxTakeawayPoints)
		if text == "" || len(ids) == 0 {
			r.stats.TakeawaysDropped++
			continue
		}
		res.Takeaways = append(res.Takeaways, Takeaway{Text: text, PointIDs: ids})
	}
	r.stats.RepairCalls = r.st.repairs
	r.stats.LLMCalls = r.st.calls
	r.stats.PromptTokens = r.st.promptTokens
	r.stats.CompletionTokens = r.st.completionTokens
	res.Stats = r.stats
	return res, nil
}

func tooLargeError() error {
	return app.E(app.CodeModelOutputInvalid, "汇总要点时，模型无法在上下文或输出上限内完成").
		WithHint("换用上下文和输出上限更大的模型后重试该任务")
}

func normalizePointID(s string) string {
	s = strings.Trim(strings.TrimSpace(s), "[]【】 ")
	if strings.HasPrefix(s, "p") {
		s = "P" + s[1:]
	}
	return s
}

// knownIDs keeps the distinct known point ids, in time order, at most limit.
func (r *reducer) knownIDs(raw []string, limit int) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, id := range raw {
		id = normalizePointID(id)
		if _, ok := r.byID[id]; ok && !seen[id] && len(out) < limit {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return r.order[out[i]] < r.order[out[j]] })
	return out
}

func (r *reducer) setRange(s *Section) {
	for i, id := range s.PointIDs {
		p := r.byID[id]
		if i == 0 || p.StartMs < s.StartMs {
			s.StartMs = p.StartMs
		}
		if i == 0 || p.EndMs > s.EndMs {
			s.EndMs = p.EndMs
		}
	}
}

// complete validates the sections proposed for the given points and puts back
// every point the model left out (§9.3):
//   - unknown ids are removed, and a point stays only where it first appears;
//   - an unplaced point is appended to the section closest in time, or to a
//     new "其他要点" section when there is none.
func (r *reducer) complete(proposed []outSection, points []Point) []Section {
	universe := make(map[string]bool, len(points))
	for _, p := range points {
		universe[p.ID] = true
	}
	placed := map[string]bool{}
	var sections []Section
	for _, ps := range proposed {
		s := Section{Heading: truncateRunes(oneLine(ps.Heading), 80), Summary: truncateRunes(oneLine(ps.Summary), 500)}
		for _, raw := range ps.PointIDs {
			id := normalizePointID(raw)
			switch {
			case !universe[id]:
				r.stats.UnknownPointIDs++
			case placed[id]:
				r.stats.DuplicatePointIDs++
			default:
				placed[id] = true
				s.PointIDs = append(s.PointIDs, id)
			}
		}
		if len(s.PointIDs) == 0 {
			continue // a section without valid points has nothing to show
		}
		if s.Heading == "" {
			s.Heading = fmt.Sprintf("第 %d 部分", len(sections)+1)
		}
		r.setRange(&s)
		sections = append(sections, s)
	}
	var unplaced []Point
	for _, p := range points {
		if !placed[p.ID] {
			unplaced = append(unplaced, p)
		}
	}
	if len(unplaced) == 0 {
		return sections
	}
	r.stats.PointsUnplaced += len(unplaced)
	if len(sections) == 0 {
		s := Section{Heading: otherSectionHeading}
		for _, p := range unplaced {
			s.PointIDs = append(s.PointIDs, p.ID)
		}
		r.setRange(&s)
		return []Section{s}
	}
	// Distances use the ranges of the model's own placement, so the result
	// does not depend on the order in which unplaced points are handled.
	type span struct{ start, end int64 }
	spans := make([]span, len(sections))
	for i, s := range sections {
		spans[i] = span{s.StartMs, s.EndMs}
	}
	for _, p := range unplaced {
		best, bestDist := 0, int64(-1)
		for i, sp := range spans {
			var d int64
			switch {
			case p.StartMs < sp.start:
				d = sp.start - p.StartMs
			case p.StartMs > sp.end:
				d = p.StartMs - sp.end
			}
			if bestDist < 0 || d < bestDist {
				best, bestDist = i, d
			}
		}
		sections[best].PointIDs = append(sections[best].PointIDs, p.ID)
	}
	for i := range sections {
		r.setRange(&sections[i])
	}
	return sections
}

func (r *reducer) summaryLines(chunks []*ChunkResult) []string {
	var out []string
	for _, ch := range chunks {
		if ch == nil || ch.Summary == "" {
			continue
		}
		out = append(out, fmt.Sprintf("第 %d 块 [%s–%s]：%s", ch.ChunkIndex,
			FormatTime(ch.StartMs, r.long), FormatTime(ch.EndMs, r.long), promptText(ch.Summary)))
	}
	return out
}

type reducePromptData struct {
	Task      string
	Title     string
	Duration  string
	Part      string
	Grouped   bool
	Summaries []string
	Points    []string
}

func (r *reducer) reducePrompt(task string, g group, part string) string {
	d := reducePromptData{Task: task, Title: promptText(r.in.VideoTitle), Duration: FormatTime(r.in.DurationMs, r.long),
		Part: part, Grouped: task != "reduce", Summaries: g.summaries}
	for _, p := range g.points {
		d.Points = append(d.Points, fmt.Sprintf("%s [%s] %s：%s", p.ID, FormatTime(p.StartMs, r.long),
			promptText(p.Title), promptText(p.Detail)))
	}
	return renderPrompt("reduce_user.tmpl", d)
}

// pointTokens estimates the prompt cost of one point line.
func pointTokens(p Point) int {
	return EstimateTokens(p.Title) + EstimateTokens(p.Detail) + 6
}

// groups packs the chunks, in time order, into groups of at most limit
// points whose estimated prompt stays within the single-call token budget.
// A chunk that is too large on its own is cut into several groups.
func (r *reducer) groups(limit int) []group {
	var out []group
	var cur group
	tokens := 0
	var summarised *ChunkResult // last chunk whose summary is in cur
	flush := func() {
		if len(cur.points) > 0 {
			out = append(out, cur)
		}
		cur, tokens, summarised = group{}, 0, nil
	}
	for _, ch := range r.in.Chunks {
		if ch == nil || len(ch.Points) == 0 {
			continue
		}
		chTokens := 0
		for _, p := range ch.Points {
			chTokens += pointTokens(p)
		}
		// Prefer to keep a chunk whole: start a new group when it does not
		// fit into the current one.
		if len(cur.points)+len(ch.Points) > limit || tokens+chTokens > twoLevelTokens {
			flush()
		}
		for _, p := range ch.Points {
			t := pointTokens(p)
			if len(cur.points) >= limit || (len(cur.points) > 0 && tokens+t > twoLevelTokens) {
				flush()
			}
			if summarised != ch {
				cur.summaries = append(cur.summaries, r.summaryLines([]*ChunkResult{ch})...)
				summarised = ch
			}
			cur.points = append(cur.points, p)
			tokens += t
		}
	}
	flush()
	return out
}

// reduceGroup reduces one group into sections, halving the group when the
// model cannot take it in one call.
func (r *reducer) reduceGroup(g group, part string) ([]Section, error) {
	out, err := askJSON[groupOutput](r.ctx, r.c, &r.st, r.system, r.reducePrompt("reduce_group", g, part), groupSchema, reduceMaxTokens)
	if errors.Is(err, errNeedSplit) {
		if len(g.points) < 2*minGroupPoints {
			return nil, tooLargeError()
		}
		half := len(g.points) / 2
		a, err := r.reduceGroup(group{points: g.points[:half:half], summaries: g.summaries}, part)
		if err != nil {
			return nil, err
		}
		b, err := r.reduceGroup(group{points: g.points[half:], summaries: g.summaries}, part)
		if err != nil {
			return nil, err
		}
		return append(a, b...), nil
	}
	if err != nil {
		return nil, err
	}
	return r.complete(out.Sections, g.points), nil
}

// twoLevel is the §9.3 two-level reduce: each group of chunks is reduced to
// sections, then one final call sees only the sections and produces the
// title, overview and takeaways, optionally merging adjacent sections by id.
func (r *reducer) twoLevel() ([]Section, frame, error) {
	r.stats.TwoLevel = true
	secs, err := r.reduceGroups()
	if err != nil {
		return nil, frame{}, err
	}
	out, err := r.askFinal(secs)
	if err != nil {
		return nil, frame{}, err
	}
	return r.applyMerges(secs, out.Merges), frame{out.Title, out.Overview, out.KeyTakeaways}, nil
}

// idSection is a first-level section with the ID ("G2.1") the final call
// uses to refer to it.
type idSection struct {
	id string
	Section
}

// reduceGroups is level one: split the points into groups in time order and
// reduce each group to sections.
func (r *reducer) reduceGroups() ([]idSection, error) {
	limit := groupMaxPoints
	groups := r.groups(limit)
	// When two-level was forced by a failed single call, make sure the input
	// really gets smaller.
	for len(groups) < 2 && limit > minGroupPoints {
		limit = max(limit/2, minGroupPoints)
		groups = r.groups(limit)
	}
	r.stats.Groups = len(groups)

	var secs []idSection
	for gi, g := range groups {
		part := fmt.Sprintf("第 %d 部分（共 %d 部分），%s–%s", gi+1, len(groups),
			FormatTime(g.points[0].StartMs, r.long), FormatTime(g.points[len(g.points)-1].EndMs, r.long))
		got, err := r.reduceGroup(g, part)
		if err != nil {
			return nil, err
		}
		for si, s := range got {
			secs = append(secs, idSection{fmt.Sprintf("G%d.%d", gi+1, si+1), s})
		}
	}
	return secs, nil
}

// askFinal is level two: one call that sees only the sections and produces
// the title, overview, takeaways and section merges. Section lines carry
// their point ids and titles so the overview and takeaways can cite points;
// when that is too long, fewer points per section are listed.
func (r *reducer) askFinal(secs []idSection) (*finalOutput, error) {
	build := func(perSection int) string {
		var lines []string
		for _, s := range secs {
			lines = append(lines, fmt.Sprintf("%s [%s–%s] %s：%s", s.id, FormatTime(s.StartMs, r.long),
				FormatTime(s.EndMs, r.long), promptText(s.Heading), promptText(s.Summary)))
			for i, id := range s.PointIDs {
				if perSection > 0 && i >= perSection {
					break
				}
				lines = append(lines, "  "+id+" "+promptText(r.byID[id].Title))
			}
		}
		return renderPrompt("final_user.tmpl", map[string]any{
			"Title": promptText(r.in.VideoTitle), "Duration": FormatTime(r.in.DurationMs, r.long), "Sections": lines})
	}
	caps := []int{0, 3, 1} // points listed per section; 0 = all
	for i, perSection := range caps {
		last := i == len(caps)-1
		user := build(perSection)
		if !last && EstimateTokens(user) > twoLevelTokens {
			continue
		}
		out, err := askJSON[finalOutput](r.ctx, r.c, &r.st, r.system, user, finalSchema, reduceMaxTokens)
		if errors.Is(err, errNeedSplit) && !last {
			continue
		}
		if errors.Is(err, errNeedSplit) {
			return nil, tooLargeError()
		}
		return out, err
	}
	return nil, tooLargeError() // unreachable: the last cap always returns
}

// applyMerges joins the sections the final call asked to merge. A merge is
// honoured only when it names at least two known, not yet merged, adjacent
// sections; anything else is ignored, so no point can be lost or duplicated.
func (r *reducer) applyMerges(secs []idSection, merges []outMerge) []Section {
	pos := map[string]int{}
	for i, s := range secs {
		pos[s.id] = i
	}
	absorbed := make([]bool, len(secs)) // merged into an earlier section
	used := map[int]bool{}
	for _, m := range merges {
		idx, ok := mergeTargets(m.SectionIDs, pos, used)
		if !ok {
			continue
		}
		first := &secs[idx[0]]
		used[idx[0]] = true
		for _, i := range idx[1:] {
			used[i] = true
			absorbed[i] = true
			first.PointIDs = append(first.PointIDs, secs[i].PointIDs...)
			if s := secs[i].Summary; s != "" {
				first.Summary = strings.TrimSpace(first.Summary + " " + s)
			}
			r.stats.SectionsMerged++
		}
		if h := truncateRunes(oneLine(m.Heading), 80); h != "" {
			first.Heading = h
		}
		if s := truncateRunes(oneLine(m.Summary), 500); s != "" {
			first.Summary = s
		}
	}
	var sections []Section
	for i, s := range secs {
		if !absorbed[i] {
			sections = append(sections, s.Section)
		}
	}
	return sections
}

// mergeTargets resolves the section IDs of one merge to sorted indexes and
// reports whether the merge is valid.
func mergeTargets(ids []string, pos map[string]int, used map[int]bool) ([]int, bool) {
	if len(ids) < 2 {
		return nil, false
	}
	idx := make([]int, 0, len(ids))
	for _, raw := range ids {
		i, known := pos[strings.ToUpper(strings.TrimSpace(raw))]
		if !known || used[i] {
			return nil, false
		}
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for k := 1; k < len(idx); k++ {
		if idx[k] != idx[k-1]+1 {
			return nil, false
		}
	}
	return idx, true
}
