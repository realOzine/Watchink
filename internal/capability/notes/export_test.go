package notes

import (
	"strings"
	"testing"
	"unicode/utf8"

	"watchink/internal/app"
)

func TestExportMarkdown(t *testing.T) {
	in := fixture(30000, app.VideoSource{Type: app.SourceFile})
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	canonical := out.Markdown
	link := "[01:00](wk://seg/seg_01JA2M8Q0000000000000000AB_00003)"
	cases := []struct {
		name   string
		source app.VideoSource
		want   string // replacement of link
	}{
		{"youtube", app.VideoSource{Type: app.SourceYouTube, PlatformID: "dQw4w9WgXcQ"}, "[01:00](https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=60s)"},
		{"youtube with dash and underscore", app.VideoSource{Type: app.SourceYouTube, PlatformID: "a-b_c-d_e-f"}, "[01:00](https://www.youtube.com/watch?v=a-b_c-d_e-f&t=60s)"},
		{"bilibili with page", app.VideoSource{Type: app.SourceBilibili, PlatformID: "BV1xx411c7mD:p3"}, "[01:00](https://www.bilibili.com/video/BV1xx411c7mD?p=3&t=60)"},
		{"bilibili bare BV", app.VideoSource{Type: app.SourceBilibili, PlatformID: "BV1xx411c7mD"}, "[01:00](https://www.bilibili.com/video/BV1xx411c7mD?p=1&t=60)"},
		{"local file", app.VideoSource{Type: app.SourceFile, URI: "/a/b.mp4"}, "[01:00]"},
		{"upload", app.VideoSource{Type: app.SourceUpload}, "[01:00]"},
		{"youtube with a bad id falls back to text", app.VideoSource{Type: app.SourceYouTube, PlatformID: "x)&evil=1"}, "[01:00]"},
		{"bilibili with a bad id falls back to text", app.VideoSource{Type: app.SourceBilibili, PlatformID: "av170001"}, "[01:00]"},
		{"unknown source", app.VideoSource{Type: "vimeo", PlatformID: "1"}, "[01:00]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExportMarkdown(canonical, tc.source, out.Citations)
			if strings.Contains(got, "wk://seg/") {
				t.Fatalf("wk:// links left in the export")
			}
			if want := strings.ReplaceAll(canonical, link, tc.want); !strings.Contains(got, tc.want+"\n") || strings.Count(got, tc.want) != strings.Count(want, tc.want) {
				t.Fatalf("link not rewritten to %q:\n%s", tc.want, got)
			}
			// Everything except the links is untouched.
			if a, b := len(strings.Split(got, "\n")), len(strings.Split(canonical, "\n")); a != b {
				t.Fatalf("line count changed: %d vs %d", a, b)
			}
			if !strings.HasPrefix(got, canonical[:strings.Index(canonical, "## 概览")]) {
				t.Fatalf("front matter or header changed")
			}
			if n := strings.Count(got, "[") - strings.Count(got, `\[`); n != strings.Count(canonical, "[")-strings.Count(canonical, `\[`) {
				t.Fatalf("number of time labels changed")
			}
		})
	}
	// Seconds come from the citations table (segment start), every link is
	// rewritten, and escaped look-alikes in model text are left alone.
	yt := ExportMarkdown(canonical, app.VideoSource{Type: app.SourceYouTube, PlatformID: "dQw4w9WgXcQ"}, out.Citations)
	if n := strings.Count(yt, "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t="); n != len(out.Citations) {
		t.Fatalf("rewrote %d links, want %d", n, len(out.Citations))
	}
	mustContain(t, yt, "[05:30](https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=330s)", `\[00:01\](wk:\/\/seg/seg\_FAKE)`)
	// Without a citations table the label is used.
	if got := ExportMarkdown(link, app.VideoSource{Type: app.SourceYouTube, PlatformID: "dQw4w9WgXcQ"}, nil); got != "[01:00](https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=60s)" {
		t.Fatalf("got %q", got)
	}
	if got := ExportMarkdown("[1:02:03](wk://seg/seg_x_00001)", app.VideoSource{Type: app.SourceBilibili, PlatformID: "BV1xx411c7mD:p2"}, nil); got != "[1:02:03](https://www.bilibili.com/video/BV1xx411c7mD?p=2&t=3723)" {
		t.Fatalf("got %q", got)
	}
	if got := ExportMarkdown("no links here", app.VideoSource{Type: app.SourceYouTube, PlatformID: "dQw4w9WgXcQ"}, nil); got != "no links here" {
		t.Fatalf("got %q", got)
	}
}

func TestSubtitleFormats(t *testing.T) {
	segs := []app.Segment{
		{Idx: 1, StartMs: 0, EndMs: 2500, Text: "大家好，欢迎来到这门课。"},
		{Idx: 2, StartMs: 2500, EndMs: 2500, Text: "zero length"},
		{Idx: 3, StartMs: 3000, EndMs: 4000, Text: "   "},
		{Idx: 4, StartMs: 61001, EndMs: 3723045, Text: "a < b && c --> d\nsecond line <b>bold</b>"},
		{Idx: 5, StartMs: 360000000, EndMs: 360001000, Text: "一百小时以后"},
	}
	wantSRT := "1\n00:00:00,000 --> 00:00:02,500\n大家好，欢迎来到这门课。\n\n" +
		"2\n00:00:02,500 --> 00:00:02,501\nzero length\n\n" +
		"3\n00:01:01,001 --> 01:02:03,045\na < b && c --> d second line <b>bold</b>\n\n" +
		"4\n100:00:00,000 --> 100:00:01,000\n一百小时以后\n\n"
	if got := SRT(segs); got != wantSRT {
		t.Errorf("SRT:\n%q\nwant\n%q", got, wantSRT)
	}
	wantVTT := "WEBVTT\n\n00:00:00.000 --> 00:00:02.500\n大家好，欢迎来到这门课。\n\n" +
		"00:00:02.500 --> 00:00:02.501\nzero length\n\n" +
		"00:01:01.001 --> 01:02:03.045\na &lt; b &amp;&amp; c --&gt; d second line &lt;b&gt;bold&lt;/b&gt;\n\n" +
		"100:00:00.000 --> 100:00:01.000\n一百小时以后\n\n"
	if got := VTT(segs); got != wantVTT {
		t.Errorf("VTT:\n%q\nwant\n%q", got, wantVTT)
	}
	wantTXT := "大家好，欢迎来到这门课。\nzero length\na < b && c --> d second line <b>bold</b>\n一百小时以后\n"
	if got := TXT(segs); got != wantTXT {
		t.Errorf("TXT:\n%q\nwant\n%q", got, wantTXT)
	}
	if SRT(nil) != "" || VTT(nil) != "WEBVTT\n\n" || TXT(nil) != "" {
		t.Error("empty transcript formats wrong")
	}
}

func TestExportFilename(t *testing.T) {
	const vid = "vid_01JA2M8Q0000000000000000CD"
	cases := []struct {
		title, videoID, format, want string
	}{
		{"机器学习导论 第 3 讲", vid, "md", "机器学习导论 第 3 讲-0000CD.md"},
		{"Intro to ML", vid, "md-raw", "Intro to ML-0000CD.md"},
		{"Intro to ML", vid, "json", "Intro to ML-0000CD.json"},
		{"a/b\\c:d*e?f\"g<h>i|j", vid, "srt", "a b c d e f g h i j-0000CD.srt"},
		{"  ..hidden.. \t\n", vid, "vtt", "hidden-0000CD.vtt"},
		{"", vid, "txt", "note-0000CD.txt"},
		{"///", vid, "txt", "note-0000CD.txt"},
		{"CON", vid, "md", "_CON-0000CD.md"},
		{"com1.backup", vid, "md", "_com1.backup-0000CD.md"},
		{"control\x00\x1fchars\u200b\u202e", vid, "md", "control chars-0000CD.md"},
		{"title", "ab", "md", "title-ab.md"},
		{"title", "", "md", "title.md"},
		{"title", "vid_../../x", "md", "title-vidx.md"},
		{"title", vid, "exe", "title-0000CD.txt"},
		{"..", vid, "md", "note-0000CD.md"},
	}
	for _, tc := range cases {
		if got := ExportFilename(tc.title, tc.videoID, tc.format); got != tc.want {
			t.Errorf("ExportFilename(%q, %q, %q) = %q, want %q", tc.title, tc.videoID, tc.format, got, tc.want)
		}
	}
	long := ExportFilename(strings.Repeat("很长的标题", 200), vid, "md")
	if utf8.RuneCountInString(long) > 80+10 || len(long) > 200 || !strings.HasSuffix(long, "-0000CD.md") || !utf8.ValidString(long) {
		t.Errorf("long filename not bounded: %d runes, %d bytes", utf8.RuneCountInString(long), len(long))
	}
	for _, r := range ExportFilename("x<>:\"/\\|?*\x01y", vid, "md") {
		if strings.ContainsRune("<>:\"/\\|?*", r) || r < 0x20 {
			t.Errorf("unsafe rune %q in filename", r)
		}
	}
}

func TestPaginate(t *testing.T) {
	const s = "ab你好cd世界ef" // 10 code points
	ptr := func(n int) *int { return &n }
	cases := []struct {
		offset, max int
		page        string
		next        *int
	}{
		{0, 4, "ab你好", ptr(4)},
		{4, 4, "cd世界", ptr(8)},
		{8, 4, "ef", nil},
		{8, 2, "ef", nil},
		{3, 1, "好", ptr(4)},
		{0, 10, s, nil},
		{0, 100, s, nil},
		{0, 0, s, nil},           // default limit
		{2, -1, "你好cd世界ef", nil}, // no limit
		{10, 5, "", nil},
		{99, 5, "", nil},
		{-3, 2, "ab", ptr(2)},
	}
	for _, tc := range cases {
		page, total, next := Paginate(s, tc.offset, tc.max)
		if page != tc.page || total != 10 || (next == nil) != (tc.next == nil) || (next != nil && *next != *tc.next) {
			t.Errorf("Paginate(%d, %d) = %q, %d, %v; want %q, 10, %v", tc.offset, tc.max, page, total, next, tc.page, tc.next)
		}
	}
	if page, total, next := Paginate("", 0, 5); page != "" || total != 0 || next != nil {
		t.Error("empty content")
	}
	// Pages concatenate back to the content; the default page is 20000 code points.
	big := strings.Repeat("笔记a", 15000) // 45000 code points
	var b strings.Builder
	offset, pages := 0, 0
	for {
		page, total, next := Paginate(big, offset, 0)
		if total != 45000 || utf8.RuneCountInString(page) > app.DefaultMaxChars {
			t.Fatalf("total=%d page=%d", total, utf8.RuneCountInString(page))
		}
		b.WriteString(page)
		pages++
		if next == nil {
			break
		}
		offset = *next
	}
	if b.String() != big || pages != 3 {
		t.Fatalf("pages=%d, reassembly ok=%v", pages, b.String() == big)
	}
}
