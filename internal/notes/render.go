package notes

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"watchink/internal/app"
	"watchink/internal/llm"
)

// RenderInput is everything the renderer needs. Segments must be the full
// transcript version the chunks were built from.
type RenderInput struct {
	VideoID      string
	NoteID       string
	TranscriptID string
	Source       app.VideoSource
	VideoTitle   string
	DurationMs   int64
	Language     string
	GeneratedAt  time.Time
	// Model is the configured model name; the front matter shows it as
	// openai-compatible/<Model>.
	Model    string
	Segments []app.Segment
	Chunks   []*ChunkResult
	Reduce   *ReduceResult
}

// Document is the structured note: the canonical JSON form of a note
// (export format "json"). Refs are citation ref_no values into Citations.
type Document struct {
	VideoID         string          `json:"video_id"`
	NoteID          string          `json:"note_id"`
	TranscriptID    string          `json:"transcript_id"`
	Title           string          `json:"title"`
	VideoTitle      string          `json:"video_title"`
	Source          app.VideoSource `json:"source"`
	DurationMs      int64           `json:"duration_ms"`
	Language        string          `json:"language"`
	GeneratedAt     string          `json:"generated_at"`
	Model           string          `json:"model"`
	PromptVersion   string          `json:"prompt_version"`
	RendererVersion string          `json:"renderer_version"`
	Overview        DocOverview     `json:"overview"`
	Sections        []DocSection    `json:"sections"`
	KeyTakeaways    []DocTakeaway   `json:"key_takeaways"`
	Terms           []DocTerm       `json:"terms"`
	Citations       []app.Citation  `json:"citations"`
	Coverage        app.Coverage    `json:"coverage"`
}

// DocOverview is the overview paragraph of a Document.
type DocOverview struct {
	Text     string   `json:"text"`
	PointIDs []string `json:"point_ids"`
	Refs     []int    `json:"refs"`
	Fallback bool     `json:"fallback,omitempty"`
}

// DocSection is one numbered section of a Document.
type DocSection struct {
	No      int        `json:"no"`
	Heading string     `json:"heading"`
	Summary string     `json:"summary"`
	StartMs int64      `json:"start_ms"`
	EndMs   int64      `json:"end_ms"`
	Points  []DocPoint `json:"points"`
}

// DocPoint is one bullet of a section.
type DocPoint struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Detail  string `json:"detail"`
	Kind    string `json:"kind"`
	StartMs int64  `json:"start_ms"`
	Refs    []int  `json:"refs"`
}

// DocTakeaway is one key conclusion.
type DocTakeaway struct {
	Text     string   `json:"text"`
	PointIDs []string `json:"point_ids"`
	Refs     []int    `json:"refs"`
}

// DocTerm is one glossary row.
type DocTerm struct {
	ID          string `json:"id"`
	Term        string `json:"term"`
	Explanation string `json:"explanation"`
	Refs        []int  `json:"refs"`
}

// Rendered is the output of Render.
type Rendered struct {
	// Markdown is the canonical Markdown with wk://seg/ links.
	Markdown string
	Title    string
	Document *Document
	// Citations has one row per rendered link, ref_no 1..n in render order.
	Citations []app.Citation
	Coverage  app.Coverage
}

var orderedListRe = regexp.MustCompile(`^(\d{1,9})([.)])`)

// escapeInline makes untrusted text (model output, video titles) safe to
// embed in one line of the canonical Markdown: it cannot start a link, an
// image, raw HTML, emphasis, a code span, a table cell or a new block, and it
// cannot forge an wk:// citation.
func escapeInline(s string) string {
	s = oneLine(s)
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\\', '`', '*', '_', '[', ']', '<', '>', '|', '~', '#', '&':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	s = b.String()
	// A literal "wk://" in text would be indistinguishable from a citation
	// target when the Markdown is re-parsed; break the scheme, visibly intact.
	s = strings.ReplaceAll(s, "wk://", `wk:\/\/`)
	s = strings.ReplaceAll(s, "WK://", `WK:\/\/`)
	return s
}

// escapeBlock is escapeInline for text that starts a paragraph or list item:
// it also neutralises block markers at the start of the line.
func escapeBlock(s string) string {
	s = escapeInline(s)
	if s == "" {
		return s
	}
	switch s[0] {
	case '-', '+', '=':
		return `\` + s
	}
	if m := orderedListRe.FindStringSubmatch(s); m != nil {
		return m[1] + `\` + s[len(m[1]):]
	}
	return s
}

// safeLinkURL returns u when it is an absolute http(s) URL, escaped so that it
// cannot terminate a Markdown link destination; otherwise "".
func safeLinkURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r <= ' ' || r == 0x7f || r == '(' || r == ')' || r == '<' || r == '>' || r == '\\' || r == '"' || r == '`':
			for _, c := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// yamlString renders a front matter scalar: plain when obviously safe,
// otherwise double-quoted (JSON string syntax is valid YAML).
var yamlPlainRe = regexp.MustCompile(`^[A-Za-z0-9_./-][A-Za-z0-9_./:+@~?=&%-]*$`)

func yamlString(s string) string {
	s = oneLine(s)
	if yamlPlainRe.MatchString(s) && !strings.HasSuffix(s, ":") && !strings.Contains(strings.ToLower(s), "wk://") {
		return s
	}
	// "\/" is a valid escape in a YAML double-quoted scalar; it keeps the
	// value intact while making sure front matter can never contain the
	// citation scheme that Verify counts.
	q := strconv.Quote(s)
	q = strings.ReplaceAll(q, "wk://", `wk:\/\/`)
	return strings.ReplaceAll(q, "WK://", `WK:\/\/`)
}

// renderer carries the state of one Render call: the indexes built from the
// inputs, and the three outputs that grow section by section (the Markdown
// text, the structured document and the citations table).
type renderer struct {
	in       RenderInput
	dur      int64 // effective duration: never shorter than the transcript
	long     bool  // H:MM:SS instead of MM:SS
	segByIdx map[int]app.Segment
	points   map[string]Point
	terms    []Term // deduplicated, in time order

	b         strings.Builder
	doc       *Document
	citations []app.Citation
	cov       app.Coverage
}

// links renders the citation links for the given segment idx values of one
// point (or term), in time order, and records one citations row per link.
func (r *renderer) links(pointID string, idxs []int) (md string, refs []int, err error) {
	segs := make([]app.Segment, 0, len(idxs))
	seen := map[int]bool{}
	for _, idx := range idxs {
		s, ok := r.segByIdx[idx]
		if !ok {
			return "", nil, app.E(app.CodeInternal, "%s 引用了字幕中不存在的片段 S%d", pointID, idx)
		}
		if !seen[idx] {
			seen[idx] = true
			segs = append(segs, s)
		}
	}
	sort.SliceStable(segs, func(i, j int) bool {
		if segs[i].StartMs != segs[j].StartMs {
			return segs[i].StartMs < segs[j].StartMs
		}
		return segs[i].Idx < segs[j].Idx
	})
	var parts []string
	refs = []int{}
	for _, s := range segs {
		ref := len(r.citations) + 1
		r.citations = append(r.citations, app.Citation{RefNo: ref, PointID: pointID, SegmentID: s.SegmentID,
			StartMs: s.StartMs, EndMs: s.EndMs, Text: s.Text})
		refs = append(refs, ref)
		parts = append(parts, fmt.Sprintf("[%s](%s%s)", FormatTime(s.StartMs, r.long), app.SegmentLinkScheme, s.SegmentID))
	}
	return strings.Join(parts, " "), refs, nil
}

// pointLinks cites the earliest segment of each referenced point; it is used
// by the overview and the takeaways, which reference points, not segments.
func (r *renderer) pointLinks(ids []string) (string, []int, error) {
	var parts []string
	refs := []int{}
	seen := map[int]bool{}
	for _, id := range ids {
		p, ok := r.points[id]
		if !ok {
			return "", nil, app.E(app.CodeInternal, "笔记引用了不存在的要点 %s", id)
		}
		if len(p.Cites) == 0 {
			continue
		}
		earliest := p.Cites[0]
		for _, c := range p.Cites[1:] {
			if a, b := r.segByIdx[c], r.segByIdx[earliest]; a.StartMs < b.StartMs {
				earliest = c
			}
		}
		if seen[earliest] {
			continue
		}
		seen[earliest] = true
		md, rf, err := r.links(id, []int{earliest})
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, md)
		refs = append(refs, rf...)
	}
	return strings.Join(parts, " "), refs, nil
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// Render produces the canonical Markdown, the structured Document, the
// citations table and the coverage metadata, then re-parses its own
// output with Verify. Any inconsistency is reported as INTERNAL: it
// is a program defect, never a model problem.
func Render(in RenderInput) (*Rendered, error) {
	if in.Reduce == nil {
		return nil, app.E(app.CodeInternal, "渲染笔记时缺少 reduce 结果")
	}
	r := &renderer{in: in}
	if err := r.index(); err != nil {
		return nil, err
	}
	title := r.writeHeader()
	for _, write := range []func() error{r.writeOverview, r.writeSections, r.writeTakeaways, r.writeGlossary} {
		if err := write(); err != nil {
			return nil, err
		}
	}

	markdown := strings.TrimRight(r.b.String(), "\n") + "\n"
	citations := r.citations
	if citations == nil {
		citations = []app.Citation{}
	}
	r.cov.Citations = len(citations)
	r.doc.Citations = citations
	r.doc.Coverage = r.cov
	if err := Verify(markdown, citations, in.Segments, r.dur); err != nil {
		return nil, err
	}
	return &Rendered{Markdown: markdown, Title: title, Document: r.doc, Citations: citations, Coverage: r.cov}, nil
}

// index builds the lookup tables (segments by idx, points by ID, the term
// list) and sums the per-chunk statistics into the coverage.
func (r *renderer) index() error {
	in := r.in
	r.segByIdx = make(map[int]app.Segment, len(in.Segments))
	r.points = map[string]Point{}
	r.cov = app.Coverage{TranscriptSegments: len(in.Segments)}
	// Timestamps come from segments, so the effective duration can never be
	// shorter than the transcript (ASR may run a little past the container
	// duration).
	r.dur = in.DurationMs
	for _, s := range in.Segments {
		if _, dup := r.segByIdx[s.Idx]; dup {
			return app.E(app.CodeInternal, "字幕片段序号重复：%d", s.Idx)
		}
		r.segByIdx[s.Idx] = s
		r.dur = max(r.dur, s.EndMs)
		r.cov.TranscriptChars += utf8.RuneCountInString(s.Text)
	}
	r.long = IsLong(r.dur)

	seenTerm := map[string]bool{}
	for _, ch := range in.Chunks {
		if ch == nil {
			return app.E(app.CodeInternal, "渲染笔记时缺少某个分块的 map 结果")
		}
		r.cov.Chunks++
		if ch.Stats.Empty {
			r.cov.ChunksEmpty++
		}
		r.cov.Points += len(ch.Points)
		r.cov.PointsDropped += ch.Stats.PointsDropped
		r.cov.CitationsDropped += ch.Stats.CitationsDropped
		r.cov.RepairCalls += ch.Stats.RepairCalls
		r.cov.SplitRetries += ch.Stats.SplitRetries
		for _, p := range ch.Points {
			if _, dup := r.points[p.ID]; dup {
				return app.E(app.CodeInternal, "要点编号重复：%s", p.ID)
			}
			r.points[p.ID] = p
		}
		for _, t := range ch.Terms {
			if key := strings.ToLower(t.Term); !seenTerm[key] {
				seenTerm[key] = true
				r.terms = append(r.terms, t)
			}
		}
	}
	sort.SliceStable(r.terms, func(i, j int) bool { return r.terms[i].StartMs < r.terms[j].StartMs })
	red := in.Reduce
	r.cov.PointsUnplaced = red.Stats.PointsUnplaced
	r.cov.RepairCalls += red.Stats.RepairCalls
	r.cov.OverviewFallback = red.Overview.Fallback
	return nil
}

// writeHeader writes the front matter, the title and the source line, and
// starts the document. It returns the note title.
func (r *renderer) writeHeader() string {
	in, b := r.in, &r.b
	title := oneLine(in.Reduce.Title)
	if title == "" {
		title = oneLine(in.VideoTitle)
	}
	if title == "" {
		title = fallbackNoteTitle
	}
	generated := in.GeneratedAt.UTC().Format(time.RFC3339)
	model := llm.Provider + "/" + oneLine(in.Model)
	sourceText := oneLine(in.Source.URI)
	if sourceText == "" {
		sourceText = in.Source.Type
	}
	r.doc = &Document{VideoID: in.VideoID, NoteID: in.NoteID, TranscriptID: in.TranscriptID, Title: title,
		VideoTitle: in.VideoTitle, Source: in.Source, DurationMs: r.dur, Language: in.Language,
		GeneratedAt: generated, Model: model, PromptVersion: PromptVersion, RendererVersion: RendererVersion,
		Sections: []DocSection{}, KeyTakeaways: []DocTakeaway{}, Terms: []DocTerm{}}

	b.WriteString("---\nwatchink:\n")
	for _, kv := range [][2]string{
		{"video_id", yamlString(in.VideoID)},
		{"note_id", yamlString(in.NoteID)},
		{"transcript_id", yamlString(in.TranscriptID)},
		{"source", yamlString(sourceText)},
		{"duration", strconv.Quote(FormatTime(r.dur, r.long))},
		{"language", yamlString(in.Language)},
		{"generated_at", generated},
		{"model", yamlString(model)},
		{"prompt_version", yamlString(PromptVersion)},
	} {
		fmt.Fprintf(b, "  %s: %s\n", kv[0], kv[1])
	}
	b.WriteString("---\n\n")
	fmt.Fprintf(b, "# %s\n\n", escapeInline(title))

	videoTitle := escapeInline(in.VideoTitle)
	if videoTitle == "" {
		videoTitle = "未命名视频"
	}
	if link := safeLinkURL(in.Source.URI); link != "" && (in.Source.Type == app.SourceYouTube || in.Source.Type == app.SourceBilibili) {
		fmt.Fprintf(b, "> 来源：[%s](%s) · 时长 %s\n\n", videoTitle, link, FormatTime(r.dur, r.long))
	} else {
		fmt.Fprintf(b, "> 来源：%s（本地文件） · 时长 %s\n\n", videoTitle, FormatTime(r.dur, r.long))
	}
	return title
}

func (r *renderer) writeOverview() error {
	ov := r.in.Reduce.Overview
	r.b.WriteString("## 概览\n\n")
	md, refs, err := r.pointLinks(ov.PointIDs)
	if err != nil {
		return err
	}
	r.doc.Overview = DocOverview{Text: oneLine(ov.Text), PointIDs: append([]string{}, ov.PointIDs...), Refs: refs, Fallback: ov.Fallback}
	text := escapeBlock(ov.Text)
	if text == "" {
		text = "（未能生成概览）"
	}
	r.b.WriteString(joinNonEmpty(text, md) + "\n\n")
	return nil
}

// writeSections writes one "## n. heading" block per section and checks the
// invariant: every valid point is rendered exactly once.
func (r *renderer) writeSections() error {
	rendered := map[string]bool{}
	for i, s := range r.in.Reduce.Sections {
		ds := DocSection{No: i + 1, Heading: oneLine(s.Heading), Summary: oneLine(s.Summary), Points: []DocPoint{}}
		var body strings.Builder
		for j, id := range s.PointIDs {
			p, ok := r.points[id]
			if !ok {
				return app.E(app.CodeInternal, "章节引用了不存在的要点 %s", id)
			}
			if rendered[id] {
				return app.E(app.CodeInternal, "要点 %s 被放进了多个章节", id)
			}
			rendered[id] = true
			md, refs, err := r.links(id, p.Cites)
			if err != nil {
				return err
			}
			if len(refs) == 0 {
				return app.E(app.CodeInternal, "要点 %s 没有引用", id)
			}
			first := r.citations[refs[0]-1]
			if j == 0 || first.StartMs < ds.StartMs {
				ds.StartMs = first.StartMs
			}
			for _, ref := range refs {
				ds.EndMs = max(ds.EndMs, r.citations[ref-1].StartMs)
			}
			text := escapeInline(p.Detail)
			if t := escapeInline(p.Title); t != "" {
				text = "**" + t + "**"
				if d := escapeInline(p.Detail); d != "" {
					text += "：" + d
				}
			}
			body.WriteString("- " + joinNonEmpty(text, md) + "\n")
			ds.Points = append(ds.Points, DocPoint{ID: id, Title: p.Title, Detail: p.Detail, Kind: p.Kind, StartMs: first.StartMs, Refs: refs})
		}
		if len(ds.Points) == 0 {
			return app.E(app.CodeInternal, "章节 %d 没有任何要点", i+1)
		}
		heading := escapeInline(s.Heading)
		if heading == "" {
			heading = fmt.Sprintf("第 %d 部分", i+1)
		}
		// The range shown is that of the cited segment starts, i.e. exactly
		// the timestamps the reader sees in this section.
		fmt.Fprintf(&r.b, "## %d. %s `%s–%s`\n\n", i+1, heading, FormatTime(ds.StartMs, r.long), FormatTime(ds.EndMs, r.long))
		if sum := escapeBlock(s.Summary); sum != "" {
			r.b.WriteString(sum + "\n\n")
		}
		r.b.WriteString(body.String() + "\n")
		r.doc.Sections = append(r.doc.Sections, ds)
	}
	if len(rendered) != len(r.points) {
		return app.E(app.CodeInternal, "有 %d 个有效要点没有出现在笔记中", len(r.points)-len(rendered))
	}
	return nil
}

func (r *renderer) writeTakeaways() error {
	takeaways := r.in.Reduce.Takeaways
	if len(takeaways) == 0 {
		return nil
	}
	r.b.WriteString("## 关键结论\n\n")
	for _, t := range takeaways {
		md, refs, err := r.pointLinks(t.PointIDs)
		if err != nil {
			return err
		}
		r.b.WriteString("- " + joinNonEmpty(escapeInline(t.Text), md) + "\n")
		r.doc.KeyTakeaways = append(r.doc.KeyTakeaways, DocTakeaway{Text: oneLine(t.Text), PointIDs: append([]string{}, t.PointIDs...), Refs: refs})
	}
	r.b.WriteString("\n")
	return nil
}

func (r *renderer) writeGlossary() error {
	if len(r.terms) == 0 {
		return nil
	}
	r.b.WriteString("## 术语\n\n| 术语 | 说明 | 出处 |\n| --- | --- | --- |\n")
	for _, t := range r.terms {
		md, refs, err := r.links(t.ID, t.Cites)
		if err != nil {
			return err
		}
		fmt.Fprintf(&r.b, "| %s | %s | %s |\n", escapeInline(t.Term), escapeInline(t.Explanation), md)
		r.doc.Terms = append(r.doc.Terms, DocTerm{ID: t.ID, Term: t.Term, Explanation: t.Explanation, Refs: refs})
	}
	r.b.WriteString("\n")
	return nil
}

// citationLinkRe matches a rendered citation: an unescaped "[time]" label
// followed by an wk://seg/ destination. Escaped text can never match because
// escapeInline puts a backslash before every "[" and "]".
var citationLinkRe = regexp.MustCompile(`\[([0-9]+(?::[0-9]{2}){1,2})\]\(wk://seg/([A-Za-z0-9_]+)\)`)

type parsedLink struct {
	label     string
	segmentID string
	start     int // byte offset of "["
	end       int
}

func parseCitationLinks(md string) []parsedLink {
	var out []parsedLink
	for _, m := range citationLinkRe.FindAllStringSubmatchIndex(md, -1) {
		if m[0] > 0 && md[m[0]-1] == '\\' {
			continue
		}
		out = append(out, parsedLink{label: md[m[2]:m[3]], segmentID: md[m[4]:m[5]], start: m[0], end: m[1]})
	}
	return out
}

// Verify re-parses a canonical Markdown note and checks the three invariants
// : every wk://seg/ link resolves to a segment of this transcript, its
// time lies in [0, durationMs], and links and citation rows correspond one to
// one (same order, same segment, ref_no 1..n). A violation is INTERNAL.
func Verify(markdown string, citations []app.Citation, segments []app.Segment, durationMs int64) error {
	fail := func(format string, args ...any) error {
		return app.E(app.CodeInternal, "笔记引用校验失败："+format, args...)
	}
	byID := make(map[string]app.Segment, len(segments))
	for _, s := range segments {
		byID[s.SegmentID] = s
	}
	links := parseCitationLinks(markdown)
	if raw := strings.Count(markdown, app.SegmentLinkScheme); raw != len(links) {
		return fail("Markdown 中有 %d 处 wk://seg/，但只有 %d 个是合法的引用链接", raw, len(links))
	}
	if len(links) != len(citations) {
		return fail("Markdown 中有 %d 个引用链接，引用表有 %d 行", len(links), len(citations))
	}
	long := IsLong(durationMs)
	for i, l := range links {
		seg, ok := byID[l.segmentID]
		if !ok {
			return fail("第 %d 个链接指向不属于本字幕版本的片段 %s", i+1, l.segmentID)
		}
		if seg.StartMs < 0 || seg.StartMs > durationMs {
			return fail("第 %d 个链接的时间 %d ms 超出视频时长 %d ms", i+1, seg.StartMs, durationMs)
		}
		c := citations[i]
		if c.RefNo != i+1 || c.SegmentID != l.segmentID || c.StartMs != seg.StartMs || c.EndMs != seg.EndMs {
			return fail("第 %d 个链接与引用表第 %d 行不一致", i+1, i+1)
		}
		if want := FormatTime(seg.StartMs, long); l.label != want {
			return fail("第 %d 个链接显示的时间 %s 与片段起始时间 %s 不一致", i+1, l.label, want)
		}
	}
	return nil
}
