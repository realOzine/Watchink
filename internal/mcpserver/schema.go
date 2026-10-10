package mcpserver

import "watchink/internal/app"

// The schemas are written by hand rather than inferred: inference cannot
// express the contract's constraints (enums, 0–30, nullable fields). A test
// keeps them in sync with the Go types.

type schema = map[string]any

func str(desc string) schema     { return schema{"type": "string", "description": desc} }
func integer(desc string) schema { return schema{"type": "integer", "description": desc} }
func number(desc string) schema  { return schema{"type": "number", "description": desc} }
func boolean(desc string) schema { return schema{"type": "boolean", "description": desc} }

// with returns a copy of s with extra keywords.
func with(s schema, kv ...any) schema {
	out := make(schema, len(s)+len(kv)/2)
	for k, v := range s {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// nullable widens the type of s with null.
func nullable(s schema) schema { return with(s, "type", []any{s["type"], "null"}) }

func enum(desc string, values ...string) schema {
	vs := make([]any, len(values))
	for i, v := range values {
		vs[i] = v
	}
	return schema{"type": "string", "description": desc, "enum": vs}
}

func array(desc string, items schema) schema {
	return schema{"type": "array", "description": desc, "items": items}
}

// object builds an output object schema (extra members are tolerated so
// that additive engine changes never break validation).
func object(desc string, required []string, props schema) schema {
	s := schema{"type": "object", "properties": props}
	if desc != "" {
		s["description"] = desc
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// input builds a tool input schema; unknown arguments are rejected.
func input(required []string, props schema) schema {
	s := object("", required, props)
	s["additionalProperties"] = false
	return s
}

func codeNames() []string {
	codes := app.KnownCodes()
	out := make([]string, len(codes))
	for i, c := range codes {
		out[i] = string(c)
	}
	return out
}

var errorSchema = object("Structured error object.",
	[]string{"code", "message", "retryable", "user_action_required"},
	schema{
		"code":                 enum("Stable machine-readable error code.", codeNames()...),
		"message":              str("Human-readable message (Chinese)."),
		"stage":                str("Pipeline stage the error happened in."),
		"retryable":            boolean("True if retrying may succeed (possibly after the user fixed something)."),
		"user_action_required": boolean("True if the user must change configuration, network or input before a retry can succeed."),
		"hint":                 str("What the user should do (Chinese)."),
		"details":              schema{"type": "object", "description": "Extra machine-readable details."},
	})

var jobStatus = enum("Job status. succeeded, failed and cancelled are final; interrupted jobs resume when the engine restarts.",
	app.JobQueued, app.JobRunning, app.JobCancelling, app.JobSucceeded, app.JobFailed, app.JobCancelled, app.JobInterrupted)

var jobSchema = object("A processing job.",
	[]string{"job_id", "kind", "status", "rev", "stage", "progress", "stages", "error", "result"},
	schema{
		"job_id":   str("Job ID (job_…)."),
		"kind":     enum("Job kind.", app.JobKindProcessVideo, app.JobKindPrepareTools),
		"video_id": nullable(str("Video the job belongs to; null for prepare_tools jobs.")),
		"status":   jobStatus,
		"rev":      integer("Revision counter; increases on every change. Pass it as after_rev to long-poll."),
		"attempt":  integer("1-based attempt number (increases on retry)."),
		"stage":    str("Current stage; empty once the job is final."),
		"progress": number("Overall progress from 0 to 1."),
		"stages": array("Per-stage state, in execution order.", object("", []string{"stage", "status", "progress"}, schema{
			"stage":       str("Stage name."),
			"status":      enum("Stage status.", app.StagePending, app.StageRunning, app.StageSucceeded, app.StageFailed, app.StageCancelled),
			"progress":    number("Stage progress from 0 to 1."),
			"reused":      boolean("True if a cached artifact was reused instead of recomputing."),
			"attempt":     integer("Attempt number of this stage."),
			"error":       errorSchema,
			"started_at":  str("RFC 3339 UTC start time."),
			"finished_at": str("RFC 3339 UTC finish time."),
		})),
		"error": nullable(with(errorSchema, "description", "Set when status is failed; null otherwise.")),
		"result": object("IDs produced so far.", nil, schema{
			"transcript_id": nullable(str("Transcript ID once transcription finished.")),
			"note_id":       nullable(str("Note ID once the note was generated.")),
		}),
		"requested_by": str("Entry point that created the job: desktop, web, cli or mcp."),
		"created_at":   str("RFC 3339 UTC creation time."),
		"started_at":   str("RFC 3339 UTC start time."),
		"finished_at":  str("RFC 3339 UTC finish time."),
		"updated_at":   str("RFC 3339 UTC time of the last change."),
	})

var importSchema = object("Result of an import request.",
	[]string{"video_id", "job_id", "status", "deduplicated", "note_id", "warnings"},
	schema{
		"video_id": str("Video ID (vid_…)."),
		"job_id":   str("Job to poll with get_job."),
		"status": enum("Status of the job, or already_processed when the video already has a note (no new job was created). failed / cancelled mean the previous job did not succeed and is not retried automatically: call retry_job.",
			app.JobQueued, app.JobRunning, app.JobCancelling, app.JobSucceeded, app.JobFailed, app.JobCancelled, app.JobInterrupted, app.ImportStatusAlreadyProcessed),
		"deduplicated": boolean("True if an existing video or active job was reused."),
		"note_id":      nullable(str("Current note ID when status is already_processed.")),
		"warnings":     array("Warning codes, e.g. MODEL_NOT_CONFIGURED: the job will fail at note generation until the user configures the online model.", schema{"type": "string"}),
	})

var videoSchema = object("A video in the library.",
	[]string{"video_id", "title", "source", "status"},
	schema{
		"video_id": str("Video ID (vid_…)."),
		"title":    str("Video title."),
		"source": object("Where the video came from.", []string{"type"}, schema{
			"type":        enum("Source type.", app.SourceFile, app.SourceUpload, app.SourceYouTube, app.SourceBilibili),
			"uri":         str("Original path or URL."),
			"platform_id": str("Platform video ID for online sources."),
		}),
		"duration_ms":    nullable(integer("Duration in milliseconds; null until probed.")),
		"language":       nullable(str("Detected or requested language; null until transcribed.")),
		"status":         enum("Video status. ready means transcript and note exist.", app.VideoProcessing, app.VideoReady, app.VideoFailed, app.VideoCancelled),
		"latest_job_id":  nullable(str("Most recent job of the video.")),
		"transcript_id":  nullable(str("Current transcript ID.")),
		"note_id":        nullable(str("Current note ID.")),
		"playback_ready": boolean("True once a browser-playable file exists."),
		"web_url":        str("URL of the video page in the local Web UI."),
		"created_at":     str("RFC 3339 UTC creation time."),
		"updated_at":     str("RFC 3339 UTC time of the last change."),
	})

var videoListSchema = object("One page of videos.", []string{"videos", "next_cursor"}, schema{
	"videos":      array("Videos, newest first.", videoSchema),
	"next_cursor": nullable(str("Pass as cursor to get the next page; null when there are no more.")),
})

var segmentSchema = object("One transcript segment.", []string{"segment_id", "idx", "start_ms", "end_ms", "text"}, schema{
	"segment_id": str("Segment ID (seg_…), the target of wk://seg/ citations."),
	"idx":        integer("1-based index inside the transcript."),
	"start_ms":   integer("Start, in milliseconds from the start of the video."),
	"end_ms":     integer("End, in milliseconds from the start of the video."),
	"text":       str("What was said."),
})

func coverageSchema() schema {
	props := schema{"overview_fallback": boolean("True if the overview was built without the model.")}
	for name, desc := range map[string]string{
		"chunks": "Transcript chunks processed.", "chunks_empty": "Chunks that produced no points.",
		"points": "Points kept in the note.", "points_dropped": "Points dropped by validation.",
		"points_unplaced": "Points not placed in any section.", "citations": "Valid citations.",
		"citations_dropped": "Citations dropped as invalid.", "repair_calls": "Model repair calls made.",
		"split_retries": "Chunks split and retried.", "transcript_segments": "Segments in the transcript.",
		"transcript_chars": "Characters in the transcript.",
	} {
		props[name] = integer(desc)
	}
	return object("How much of the transcript the note covers and what validation dropped.", nil, props)
}

var noteSchema = object("A note, possibly one page of it.",
	[]string{"note_id", "title", "format", "content", "total_chars", "next_offset", "citations", "coverage"},
	schema{
		"note_id":     str("Note ID (note_…)."),
		"title":       str("Note title."),
		"format":      enum("Format of content.", app.FormatMarkdown, app.FormatJSON),
		"content":     str("Markdown with wk://seg/<segment_id> citation links, or the structured note as a JSON string. May be one page of the full text."),
		"total_chars": integer("Length of the full content in Unicode code points."),
		"next_offset": nullable(integer("Pass as offset to read the next page; null when this page reaches the end.")),
		"citations": array("Every citation of the note, resolved to times.", object("", []string{"ref_no", "segment_id", "start_ms", "end_ms"}, schema{
			"ref_no":     integer("Reference number shown in the note."),
			"point_id":   str("ID of the note point that cites the segment."),
			"segment_id": str("Cited transcript segment."),
			"start_ms":   integer("Segment start in milliseconds."),
			"end_ms":     integer("Segment end in milliseconds."),
			"text":       str("Text of the cited segment."),
		})),
		"coverage": coverageSchema(),
	})

var transcriptSchema = object("Transcript segments.",
	[]string{"transcript_id", "language", "segments", "truncated", "next_start_ms"},
	schema{
		"transcript_id": str("Transcript ID (tr_…)."),
		"language":      str("Transcript language."),
		"segments":      array("Segments in time order.", segmentSchema),
		"truncated":     boolean("True if max_chars cut the result short."),
		"next_start_ms": nullable(integer("Pass as start_ms to continue reading; null when not truncated.")),
	})

var exportSchema = object("An exported file.", []string{"path", "filename", "bytes", "sha256"}, schema{
	"path":     str("Absolute path of the file, always under <data dir>/exports/."),
	"filename": str("File name."),
	"bytes":    integer("File size in bytes."),
	"sha256":   str("Hex SHA-256 of the file."),
	"content":  str("The file content, present only when inline is not false and the content is shorter than 20000 characters."),
})

const (
	descVideoID = "Video ID (vid_…) as returned by import_video or list_videos."
	descJobID   = "Job ID (job_…) as returned by import_video."
)

var (
	importInput = input([]string{"source"}, schema{
		"source":         with(str("Absolute local file path, or an http(s) URL of a single YouTube or Bilibili video. Relative paths are rejected. Local paths must be inside the user's allowed import roots."), "minLength", 1),
		"language":       enum("Spoken language; auto detects it. Defaults to the user's configured language.", "auto", "zh", "en"),
		"reprocess_from": enum("Force a new job that recomputes from this stage on (earlier stages are reused): acquire re-fetches the media, transcribe redoes speech recognition, note regenerates only the note. Omit for normal imports.", "acquire", "transcribe", "note"),
	})
	getJobInput = input([]string{"job_id"}, schema{
		"job_id":       with(str(descJobID), "minLength", 1),
		"wait_seconds": with(integer("Long-poll: return as soon as the job's rev exceeds after_rev, or after this many seconds. 0 returns immediately."), "minimum", 0, "maximum", app.MaxWaitSeconds, "default", 0),
		"after_rev":    with(integer("The rev of the job you last saw; used with wait_seconds."), "minimum", 0),
	})
	jobIDInput = input([]string{"job_id"}, schema{
		"job_id": with(str(descJobID), "minLength", 1),
	})
	listVideosInput = input(nil, schema{
		"limit":          with(integer("Page size."), "minimum", 1, "maximum", app.MaxLimit, "default", app.DefaultLimit),
		"cursor":         str("next_cursor of the previous page."),
		"title_contains": str("Only return videos whose title contains this text."),
	})
	getNoteInput = input([]string{"video_id"}, schema{
		"video_id":  with(str(descVideoID), "minLength", 1),
		"format":    with(enum("markdown: the note with wk://seg/ citation links; json: the structured note.", app.FormatMarkdown, app.FormatJSON), "default", app.FormatMarkdown),
		"offset":    with(integer("Character offset to start from (use next_offset of the previous page)."), "minimum", 0, "default", 0),
		"max_chars": with(integer("Maximum characters to return."), "minimum", 1, "default", app.DefaultMaxChars),
	})
	getTranscriptInput = input([]string{"video_id"}, schema{
		"video_id":    with(str(descVideoID), "minLength", 1),
		"start_ms":    with(integer("Return segments from this time on (milliseconds). Cannot be combined with segment_ids."), "minimum", 0),
		"end_ms":      with(integer("Return segments up to this time (milliseconds). Cannot be combined with segment_ids."), "minimum", 0),
		"segment_ids": with(array("Return exactly these segments (e.g. the ones cited by a note).", schema{"type": "string"}), "maxItems", app.MaxSegmentIDs),
		"max_chars":   with(integer("Maximum characters of text to return; the result is truncated at a segment boundary."), "minimum", 1, "default", app.DefaultMaxChars),
	})
	exportInput = input([]string{"video_id", "format"}, schema{
		"video_id": with(str(descVideoID), "minLength", 1),
		"format":   enum("md: portable Markdown with timestamps; md-raw: canonical Markdown with wk://seg/ links; json: structured note; srt / vtt / txt: the transcript.", app.FormatMD, app.FormatMDRaw, app.FormatJSON, app.FormatSRT, app.FormatVTT, app.FormatTXT),
		"inline":   with(boolean("Also return the file content when it is small enough. Set false to get only the path."), "default", true),
	})
)
