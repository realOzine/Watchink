package notes

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"watchink/internal/app"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from golden file.\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// fixture is a small hand-written note: two chunks, three sections' worth of
// points, a takeaway and glossary terms, with deliberately hostile text.
func fixture(segMs int64, source app.VideoSource) RenderInput {
	segs := mkSegs(12, segMs, 0, func(i int) string { return "片段" + string(rune('A'+i-1)) + " 的原文" })
	at := func(idx int) int64 { return segs[idx-1].StartMs }
	end := func(idx int) int64 { return segs[idx-1].EndMs }
	c1 := &ChunkResult{ChunkIndex: 1, Key: "S1-S6", FirstIdx: 1, LastIdx: 6, StartMs: at(1), EndMs: end(6), Summary: "第一块摘要。",
		Points: []Point{
			{ID: "P1.1", Title: "梯度下降", Detail: "沿负梯度方向更新参数。", Kind: "method", Cites: []int{2, 3}, StartMs: at(2), EndMs: end(3)},
			{ID: "P1.2", Title: "学习率 [重要](javascript:alert(1))", Detail: "步长过大 <script>alert(1)</script> 会发散 | 过小则很慢\n换行 *强调* `代码`", Kind: "caution", Cites: []int{5}, StartMs: at(5), EndMs: end(5)},
			{ID: "P1.3", Title: "", Detail: "没有标题的要点", Kind: "other", Cites: []int{6}, StartMs: at(6), EndMs: end(6)},
		},
		Terms: []Term{
			{ID: "T1.1", Term: "learning rate", Explanation: "每次更新的步长 | 也叫 step size", Cites: []int{5}, StartMs: at(5)},
			{ID: "T1.2", Term: "Gradient", Explanation: "损失函数的导数", Cites: []int{2}, StartMs: at(2)},
		},
		Stats: ChunkStats{PointsDropped: 1, CitationsDropped: 2, RepairCalls: 1}}
	c2 := &ChunkResult{ChunkIndex: 2, Key: "S7-S12+ctxS4", FirstIdx: 7, LastIdx: 12, StartMs: at(7), EndMs: end(12), Summary: "第二块摘要。",
		Points: []Point{
			{ID: "P2.1", Title: "动量法", Detail: "累积历史梯度 [00:01](wk://seg/seg_FAKE) 以加速收敛。", Kind: "concept", Cites: []int{8, 7, 12}, StartMs: at(7), EndMs: end(12)},
			{ID: "P2.2", Title: "Adam", Detail: "", Kind: "method", Cites: []int{10}, StartMs: at(10), EndMs: end(10)},
		},
		Terms: []Term{
			{ID: "T2.1", Term: "gradient", Explanation: "重复术语，不应再出现", Cites: []int{7}, StartMs: at(7)},
			{ID: "T2.2", Term: "Momentum", Explanation: "动量", Cites: []int{7, 8}, StartMs: at(7)},
		},
		Stats: ChunkStats{SplitRetries: 1}}
	red := &ReduceResult{
		Title:    "优化算法 # 入门",
		Overview: Overview{Text: "1. 本讲介绍梯度下降及其变体。", PointIDs: []string{"P1.1", "P2.1", "P1.2"}},
		Sections: []Section{
			{Heading: "基础 > 梯度下降", Summary: "- 介绍最基本的优化方法。", PointIDs: []string{"P1.1", "P1.2", "P1.3"}},
			{Heading: "改进方法", Summary: "", PointIDs: []string{"P2.2", "P2.1"}},
		},
		Takeaways: []Takeaway{
			{Text: "学习率是最重要的超参数。", PointIDs: []string{"P1.2"}},
			{Text: "动量和 Adam 能加速收敛。", PointIDs: []string{"P2.1", "P2.2"}},
		},
		Stats: ReduceStats{Points: 5, PointsUnplaced: 1, RepairCalls: 1},
	}
	return RenderInput{VideoID: "vid_01JA2M8Q0000000000000000CD", NoteID: "note_01JA2M8Q0000000000000000EF", TranscriptID: testTranscriptID,
		Source: source, VideoTitle: "优化算法 [第 3 讲]", DurationMs: end(12), Language: "zh",
		GeneratedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.FixedZone("CST", 8*3600)), Model: "gpt-x",
		Segments: segs, Chunks: []*ChunkResult{c1, c2}, Reduce: red}
}

func TestRenderGolden(t *testing.T) {
	short, err := Render(fixture(30000, app.VideoSource{Type: app.SourceFile, URI: "/Users/me/课程/第 3 讲.mp4"}))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "note_short.md.golden", short.Markdown)
	doc, _ := json.MarshalIndent(short.Document, "", "  ")
	golden(t, "note_short.json.golden", string(doc)+"\n")

	long, err := Render(fixture(600000, app.VideoSource{Type: app.SourceYouTube, URI: "https://www.youtube.com/watch?v=dQw4w9WgXcQ", PlatformID: "dQw4w9WgXcQ"}))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "note_long.md.golden", long.Markdown)

	// Spot checks that do not depend on the golden files.
	mustContain(t, short.Markdown,
		"---\nwatchink:\n  video_id: vid_01JA2M8Q0000000000000000CD\n  note_id: note_01JA2M8Q0000000000000000EF\n  transcript_id: "+testTranscriptID+"\n",
		"  source: \"/Users/me/课程/第 3 讲.mp4\"\n  duration: \"06:00\"\n  language: zh\n  generated_at: 2026-10-09T04:00:00Z\n  model: openai-compatible/gpt-x\n  prompt_version: note-v0.1\n---\n\n# 优化算法 \\# 入门\n\n> 来源：优化算法 \\[第 3 讲\\]（本地文件） · 时长 06:00\n\n## 概览\n\n",
		"## 1. 基础 \\> 梯度下降 `00:30–02:30`\n\n\\- 介绍最基本的优化方法。\n\n- **梯度下降**：沿负梯度方向更新参数。 [00:30](wk://seg/seg_01JA2M8Q0000000000000000AB_00002) [01:00](wk://seg/seg_01JA2M8Q0000000000000000AB_00003)\n",
		"## 2. 改进方法 `03:00–05:30`\n\n- **Adam** [04:30](",
		"## 关键结论\n\n- 学习率是最重要的超参数。 [02:00](",
		"## 术语\n\n| 术语 | 说明 | 出处 |\n| --- | --- | --- |\n| Gradient | 损失函数的导数 | [00:30](",
	)
	mustContain(t, long.Markdown, "  source: https://www.youtube.com/watch?v=dQw4w9WgXcQ\n  duration: \"2:00:00\"\n",
		"> 来源：[优化算法 \\[第 3 讲\\]](https://www.youtube.com/watch?v=dQw4w9WgXcQ) · 时长 2:00:00\n",
		"## 1. 基础 \\> 梯度下降 `0:10:00–0:50:00`", "[1:50:00](wk://seg/seg_01JA2M8Q0000000000000000AB_00012)")
}

func TestRenderCitationsAndCoverage(t *testing.T) {
	in := fixture(30000, app.VideoSource{Type: app.SourceUpload})
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	// One row per rendered link, numbered in render order.
	links := parseCitationLinks(out.Markdown)
	if len(links) != len(out.Citations) || len(links) != 18 {
		t.Fatalf("links=%d citations=%d", len(links), len(out.Citations))
	}
	segByID := map[string]app.Segment{}
	for _, s := range in.Segments {
		segByID[s.SegmentID] = s
	}
	for i, c := range out.Citations {
		s := segByID[c.SegmentID]
		if c.RefNo != i+1 || c.SegmentID != links[i].segmentID || c.StartMs != s.StartMs || c.EndMs != s.EndMs || c.Text != s.Text || c.PointID == "" {
			t.Errorf("citation %d = %+v", i, c)
		}
	}
	// Overview cites the first segment of P1.1, P2.1, P1.2 in the given
	// order; a point's own cites are in time order (S7, S8, S12).
	var pointIDs []string
	for _, c := range out.Citations {
		pointIDs = append(pointIDs, c.PointID)
	}
	want := "P1.1 P2.1 P1.2" + // overview
		" P1.1 P1.1 P1.2 P1.3 P2.2 P2.1 P2.1 P2.1" + // sections
		" P1.2 P2.1 P2.2" + // takeaways
		" T1.2 T1.1 T2.2 T2.2" // glossary, by first appearance
	if got := strings.Join(pointIDs, " "); got != want {
		t.Errorf("citation point ids = %s\nwant %s", got, want)
	}
	if o := out.Citations[1]; o.StartMs != 180000 {
		t.Errorf("overview must cite the earliest segment of P2.1, got %d ms", o.StartMs)
	}
	p21 := out.Document.Sections[1].Points[1]
	var starts []int64
	for _, ref := range p21.Refs {
		starts = append(starts, out.Citations[ref-1].StartMs)
	}
	if !reflect.DeepEqual(starts, []int64{180000, 210000, 330000}) {
		t.Errorf("P2.1 cites not in time order: %v", starts)
	}
	wantCov := app.Coverage{Chunks: 2, ChunksEmpty: 0, Points: 5, PointsDropped: 1, PointsUnplaced: 1, Citations: 18,
		CitationsDropped: 2, RepairCalls: 2, SplitRetries: 1, TranscriptSegments: 12, TranscriptChars: 12 * 7}
	if out.Coverage != wantCov || out.Document.Coverage != wantCov {
		t.Errorf("coverage = %+v\nwant       %+v", out.Coverage, wantCov)
	}
	if out.Title != "优化算法 # 入门" || out.Document.Title != out.Title || len(out.Document.Terms) != 3 ||
		!reflect.DeepEqual(out.Document.Citations, out.Citations) || out.Document.RendererVersion != RendererVersion {
		t.Errorf("document metadata wrong: %+v", out.Document)
	}
	mustContain(t, out.Markdown, "> 来源：优化算法 \\[第 3 讲\\]（本地文件） · 时长 06:00", "  source: upload\n")
}

var (
	rawHTMLRe  = regexp.MustCompile(`(^|[^\\])<[A-Za-z/!]`)
	anyLinkRe  = regexp.MustCompile(`(^|[^\\])\]\(([^)]*)\)`)
	tableRowRe = regexp.MustCompile(`(^|[^\\])\|`)
)

// checkMarkdownSafety asserts the output rules on a canonical note: no
// raw HTML, only wk:// and http(s) link destinations, intact table rows.
func checkMarkdownSafety(t *testing.T, md string) {
	t.Helper()
	body := md[strings.Index(md, "\n---\n")+5:]
	inTable := false
	for _, line := range strings.Split(body, "\n") {
		if rawHTMLRe.MatchString(line) {
			t.Errorf("raw HTML in line: %s", line)
		}
		for _, m := range anyLinkRe.FindAllStringSubmatch(line, -1) {
			if !strings.HasPrefix(m[2], "wk://seg/") && !strings.HasPrefix(m[2], "https://") && !strings.HasPrefix(m[2], "http://") {
				t.Errorf("link with a forbidden destination %q in line: %s", m[2], line)
			}
		}
		if strings.HasPrefix(line, "| ") {
			inTable = true
			if n := len(tableRowRe.FindAllString(line, -1)); n != 4 {
				t.Errorf("table row has %d unescaped pipes: %s", n, line)
			}
		} else if inTable && line != "" {
			t.Errorf("table interrupted by: %s", line)
		}
		if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "# ") && !strings.HasPrefix(line, "## ") {
			t.Errorf("unexpected heading level: %s", line)
		}
	}
}

func TestRenderEscapesHostileText(t *testing.T) {
	hostile := []string{
		`](javascript:alert(1))`, `[x](javascript:alert(1))`, `<script>alert(1)</script>`, `<img src=x onerror=alert(1)>`,
		"a | b || c", "line1\nline2\r\n\tline3", `[00:01](wk://seg/seg_01JA2M8Q0000000000000000AB_00001)`,
		`wk://seg/whatever WK://SEG/x`, "# heading", "- item", "+ item", "1. item", "12) item", "> quote", "=== setext", "--- rule",
		"**bold** _em_ `code` ~~strike~~", `\\ backslash \[`, "![img](http://x/y.png)", "&lt;b&gt; &#60;i&#62;", "```fence", "| a | b |",
		"<!-- comment -->", "[ref]: http://evil.example", "\x00\x07control\x1b[31m", "",
	}
	for i, h := range hostile {
		in := fixture(30000, app.VideoSource{Type: app.SourceBilibili, URI: "https://www.bilibili.com/video/BV1xx411c7mD?p=2&x=(1)<2> 3"})
		in.VideoTitle = h
		in.Reduce.Title = h
		in.Reduce.Overview.Text = h
		in.Reduce.Sections[0].Heading = h
		in.Reduce.Sections[0].Summary = h
		in.Reduce.Takeaways[0].Text = h
		in.Chunks[0].Points[0].Title = h
		in.Chunks[0].Points[0].Detail = h
		in.Chunks[0].Terms[0].Term = h + "t"
		in.Chunks[0].Terms[0].Explanation = h
		in.Model = h
		in.Language = h
		out, err := Render(in) // Render runs Verify: forged citations would fail here
		if err != nil {
			t.Fatalf("hostile[%d] %q: %v", i, h, err)
		}
		checkMarkdownSafety(t, out.Markdown)
		if len(out.Citations) != 18 {
			t.Errorf("hostile[%d] %q: %d citations, want 18", i, h, len(out.Citations))
		}
		// Structure is intact: front matter closed once, fixed headings.
		if strings.Count(out.Markdown, "\n---\n") != 1 || strings.Count(out.Markdown, "\n## ") != 5 || strings.Count(out.Markdown, "\n# ") != 1 {
			t.Errorf("hostile[%d] %q broke the structure:\n%s", i, h, out.Markdown)
		}
		fm := out.Markdown[:strings.Index(out.Markdown, "\n---\n")]
		if n := strings.Count(fm, "\n"); n != 10 {
			t.Errorf("hostile[%d] %q: front matter has %d lines", i, h, n)
		}
		mustContain(t, out.Markdown, "](https://www.bilibili.com/video/BV1xx411c7mD?p=2&x=%281%29%3C2%3E%203) · 时长")
	}
	// A non-http source URI is never rendered as a link.
	in := fixture(30000, app.VideoSource{Type: app.SourceYouTube, URI: "javascript:alert(1)"})
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	checkMarkdownSafety(t, out.Markdown)
	mustContain(t, out.Markdown, "> 来源：优化算法 \\[第 3 讲\\]（本地文件）")
}

func TestRenderRejectsInconsistentInput(t *testing.T) {
	src := app.VideoSource{Type: app.SourceFile}
	cases := []struct {
		name string
		mod  func(*RenderInput)
	}{
		{"nil reduce", func(in *RenderInput) { in.Reduce = nil }},
		{"nil chunk", func(in *RenderInput) { in.Chunks[1] = nil }},
		{"point missing from sections", func(in *RenderInput) { in.Reduce.Sections[1].PointIDs = []string{"P2.2"} }},
		{"point in two sections", func(in *RenderInput) { in.Reduce.Sections[1].PointIDs = []string{"P2.2", "P2.1", "P1.1"} }},
		{"unknown point in section", func(in *RenderInput) { in.Reduce.Sections[1].PointIDs = []string{"P2.2", "P2.1", "P9.9"} }},
		{"unknown point in overview", func(in *RenderInput) { in.Reduce.Overview.PointIDs = []string{"P9.9"} }},
		{"unknown point in takeaway", func(in *RenderInput) { in.Reduce.Takeaways[0].PointIDs = []string{"P9.9"} }},
		{"cite outside transcript", func(in *RenderInput) { in.Chunks[0].Points[0].Cites = []int{99} }},
		{"point without cites", func(in *RenderInput) { in.Chunks[0].Points[2].Cites = nil }},
		{"term cite outside transcript", func(in *RenderInput) { in.Chunks[0].Terms[0].Cites = []int{0} }},
		{"duplicate point id", func(in *RenderInput) { in.Chunks[1].Points[0].ID = "P1.1" }},
		{"duplicate segment idx", func(in *RenderInput) { in.Segments[3].Idx = 3 }},
		{"empty section", func(in *RenderInput) {
			in.Reduce.Sections = append(in.Reduce.Sections, Section{Heading: "空"})
		}},
		{"segment id that cannot be a link", func(in *RenderInput) { in.Segments[1].SegmentID = "seg bad)id" }},
		{"negative segment time", func(in *RenderInput) { in.Segments[1].StartMs = -5 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture(30000, src)
			tc.mod(&in)
			if _, err := Render(in); !app.IsCode(err, app.CodeInternal) {
				t.Fatalf("err = %v, want INTERNAL", err)
			}
		})
	}
}

func TestRenderMinimalNotes(t *testing.T) {
	// No points at all (silent video): still a valid note.
	segs := mkSegs(2, 5000, 0, func(int) string { return "" })
	empty := &ChunkResult{ChunkIndex: 1, Points: []Point{}, Terms: []Term{}, Stats: ChunkStats{Empty: true}}
	out, err := Render(RenderInput{VideoID: "vid_x", NoteID: "note_x", TranscriptID: testTranscriptID, VideoTitle: "", DurationMs: 0,
		GeneratedAt: time.Unix(0, 0), Segments: segs, Chunks: []*ChunkResult{empty},
		Reduce: &ReduceResult{Overview: Overview{Fallback: true}}})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out.Markdown, "# 讲座笔记\n\n> 来源：未命名视频（本地文件） · 时长 00:10\n\n## 概览\n\n（未能生成概览）\n")
	if strings.Contains(out.Markdown, "关键结论") || strings.Contains(out.Markdown, "术语") || out.Citations == nil || len(out.Citations) != 0 {
		t.Errorf("unexpected content:\n%s", out.Markdown)
	}
	want := app.Coverage{Chunks: 1, ChunksEmpty: 1, TranscriptSegments: 2, OverviewFallback: true}
	if out.Coverage != want {
		t.Errorf("coverage = %+v", out.Coverage)
	}
	// ASR running past the probed duration must not fail the render: the
	// effective duration is at least the end of the transcript.
	in := fixture(30000, app.VideoSource{Type: app.SourceFile})
	in.DurationMs = 100000
	if out, err = Render(in); err != nil || out.Document.DurationMs != 360000 {
		t.Fatalf("err=%v", err)
	}
	// Exactly one hour switches every timestamp to H:MM:SS.
	in = fixture(30000, app.VideoSource{Type: app.SourceFile})
	in.DurationMs = 3600000
	if out, err = Render(in); err != nil {
		t.Fatal(err)
	}
	mustContain(t, out.Markdown, "duration: \"1:00:00\"", "`0:00:30–0:02:30`", "[0:00:30](wk://seg/")
}

func TestVerifyCatchesTampering(t *testing.T) {
	in := fixture(30000, app.VideoSource{Type: app.SourceFile})
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	md, cits, segs, dur := out.Markdown, out.Citations, in.Segments, int64(360000)
	if err := Verify(md, cits, segs, dur); err != nil {
		t.Fatalf("untampered note rejected: %v", err)
	}
	link3 := "[01:00](wk://seg/seg_01JA2M8Q0000000000000000AB_00003)"
	if !strings.Contains(md, link3) {
		t.Fatal("fixture changed")
	}
	clone := func(mod func([]app.Citation) []app.Citation) []app.Citation {
		return mod(append([]app.Citation(nil), cits...))
	}
	cases := []struct {
		name string
		md   string
		cits []app.Citation
		segs []app.Segment
		dur  int64
	}{
		{"link to a segment of another transcript", strings.Replace(md, "0000AB_00003)", "0000ZZ_00003)", 1), cits, segs, dur},
		{"link to a different segment", strings.Replace(md, link3, "[01:00](wk://seg/seg_01JA2M8Q0000000000000000AB_00004)", 1), cits, segs, dur},
		{"wrong time label", strings.Replace(md, link3, "[09:59](wk://seg/seg_01JA2M8Q0000000000000000AB_00003)", 1), cits, segs, dur},
		{"link removed", strings.Replace(md, " "+link3, "", 1), cits, segs, dur},
		{"link added", md + "\n" + link3 + "\n", cits, segs, dur},
		{"links swapped", strings.Replace(strings.Replace(md, link3, "@@", 1), "[00:30](wk://seg/seg_01JA2M8Q0000000000000000AB_00002) @@", link3+" [00:30](wk://seg/seg_01JA2M8Q0000000000000000AB_00002)", 1), cits, segs, dur},
		{"bare wk:// text", md + "\n参见 wk://seg/seg_01JA2M8Q0000000000000000AB_00003\n", cits, segs, dur},
		{"malformed link", strings.Replace(md, link3, "[01:00](wk://seg/seg 3)", 1), cits, segs, dur},
		{"citation row missing", md, cits[:len(cits)-1], segs, dur},
		{"citation row extra", md, clone(func(c []app.Citation) []app.Citation { return append(c, c[0]) }), segs, dur},
		{"citation rows reordered", md, clone(func(c []app.Citation) []app.Citation { c[3], c[4] = c[4], c[3]; return c }), segs, dur},
		{"ref_no not sequential", md, clone(func(c []app.Citation) []app.Citation { c[5].RefNo = 99; return c }), segs, dur},
		{"citation time differs from segment", md, clone(func(c []app.Citation) []app.Citation { c[0].StartMs++; return c }), segs, dur},
		{"segment missing from transcript", md, cits, segs[:11], dur},
		{"time beyond duration", md, cits, segs, 300000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.md == md && reflect.DeepEqual(tc.cits, cits) && len(tc.segs) == len(segs) && tc.dur == dur {
				t.Fatal("test case does not tamper with anything")
			}
			err := Verify(tc.md, tc.cits, tc.segs, tc.dur)
			if !app.IsCode(err, app.CodeInternal) {
				t.Fatalf("err = %v, want INTERNAL", err)
			}
		})
	}
	if err := Verify("no links", nil, nil, 0); err != nil {
		t.Fatalf("empty note: %v", err)
	}
}
