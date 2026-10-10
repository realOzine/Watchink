package asr

import "strings"

// collapseMinRun is the run length from which identical consecutive segments
// are treated as a repetition hallucination (§8.2 step 3).
const collapseMinRun = 3

// Segment is a cleaned transcript segment on the video timeline.
type Segment struct {
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
	Text    string `json:"text"`
}

// Meta records what post-processing did; it is stored with the transcript.
type Meta struct {
	// TimelineOffsetMs was added to every segment time (§8.1).
	TimelineOffsetMs int64 `json:"timeline_offset_ms"`
	// RawSegments is the number of segments the engine produced.
	RawSegments int `json:"raw_segments"`
	// DroppedEmpty counts segments dropped because their text was blank.
	DroppedEmpty int `json:"dropped_empty"`
	// CollapsedRuns counts runs of >= 3 identical consecutive segments that
	// were folded into one; CollapsedSegments counts the segments removed.
	CollapsedRuns     int `json:"collapsed_runs"`
	CollapsedSegments int `json:"collapsed_segments"`
	// AdjustedTimes counts segments whose times had to be corrected.
	AdjustedTimes int `json:"adjusted_times"`
}

// PostProcess implements §8.2 step 3 on the engine's raw segments:
//
//  1. trim surrounding whitespace and drop segments with empty text;
//  2. fold every run of three or more consecutive identical segments (the
//     usual repetition hallucination) into one segment that keeps the first
//     occurrence's times, counting the folds in Meta;
//  3. add offsetMs (timeline_offset_ms, §8.1) to all times, clamp at zero;
//  4. make start times non-decreasing and guarantee start_ms < end_ms.
//
// It is pure: the input slice is not modified and segs is never nil.
func PostProcess(raw []RawSegment, offsetMs int64) (segs []Segment, meta Meta) {
	meta = Meta{TimelineOffsetMs: offsetMs, RawSegments: len(raw)}

	cleaned := make([]Segment, 0, len(raw))
	for _, r := range raw {
		text := strings.TrimSpace(r.Text)
		if text == "" {
			meta.DroppedEmpty++
			continue
		}
		cleaned = append(cleaned, Segment{StartMs: r.StartMs, EndMs: r.EndMs, Text: text})
	}

	segs = make([]Segment, 0, len(cleaned))
	for i := 0; i < len(cleaned); {
		j := i + 1
		for j < len(cleaned) && cleaned[j].Text == cleaned[i].Text {
			j++
		}
		if run := j - i; run >= collapseMinRun {
			meta.CollapsedRuns++
			meta.CollapsedSegments += run - 1
			segs = append(segs, cleaned[i])
		} else {
			segs = append(segs, cleaned[i:j]...)
		}
		i = j
	}

	var prevStart int64
	for i := range segs {
		s := &segs[i]
		start, end := s.StartMs+offsetMs, s.EndMs+offsetMs
		// A negative offset must not push times before the video start;
		// that clamp is the offset at work, not a correction of bad data.
		if start < 0 {
			start = 0
		}
		if end < 0 {
			end = 0
		}
		adjusted := false
		if start < prevStart {
			start, adjusted = prevStart, true
		}
		if end <= start {
			end, adjusted = start+1, true
		}
		if adjusted {
			meta.AdjustedTimes++
		}
		s.StartMs, s.EndMs = start, end
		prevStart = start
	}
	return segs, meta
}
