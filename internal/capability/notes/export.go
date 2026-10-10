package notes

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"watchink/internal/app"
)

var (
	youtubeIDRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	bilibiliIDRe = regexp.MustCompile(`^(BV[0-9A-Za-z]{6,20})(?::p([0-9]{1,5}))?$`)
)

// labelSeconds parses a rendered time label (MM:SS or H:MM:SS).
func labelSeconds(label string) int64 {
	var sec int64
	for _, part := range strings.Split(label, ":") {
		n, _ := strconv.ParseInt(part, 10, 64)
		sec = sec*60 + n
	}
	return sec
}

// ExportMarkdown turns the canonical Markdown into portable Markdown:
// every wk://seg/ link becomes a platform time link for YouTube and Bilibili
// sources, or the plain text "[MM:SS]" for local files and uploads (and for a
// platform id that does not look valid). The visible time label is kept.
func ExportMarkdown(canonicalMD string, source app.VideoSource, citations []app.Citation) string {
	startBySeg := make(map[string]int64, len(citations))
	for _, c := range citations {
		startBySeg[c.SegmentID] = c.StartMs
	}
	var target func(sec int64) string
	switch source.Type {
	case app.SourceYouTube:
		if id := strings.TrimSpace(source.PlatformID); youtubeIDRe.MatchString(id) {
			target = func(sec int64) string {
				return fmt.Sprintf("https://www.youtube.com/watch?v=%s&t=%ds", id, sec)
			}
		}
	case app.SourceBilibili:
		if m := bilibiliIDRe.FindStringSubmatch(strings.TrimSpace(source.PlatformID)); m != nil {
			page := m[2]
			if page == "" {
				page = "1"
			}
			target = func(sec int64) string {
				return fmt.Sprintf("https://www.bilibili.com/video/%s?p=%s&t=%d", m[1], page, sec)
			}
		}
	}
	links := parseCitationLinks(canonicalMD)
	if len(links) == 0 {
		return canonicalMD
	}
	var b strings.Builder
	b.Grow(len(canonicalMD) + len(links)*32)
	last := 0
	for _, l := range links {
		b.WriteString(canonicalMD[last:l.start])
		last = l.end
		if target == nil {
			b.WriteString("[" + l.label + "]")
			continue
		}
		sec := labelSeconds(l.label)
		if ms, ok := startBySeg[l.segmentID]; ok {
			sec = ms / 1000
		}
		b.WriteString("[" + l.label + "](" + target(sec) + ")")
	}
	b.WriteString(canonicalMD[last:])
	return b.String()
}

// writeCueTime appends HH:MM:SS<sep>mmm. Subtitle export writes two of
// these per segment, so it avoids fmt (see BenchmarkText in internal/bench).
func writeCueTime(b *strings.Builder, ms int64, sep byte) {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3600000
	if h < 10 {
		b.WriteByte('0')
	}
	b.WriteString(strconv.FormatInt(h, 10))
	m, sec, milli := ms/60000%60, ms/1000%60, ms%1000
	b.Write([]byte{':', byte('0' + m/10), byte('0' + m%10), ':', byte('0' + sec/10), byte('0' + sec%10), sep,
		byte('0' + milli/100), byte('0' + milli/10%10), byte('0' + milli%10)})
}

// writeCue appends "start --> end\ntext\n\n".
func writeCue(b *strings.Builder, s app.Segment, sep byte, text string) {
	start, end := cueBounds(s)
	writeCueTime(b, start, sep)
	b.WriteString(" --> ")
	writeCueTime(b, end, sep)
	b.WriteByte('\n')
	b.WriteString(text)
	b.WriteString("\n\n")
}

// textBytes sizes the output buffer of the subtitle writers.
func textBytes(segs []app.Segment) int {
	n := 0
	for _, s := range segs {
		n += len(s.Text)
	}
	return n
}

// cueBounds returns the cue times of a segment, with a strictly positive
// length (players reject zero-length cues).
func cueBounds(s app.Segment) (start, end int64) {
	start, end = max(s.StartMs, 0), s.EndMs
	if end <= start {
		end = start + 1
	}
	return start, end
}

// SRT renders segments as SubRip. Segments without text are skipped and cues
// are numbered consecutively from 1.
func SRT(segs []app.Segment) string {
	var b strings.Builder
	b.Grow(textBytes(segs) + len(segs)*40)
	n := 0
	for _, s := range segs {
		text := oneLine(s.Text)
		if text == "" {
			continue
		}
		n++
		b.WriteString(strconv.Itoa(n))
		b.WriteByte('\n')
		writeCue(&b, s, ',', text)
	}
	return b.String()
}

var vttEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// VTT renders segments as WebVTT. Cue text is escaped so it can contain
// neither tags nor the "-->" cue separator.
func VTT(segs []app.Segment) string {
	var b strings.Builder
	b.Grow(textBytes(segs) + len(segs)*34 + 8)
	b.WriteString("WEBVTT\n\n")
	for _, s := range segs {
		text := oneLine(s.Text)
		if text == "" {
			continue
		}
		writeCue(&b, s, '.', vttEscaper.Replace(text))
	}
	return b.String()
}

// TXT renders the transcript as plain text, one segment per line.
func TXT(segs []app.Segment) string {
	var b strings.Builder
	for _, s := range segs {
		if text := oneLine(s.Text); text != "" {
			b.WriteString(text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

const (
	maxFilenameTitleRunes = 80
	maxFilenameTitleBytes = 150
)

var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizeFilenamePart keeps letters, digits and harmless punctuation of any
// script and removes everything that is unsafe on Windows, macOS or Linux.
func sanitizeFilenamePart(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case strings.ContainsRune(`<>:"/\|?*`, r), unicode.IsControl(r), unicode.IsSpace(r), r == utf8.RuneError:
			return ' '
		case !unicode.IsPrint(r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > maxFilenameTitleRunes {
		r = r[:maxFilenameTitleRunes]
	}
	for len(string(r)) > maxFilenameTitleBytes {
		r = r[:len(r)-1]
	}
	// Windows silently strips trailing dots and spaces; leading dots would
	// hide the file on Unix.
	s = strings.Trim(string(r), " .")
	if base, _, _ := strings.Cut(s, "."); windowsReserved[strings.ToUpper(strings.TrimSpace(base))] {
		s = "_" + s
	}
	return s
}

// ExportExt maps an export format to its file extension (md-raw → md).
func ExportExt(format string) string {
	switch format {
	case app.FormatMD, app.FormatMDRaw, app.FormatMarkdown:
		return "md"
	case app.FormatJSON, app.FormatSRT, app.FormatVTT, app.FormatTXT:
		return format
	}
	return "txt"
}

// ExportFilename builds "<sanitised title>-<last 6 of video_id>.<ext>".
// The result is a single path element that is valid on every supported OS and
// bounded in length.
func ExportFilename(title, videoID, format string) string {
	name := sanitizeFilenamePart(title)
	if name == "" {
		name = "note"
	}
	id := strings.Map(func(r rune) rune {
		if r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return r
		}
		return -1
	}, videoID)
	if len(id) > 6 {
		id = id[len(id)-6:]
	}
	if id != "" {
		name += "-" + id
	}
	return name + "." + ExportExt(format)
}

// Paginate returns the page of content that starts at offset and holds at
// most maxChars Unicode code points, the total number of code points, and
// the next offset (nil when the page reaches the end) — the long-text
// convention. maxChars == 0 means the default (20000); a negative
// maxChars means no limit.
func Paginate(content string, offset, maxChars int) (page string, total int, next *int) {
	total = utf8.RuneCountInString(content)
	if maxChars == 0 {
		maxChars = app.DefaultMaxChars
	}
	offset = min(max(offset, 0), total)
	end := total
	if maxChars > 0 && offset+maxChars < total {
		end = offset + maxChars
	}
	// Walk to the byte positions of the two code point offsets.
	startByte, endByte, i := len(content), len(content), 0
	for pos := range content {
		if i == offset {
			startByte = pos
		}
		if i == end {
			endByte = pos
			break
		}
		i++
	}
	if end < total {
		n := end
		next = &n
	}
	return content[startByte:endByte], total, next
}
