package mcpserver

import (
	"context"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"watchink/internal/app"
)

// Tool names (§13.5, contract).
const (
	ToolImportVideo   = "import_video"
	ToolGetJob        = "get_job"
	ToolCancelJob     = "cancel_job"
	ToolRetryJob      = "retry_job"
	ToolListVideos    = "list_videos"
	ToolGetNote       = "get_note"
	ToolGetTranscript = "get_transcript"
	ToolExportNote    = "export_note"
)

// ImportVideoInput is the input of import_video.
type ImportVideoInput struct {
	Source        string `json:"source"`
	Language      string `json:"language,omitempty"`
	ReprocessFrom string `json:"reprocess_from,omitempty"`
}

// GetJobInput is the input of get_job.
type GetJobInput struct {
	JobID       string `json:"job_id"`
	WaitSeconds int    `json:"wait_seconds,omitempty"`
	AfterRev    int64  `json:"after_rev,omitempty"`
}

// JobIDInput is the input of cancel_job and retry_job.
type JobIDInput struct {
	JobID string `json:"job_id"`
}

// ListVideosInput is the input of list_videos.
type ListVideosInput struct {
	Limit         int    `json:"limit,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
	TitleContains string `json:"title_contains,omitempty"`
}

// GetNoteInput is the input of get_note.
type GetNoteInput struct {
	VideoID  string `json:"video_id"`
	Format   string `json:"format,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	MaxChars int    `json:"max_chars,omitempty"`
}

// GetTranscriptInput is the input of get_transcript.
type GetTranscriptInput struct {
	VideoID    string   `json:"video_id"`
	StartMs    *int64   `json:"start_ms,omitempty"`
	EndMs      *int64   `json:"end_ms,omitempty"`
	SegmentIDs []string `json:"segment_ids,omitempty"`
	MaxChars   int      `json:"max_chars,omitempty"`
}

// ExportNoteInput is the input of export_note.
type ExportNoteInput struct {
	VideoID string `json:"video_id"`
	Format  string `json:"format"`
	Inline  *bool  `json:"inline,omitempty"`
}

// NoteOutput is the output of get_note (§13.5).
type NoteOutput struct {
	NoteID     string         `json:"note_id"`
	Title      string         `json:"title"`
	Format     string         `json:"format"`
	Content    string         `json:"content"`
	TotalChars int            `json:"total_chars"`
	NextOffset *int           `json:"next_offset"`
	Citations  []app.Citation `json:"citations"`
	Coverage   app.Coverage   `json:"coverage"`
}

// TranscriptOutput is the output of get_transcript (§13.5).
type TranscriptOutput struct {
	TranscriptID string        `json:"transcript_id"`
	Language     string        `json:"language"`
	Segments     []app.Segment `json:"segments"`
	Truncated    bool          `json:"truncated"`
	NextStartMs  *int64        `json:"next_start_ms"`
}

// ExportOutput is the output of export_note (§13.5).
type ExportOutput struct {
	Path     string  `json:"path"`
	Filename string  `json:"filename"`
	Bytes    int64   `json:"bytes"`
	SHA256   string  `json:"sha256"`
	Content  *string `json:"content,omitempty"`
}

func boolPtr(b bool) *bool { return &b }

// addTool registers a typed tool. Business errors become isError results
// carrying the §7.7 error object; successful outputs are returned by the SDK
// as structuredContent plus the same JSON as a text block.
func addTool[In any](s *mcp.Server, t *mcp.Tool, fn func(ctx context.Context, in In) (any, error)) {
	mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		out, err := fn(ctx, in)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return nil, out, nil
	})
}

func registerTools(s *mcp.Server, b *bridge) {
	addTool(s, &mcp.Tool{
		Name:  ToolImportVideo,
		Title: "Import a video",
		Description: "Import a lecture video from an absolute local file path or an http(s) URL (YouTube / Bilibili) and start processing it into a transcript and a cited note. " +
			"Returns immediately with video_id and job_id; poll get_job for progress. " +
			"Importing the same video again does not create a duplicate: it returns the existing job, or status \"already_processed\" with note_id when the note already exists.",
		InputSchema:  importInput,
		OutputSchema: importSchema,
		// Idempotent thanks to dedup (§7.5); it reaches out to video sites.
		Annotations: &mcp.ToolAnnotations{Title: "Import a video", IdempotentHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(true)},
	}, func(ctx context.Context, in ImportVideoInput) (any, error) {
		src, err := importSource(in.Source)
		if err != nil {
			return nil, err
		}
		var out *app.ImportResponse
		err = b.do(ctx, func(r app.Remote) (err error) {
			out, err = r.Import(ctx, app.ImportRequest{Source: src, Language: in.Language, ReprocessFrom: in.ReprocessFrom, RequestedBy: requester})
			return err
		})
		if err != nil {
			return nil, err
		}
		if out.Warnings == nil {
			out.Warnings = []string{}
		}
		return out, nil
	})

	addTool(s, &mcp.Tool{
		Name:  ToolGetJob,
		Title: "Get job status",
		Description: "Get the status, per-stage progress and error of a job. " +
			"To wait efficiently, pass wait_seconds (max 30) together with after_rev = the rev you last saw: the call returns as soon as the job changes. " +
			"Repeat until status is succeeded, failed or cancelled; result.note_id is set once the note exists.",
		InputSchema:  getJobInput,
		OutputSchema: jobSchema,
		Annotations:  &mcp.ToolAnnotations{Title: "Get job status", ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, func(ctx context.Context, in GetJobInput) (any, error) {
		return jobCall(ctx, b, func(r app.Remote) (*app.Job, error) {
			return r.GetJob(ctx, app.GetJobRequest{JobID: in.JobID, WaitSeconds: in.WaitSeconds, AfterRev: in.AfterRev})
		})
	})

	addTool(s, &mcp.Tool{
		Name:         ToolCancelJob,
		Title:        "Cancel a job",
		Description:  "Cancel a queued or running job. Already produced transcripts and notes are kept; the job can be resumed later with retry_job. Cancelling a job that is already cancelling or cancelled has no further effect.",
		InputSchema:  jobIDInput,
		OutputSchema: jobSchema,
		Annotations:  &mcp.ToolAnnotations{Title: "Cancel a job", IdempotentHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)},
	}, func(ctx context.Context, in JobIDInput) (any, error) {
		return jobCall(ctx, b, func(r app.Remote) (*app.Job, error) { return r.CancelJob(ctx, in.JobID) })
	})

	addTool(s, &mcp.Tool{
		Name:         ToolRetryJob,
		Title:        "Retry a job",
		Description:  "Re-queue a failed, cancelled or interrupted job. Finished stages are reused, so only the remaining work is redone. Check the job's error first: if user_action_required is true the user has to fix the cause before a retry can succeed.",
		InputSchema:  jobIDInput,
		OutputSchema: jobSchema,
		Annotations:  &mcp.ToolAnnotations{Title: "Retry a job", DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(true)},
	}, func(ctx context.Context, in JobIDInput) (any, error) {
		return jobCall(ctx, b, func(r app.Remote) (*app.Job, error) { return r.RetryJob(ctx, in.JobID) })
	})

	addTool(s, &mcp.Tool{
		Name:         ToolListVideos,
		Title:        "List videos",
		Description:  "List the videos in the library, newest first, optionally filtered by title. Use it to find the video_id of a video imported earlier. Paged with limit and cursor.",
		InputSchema:  listVideosInput,
		OutputSchema: videoListSchema,
		Annotations:  &mcp.ToolAnnotations{Title: "List videos", ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, func(ctx context.Context, in ListVideosInput) (any, error) {
		var out *app.VideoList
		err := b.do(ctx, func(r app.Remote) (err error) {
			out, err = r.ListVideos(ctx, app.ListVideosRequest{Limit: in.Limit, Cursor: in.Cursor, TitleContains: in.TitleContains})
			return err
		})
		if err != nil {
			return nil, err
		}
		if out.Videos == nil {
			out.Videos = []app.Video{}
		}
		return out, nil
	})

	addTool(s, &mcp.Tool{
		Name:  ToolGetNote,
		Title: "Read a note",
		Description: "Read the current note of a video. content is Markdown whose statements link to transcript segments as wk://seg/<segment_id>; citations resolves every link to start_ms / end_ms. " +
			"Long notes are paged by characters: when next_offset is not null, call again with offset = next_offset. Fails with NOT_FOUND until the video's job has succeeded.",
		InputSchema:  getNoteInput,
		OutputSchema: noteSchema,
		Annotations:  &mcp.ToolAnnotations{Title: "Read a note", ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, func(ctx context.Context, in GetNoteInput) (any, error) {
		var n *app.Note
		err := b.do(ctx, func(r app.Remote) (err error) {
			n, err = r.GetNote(ctx, app.GetNoteRequest{VideoID: in.VideoID, Format: in.Format, Offset: in.Offset, MaxChars: in.MaxChars})
			return err
		})
		if err != nil {
			return nil, err
		}
		out := NoteOutput{
			NoteID: n.NoteID, Title: n.Title, Format: n.Format, Content: n.Content, TotalChars: n.TotalChars,
			NextOffset: n.NextOffset, Citations: n.Citations, Coverage: n.Coverage,
		}
		if out.Citations == nil {
			out.Citations = []app.Citation{}
		}
		return out, nil
	})

	addTool(s, &mcp.Tool{
		Name:  ToolGetTranscript,
		Title: "Read transcript segments",
		Description: "Read transcript segments of a video, either by segment_ids (to verify the citations of a note) or by a start_ms / end_ms time range; with neither, reading starts at the beginning. " +
			"All times are milliseconds from the start of the original video. When truncated is true, continue with start_ms = next_start_ms.",
		InputSchema:  getTranscriptInput,
		OutputSchema: transcriptSchema,
		Annotations:  &mcp.ToolAnnotations{Title: "Read transcript segments", ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, func(ctx context.Context, in GetTranscriptInput) (any, error) {
		if len(in.SegmentIDs) > 0 && (in.StartMs != nil || in.EndMs != nil) {
			return nil, app.E(app.CodeInvalidArgument, "segment_ids 不能与 start_ms / end_ms 同时使用")
		}
		if in.StartMs != nil && in.EndMs != nil && *in.EndMs < *in.StartMs {
			return nil, app.E(app.CodeInvalidArgument, "end_ms 不能小于 start_ms")
		}
		var t *app.Transcript
		err := b.do(ctx, func(r app.Remote) (err error) {
			t, err = r.GetTranscript(ctx, app.GetTranscriptRequest{
				VideoID: in.VideoID, StartMs: in.StartMs, EndMs: in.EndMs, SegmentIDs: in.SegmentIDs,
				Format: app.FormatJSON, MaxChars: in.MaxChars,
			})
			return err
		})
		if err != nil {
			return nil, err
		}
		out := TranscriptOutput{TranscriptID: t.TranscriptID, Language: t.Language, Segments: t.Segments, Truncated: t.Truncated, NextStartMs: t.NextStartMs}
		if out.Segments == nil {
			out.Segments = []app.Segment{}
		}
		return out, nil
	})

	addTool(s, &mcp.Tool{
		Name:  ToolExportNote,
		Title: "Export a note or transcript",
		Description: "Write the note (md, md-raw, json) or the transcript (srt, vtt, txt) of a video to a file under the application's data directory (<data dir>/exports/) and return its path, size and SHA-256. " +
			"The destination cannot be chosen; copy the file elsewhere yourself if needed. Small exports are also returned inline as content unless inline is false.",
		InputSchema:  exportInput,
		OutputSchema: exportSchema,
		// Writes only inside the data dir and never overwrites user files.
		Annotations: &mcp.ToolAnnotations{Title: "Export a note or transcript", DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)},
	}, func(ctx context.Context, in ExportNoteInput) (any, error) {
		var out ExportOutput
		err := b.do(ctx, func(r app.Remote) error {
			exp, err := r.CreateExport(ctx, app.ExportRequest{VideoID: in.VideoID, Format: in.Format})
			if err != nil {
				return err
			}
			out = ExportOutput{Path: exp.Path, Filename: exp.Filename, Bytes: exp.Bytes, SHA256: exp.SHA256}
			// A character is at most 4 bytes, so anything larger cannot be
			// under the limit and is not even read.
			if (in.Inline != nil && !*in.Inline) || exp.Bytes > maxInlineBytes {
				return nil
			}
			content, ok, err := readInline(ctx, r, exp.ExportID)
			if err != nil {
				return err
			}
			if ok {
				out.Content = &content
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	})
}

// maxInlineBytes is the largest file that can still hold fewer than
// DefaultMaxChars characters.
const maxInlineBytes = 4 * app.DefaultMaxChars

// readInline reads an export through OpenExport and reports whether it is
// valid text under the default max_chars (§13.1).
func readInline(ctx context.Context, r app.Remote, exportID string) (string, bool, error) {
	body, _, err := r.OpenExport(ctx, exportID)
	if err != nil {
		return "", false, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxInlineBytes+1))
	if err != nil {
		return "", false, app.E(app.CodeInternal, "读取导出内容失败：%v", err).WithCause(err)
	}
	if len(data) > maxInlineBytes || !utf8.Valid(data) || utf8.RuneCount(data) >= app.DefaultMaxChars {
		return "", false, nil
	}
	return string(data), true, nil
}

// jobCall runs a job-returning call and normalizes the result for the
// output schema.
func jobCall(ctx context.Context, b *bridge, fn func(r app.Remote) (*app.Job, error)) (any, error) {
	var job *app.Job
	err := b.do(ctx, func(r app.Remote) (err error) {
		job, err = fn(r)
		return err
	})
	if err != nil {
		return nil, err
	}
	if job.Stages == nil {
		job.Stages = []app.JobStage{}
	}
	return job, nil
}

// importSource turns the `source` argument into an import source: an
// http(s) URL, or an absolute local path. Unlike the CLI there is no
// meaningful working directory, so relative paths are rejected (§13.5).
func importSource(source string) (app.ImportSource, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return app.ImportSource{}, app.E(app.CodeInvalidArgument, "source 不能为空")
	}
	if u, err := url.Parse(source); err == nil && u.Host != "" {
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			return app.ImportSource{Type: app.SourceURL, URL: source}, nil
		}
	}
	if filepath.IsAbs(source) {
		return app.ImportSource{Type: app.SourceFile, Path: filepath.Clean(source)}, nil
	}
	if strings.Contains(source, "://") {
		return app.ImportSource{}, app.E(app.CodeUnsupportedSource, "不支持的链接：只接受 http / https 链接或本地绝对路径")
	}
	return app.ImportSource{}, app.E(app.CodeInvalidArgument, "source 必须是绝对路径或 http(s) 链接").
		WithHint("把相对路径改成绝对路径后重试")
}
