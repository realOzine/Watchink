package llmtest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The block delimiters and line formats below mirror the prompt templates of
// internal/capability/notes; the notes tests fail if the two drift apart.
var (
	segLineRe     = regexp.MustCompile(`^S(\d+) \[[^\]]*\] ?(.*)$`)
	pointLineRe   = regexp.MustCompile(`^(P\d+\.\d+) `)
	sectionLineRe = regexp.MustCompile(`^(G\d+\.\d+) `)
	pointIDRe     = regexp.MustCompile(`P\d+\.\d+`)
	titleRe       = regexp.MustCompile(`(?m)^视频标题：(.*)$`)
)

type segLine struct {
	id   string
	text string
}

type prompt struct {
	task     string
	title    string
	segs     []segLine // map: citable lines
	points   []string  // reduce / reduce_group: point ids in prompt order
	sections []string  // reduce_final: section ids
	secPts   []string  // reduce_final: point ids listed under the sections
}

// block returns the lines between "<<<NAME" and "NAME>>>".
func block(user, name string) []string {
	_, rest, ok := strings.Cut(user, "<<<"+name+"\n")
	if !ok {
		return nil
	}
	body, _, ok := strings.Cut(rest, "\n"+name+">>>")
	if !ok {
		// An empty block has the closing delimiter right after the opening.
		if strings.HasPrefix(rest, name+">>>") {
			return nil
		}
		body = rest
	}
	return strings.Split(body, "\n")
}

func parsePrompt(task, user string) *prompt {
	p := &prompt{task: task, title: "讲座"}
	if m := titleRe.FindStringSubmatch(user); m != nil && strings.TrimSpace(m[1]) != "" {
		p.title = strings.TrimSpace(m[1])
	}
	switch task {
	case TaskMap:
		for _, line := range block(user, "CITABLE") {
			if m := segLineRe.FindStringSubmatch(line); m != nil {
				p.segs = append(p.segs, segLine{id: "S" + m[1], text: m[2]})
			}
		}
	case TaskReduce, TaskReduceGroup:
		for _, line := range block(user, "POINTS") {
			if m := pointLineRe.FindStringSubmatch(line); m != nil {
				p.points = append(p.points, m[1])
			}
		}
	case TaskReduceFinal:
		for _, line := range block(user, "SECTIONS") {
			if m := sectionLineRe.FindStringSubmatch(line); m != nil {
				p.sections = append(p.sections, m[1])
				continue
			}
			if id := pointIDRe.FindString(line); id != "" {
				p.secPts = append(p.secPts, id)
			}
		}
	}
	return p
}

func snippet(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

var kinds = []string{"concept", "definition", "method", "example", "conclusion", "caution", "other"}

type obj = map[string]any

// answer builds the JSON content for the prompt; kind selects a scripted
// variation of the otherwise valid answer.
func (p *prompt) answer(kind string) string {
	var v any
	switch p.task {
	case TaskMap:
		v = p.mapAnswer(kind)
	case TaskReduce:
		v = p.reduceAnswer(kind, true)
	case TaskReduceGroup:
		v = p.reduceAnswer(kind, false)
	case TaskReduceFinal:
		v = p.finalAnswer()
	default:
		v = obj{"ok": true}
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func (p *prompt) mapAnswer(kind string) obj {
	n := len(p.segs)
	points := []obj{}
	terms := []obj{}
	if n > 0 && kind != KindEmptyPoints {
		// About one point per six segments, between one and eight per chunk.
		np := min(max(n/6, 1), 8)
		for i := 0; i < np; i++ {
			lo, hi := i*n/np, (i+1)*n/np
			var cites []string
			for j := lo; j < hi && len(cites) < 3; j++ {
				cites = append(cites, p.segs[j].id)
			}
			switch kind {
			case KindBadCites:
				cites = []string{"S999999", "S0"}
			case KindMixedCites:
				cites = append(cites[:1:1], "S999999")
			}
			points = append(points, obj{
				"title":  fmt.Sprintf("要点 %s：%s", p.segs[lo].id, snippet(p.segs[lo].text, 12)),
				"detail": "这一部分讲到：" + snippet(p.segs[lo].text, 60) + "。",
				"kind":   kinds[i%len(kinds)],
				"cites":  cites,
			})
		}
		if term := snippet(p.segs[0].text, 6); term != "" && kind != KindBadCites {
			terms = append(terms, obj{"term": term, "explanation": "本块开头出现的说法。", "cites": []string{p.segs[0].id}})
		}
	}
	summary := "本块没有可整理的内容。"
	if n > 0 {
		summary = fmt.Sprintf("本块从 %s 讲到 %s，共 %d 个片段。", p.segs[0].id, p.segs[n-1].id, n)
	}
	return obj{"chunk_summary": summary, "points": points, "terms": terms}
}

func (p *prompt) reduceAnswer(kind string, full bool) obj {
	sections := []obj{}
	const perSection = 5
	for i := 0; i < len(p.points); i += perSection {
		ids := p.points[i:min(i+perSection, len(p.points))]
		if kind == KindOmitPoints {
			var kept []string
			for j, id := range ids {
				if j%2 == 0 {
					kept = append(kept, id)
				}
			}
			ids = kept
		}
		sections = append(sections, obj{
			"heading":   fmt.Sprintf("第 %d 部分", len(sections)+1),
			"summary":   fmt.Sprintf("这一部分整理了从 %s 开始的 %d 个要点。", ids[0], len(ids)),
			"point_ids": ids,
		})
	}
	if !full {
		return obj{"sections": sections}
	}
	out := p.frame(p.points)
	out["sections"] = sections
	return out
}

// frame is the title / overview / key_takeaways part shared by the single
// reduce and the final reduce.
func (p *prompt) frame(points []string) obj {
	pick := func(i int) []string {
		if len(points) == 0 {
			return []string{}
		}
		return []string{points[i%len(points)]}
	}
	overviewIDs := []string{}
	takeaways := []obj{}
	if n := len(points); n > 0 {
		overviewIDs = append(overviewIDs, points[0])
		if n > 2 {
			overviewIDs = append(overviewIDs, points[n/2])
		}
		for i := 0; i < min(3, n); i++ {
			takeaways = append(takeaways, obj{
				"text":      fmt.Sprintf("结论 %d：与 %s 相关的内容值得重点复习。", i+1, pick(i * n / 3)[0]),
				"point_ids": pick(i * n / 3),
			})
		}
	}
	return obj{
		"title":         p.title + " 笔记",
		"overview":      obj{"text": "这份笔记按时间顺序整理了《" + p.title + "》的主要内容，并标注了每个要点在视频中的出处。", "point_ids": overviewIDs},
		"key_takeaways": takeaways,
	}
}

func (p *prompt) finalAnswer() obj {
	out := p.frame(p.secPts)
	merges := []obj{}
	// Merge the first two sections when they belong to the same group, to
	// exercise the merge path with a plausible answer.
	if len(p.sections) >= 2 {
		a, _, _ := strings.Cut(p.sections[0], ".")
		b, _, _ := strings.Cut(p.sections[1], ".")
		if a == b {
			merges = append(merges, obj{"heading": "开篇与背景", "summary": "合并了开头相邻的两个章节。", "section_ids": p.sections[:2]})
		}
	}
	out["merges"] = merges
	return out
}
