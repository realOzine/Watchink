package app

// All JSON fields are snake_case; durations relative to a video are *_ms
// integers; instants are RFC 3339 UTC strings (docs/v0.md §13.1, contract).

// Job, stage and video status values (contract).
const (
	JobQueued      = "queued"
	JobRunning     = "running"
	JobCancelling  = "cancelling"
	JobSucceeded   = "succeeded"
	JobFailed      = "failed"
	JobCancelled   = "cancelled"
	JobInterrupted = "interrupted"

	StagePending   = "pending"
	StageRunning   = "running"
	StageSucceeded = "succeeded"
	StageFailed    = "failed"
	StageCancelled = "cancelled"

	VideoProcessing = "processing"
	VideoReady      = "ready"
	VideoFailed     = "failed"
	VideoCancelled  = "cancelled"

	JobKindProcessVideo = "process_video"
	JobKindPrepareTools = "prepare_tools"

	SourceFile     = "file"
	SourceUpload   = "upload"
	SourceURL      = "url" // import request only; stored as youtube / bilibili
	SourceYouTube  = "youtube"
	SourceBilibili = "bilibili"

	// ImportStatusAlreadyProcessed is returned by Import when the video
	// already has a successful job.
	ImportStatusAlreadyProcessed = "already_processed"

	WarnModelNotConfigured = "MODEL_NOT_CONFIGURED"
)

// Stage names of a process_video job, in DAG order.
const (
	StageEnsureTools  = "ensure_tools"
	StageAcquire      = "acquire"
	StageProbe        = "probe"
	StageExtractAudio = "extract_audio"
	StageTranscribe   = "transcribe"
	StagePlayback     = "playback"
	StageNoteMap      = "note_map"
	StageNoteReduce   = "note_reduce"
	StageNoteRender   = "note_render"

	// StagePrepare is the single stage of a prepare_tools job.
	StagePrepare = "prepare"
)

// Export and read formats.
const (
	FormatMarkdown    = "markdown" // note read format
	FormatJSON        = "json"
	FormatMD          = "md" // export: portable Markdown
	FormatMDRaw       = "md-raw"
	FormatSRT         = "srt"
	FormatVTT         = "vtt"
	FormatTXT         = "txt"
	DefaultLimit      = 20
	MaxLimit          = 100
	DefaultMaxChars   = 20000
	MaxWaitSeconds    = 30
	MaxSegmentIDs     = 50
	APIVersion        = 1
	APIPrefix         = "/api/v1"
	DefaultEnginePort = 47823
)

// EngineInfo is the body of GET /api/v1/engine.
type EngineInfo struct {
	Version   string `json:"version"`
	BuildID   string `json:"build_id"`
	API       int    `json:"api"`
	PID       int    `json:"pid"`
	DataDir   string `json:"data_dir"`
	Mode      string `json:"mode"` // background / foreground
	BaseURL   string `json:"base_url"`
	StartedAt string `json:"started_at"`
	// ActiveJobs counts queued + running + cancelling jobs; clients use it to
	// decide whether a mismatched engine may be shut down.
	ActiveJobs int `json:"active_jobs"`
	// ToolsReady is true when every tool and model needed for a local-file
	// import is installed.
	ToolsReady bool `json:"tools_ready"`
	// LLMConfigured is true when base_url, model and an API key are present.
	LLMConfigured bool `json:"llm_configured"`
}

// ImportSource is one of: {type:file,path} / {type:url,url} / {type:upload,upload_id}.
type ImportSource struct {
	Type     string `json:"type"`
	Path     string `json:"path,omitempty"`
	URL      string `json:"url,omitempty"`
	UploadID string `json:"upload_id,omitempty"`
}

// ImportRequest is the body of POST /api/v1/imports.
type ImportRequest struct {
	Source        ImportSource `json:"source"`
	Language      string       `json:"language,omitempty"`       // auto / zh / en
	ReprocessFrom string       `json:"reprocess_from,omitempty"` // acquire / transcribe / note
	RequestedBy   string       `json:"requested_by,omitempty"`   // desktop / web / cli / mcp
}

// ImportResponse is the 202 body of POST /api/v1/imports.
type ImportResponse struct {
	VideoID      string   `json:"video_id"`
	JobID        string   `json:"job_id"`
	Status       string   `json:"status"` // job status, or already_processed
	Deduplicated bool     `json:"deduplicated"`
	NoteID       *string  `json:"note_id"`
	Warnings     []string `json:"warnings"`
}

// Upload is the body returned by POST /api/v1/uploads.
type Upload struct {
	UploadID  string `json:"upload_id"`
	Filename  string `json:"filename"`
	SizeBytes int64  `json:"size_bytes"`
	ExpiresAt string `json:"expires_at"`
}

// VideoSource describes where a video came from.
type VideoSource struct {
	Type       string `json:"type"` // file / upload / youtube / bilibili
	URI        string `json:"uri,omitempty"`
	PlatformID string `json:"platform_id,omitempty"`
}

// Video is the video DTO.
type Video struct {
	VideoID      string      `json:"video_id"`
	Title        string      `json:"title"`
	Source       VideoSource `json:"source"`
	DurationMs   *int64      `json:"duration_ms"`
	Language     *string     `json:"language"`
	Status       string      `json:"status"` // processing / ready / failed / cancelled
	LatestJobID  *string     `json:"latest_job_id"`
	TranscriptID *string     `json:"transcript_id"`
	NoteID       *string     `json:"note_id"`
	// PlaybackReady is true once the playback stage has produced (or
	// approved) a browser-playable file.
	PlaybackReady bool   `json:"playback_ready"`
	WebURL        string `json:"web_url"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// VideoList is the body of GET /api/v1/videos.
type VideoList struct {
	Videos     []Video `json:"videos"`
	NextCursor *string `json:"next_cursor"`
}

// ListVideosRequest filters GET /api/v1/videos.
type ListVideosRequest struct {
	Limit         int    `json:"limit,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
	TitleContains string `json:"title_contains,omitempty"`
}

// JobStage is one stage row of a Job.
type JobStage struct {
	Stage      string  `json:"stage"`
	Status     string  `json:"status"`
	Progress   float64 `json:"progress"`
	Reused     bool    `json:"reused"`
	Attempt    int     `json:"attempt"`
	Error      *Error  `json:"error,omitempty"`
	StartedAt  *string `json:"started_at,omitempty"`
	FinishedAt *string `json:"finished_at,omitempty"`
}

// JobResult carries the IDs a job produced so far.
type JobResult struct {
	TranscriptID *string `json:"transcript_id"`
	NoteID       *string `json:"note_id"`
}

// Job is the job DTO.
type Job struct {
	JobID   string  `json:"job_id"`
	Kind    string  `json:"kind"`
	VideoID *string `json:"video_id"`
	Status  string  `json:"status"`
	Rev     int64   `json:"rev"`
	Attempt int     `json:"attempt"`
	// Stage is the first non-finished stage (empty once the job is terminal).
	Stage string `json:"stage"`
	// Progress is the weighted overall progress in [0,1].
	Progress    float64    `json:"progress"`
	Stages      []JobStage `json:"stages"`
	Error       *Error     `json:"error"`
	Result      JobResult  `json:"result"`
	RequestedBy string     `json:"requested_by,omitempty"`
	CreatedAt   string     `json:"created_at"`
	StartedAt   *string    `json:"started_at,omitempty"`
	FinishedAt  *string    `json:"finished_at,omitempty"`
	UpdatedAt   string     `json:"updated_at"`
}

// Terminal reports whether the job reached a final state. interrupted is
// not terminal for waiting purposes only when auto-resume re-queues it, so
// callers treat it as terminal here and re-poll if they wish.
func (j *Job) Terminal() bool {
	switch j.Status {
	case JobSucceeded, JobFailed, JobCancelled, JobInterrupted:
		return true
	}
	return false
}

// JobList is the body of GET /api/v1/jobs.
type JobList struct {
	Jobs       []Job   `json:"jobs"`
	NextCursor *string `json:"next_cursor"`
}

// ListJobsRequest filters GET /api/v1/jobs.
type ListJobsRequest struct {
	Status  string `json:"status,omitempty"`
	VideoID string `json:"video_id,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
}

// GetJobRequest is a job read with optional long polling: when WaitSeconds > 0
// the call returns once rev > AfterRev or the wait elapses.
type GetJobRequest struct {
	JobID       string `json:"job_id"`
	WaitSeconds int    `json:"wait_seconds,omitempty"`
	AfterRev    int64  `json:"after_rev,omitempty"`
}

// Citation is one resolved wk://seg/ reference of a note.
type Citation struct {
	RefNo     int    `json:"ref_no"`
	PointID   string `json:"point_id"`
	SegmentID string `json:"segment_id"`
	StartMs   int64  `json:"start_ms"`
	EndMs     int64  `json:"end_ms"`
	// Text is the cited segment text (hover preview).
	Text string `json:"text,omitempty"`
}

// Coverage is the note coverage metadata.
type Coverage struct {
	Chunks             int  `json:"chunks"`
	ChunksEmpty        int  `json:"chunks_empty"`
	Points             int  `json:"points"`
	PointsDropped      int  `json:"points_dropped"`
	PointsUnplaced     int  `json:"points_unplaced"`
	Citations          int  `json:"citations"`
	CitationsDropped   int  `json:"citations_dropped"`
	RepairCalls        int  `json:"repair_calls"`
	SplitRetries       int  `json:"split_retries"`
	TranscriptSegments int  `json:"transcript_segments"`
	TranscriptChars    int  `json:"transcript_chars"`
	OverviewFallback   bool `json:"overview_fallback,omitempty"`
}

// GetNoteRequest reads the current (or a historical) note of a video.
type GetNoteRequest struct {
	VideoID  string `json:"video_id"`
	Format   string `json:"format,omitempty"` // markdown (default) / json
	Offset   int    `json:"offset,omitempty"`
	MaxChars int    `json:"max_chars,omitempty"`
	NoteID   string `json:"note_id,omitempty"`
}

// Note is the note read result. Content is the canonical Markdown (with
// wk://seg/ links) or the structured note JSON as a string; offset and
// max_chars count Unicode code points.
type Note struct {
	NoteID        string     `json:"note_id"`
	VideoID       string     `json:"video_id"`
	TranscriptID  string     `json:"transcript_id"`
	Version       int        `json:"version"`
	Title         string     `json:"title"`
	Format        string     `json:"format"`
	Content       string     `json:"content"`
	TotalChars    int        `json:"total_chars"`
	NextOffset    *int       `json:"next_offset"`
	Citations     []Citation `json:"citations"`
	Coverage      Coverage   `json:"coverage"`
	LLMProvider   string     `json:"llm_provider"`
	LLMModel      string     `json:"llm_model"`
	PromptVersion string     `json:"prompt_version"`
	CreatedAt     string     `json:"created_at"`
}

// Segment is one transcript segment; times are relative to the original video.
type Segment struct {
	SegmentID string `json:"segment_id"`
	Idx       int    `json:"idx"` // 1-based index inside the transcript
	StartMs   int64  `json:"start_ms"`
	EndMs     int64  `json:"end_ms"`
	Text      string `json:"text"`
}

// GetTranscriptRequest reads segments by time range or by IDs.
type GetTranscriptRequest struct {
	VideoID    string   `json:"video_id"`
	StartMs    *int64   `json:"start_ms,omitempty"`
	EndMs      *int64   `json:"end_ms,omitempty"`
	SegmentIDs []string `json:"segment_ids,omitempty"` // at most 50
	Format     string   `json:"format,omitempty"`      // json (default) / srt / vtt / txt
	// MaxChars limits the returned text; 0 means the default. A negative
	// value means no limit (used by export).
	MaxChars int `json:"max_chars,omitempty"`
}

// Transcript is the transcript read result. For format json, Segments is
// filled; for srt / vtt / txt, Content holds the rendered text as well.
type Transcript struct {
	TranscriptID string    `json:"transcript_id"`
	VideoID      string    `json:"video_id"`
	Language     string    `json:"language"`
	Engine       string    `json:"engine"`
	Model        string    `json:"model"`
	SegmentCount int       `json:"segment_count"`
	Format       string    `json:"format"`
	Segments     []Segment `json:"segments"`
	Content      string    `json:"content,omitempty"`
	Truncated    bool      `json:"truncated"`
	NextStartMs  *int64    `json:"next_start_ms"`
}

// Playback is the body of GET /api/v1/videos/{id}/playback.
type Playback struct {
	URL       string `json:"url"`
	Mime      string `json:"mime"`
	ExpiresAt string `json:"expires_at"`
}

// ExportRequest is the body of POST /api/v1/videos/{id}/exports.
type ExportRequest struct {
	VideoID string `json:"video_id,omitempty"`
	Format  string `json:"format"` // md / md-raw / json / srt / vtt / txt
}

// Export describes a file written under <data>/exports/.
type Export struct {
	ExportID    string `json:"export_id"`
	VideoID     string `json:"video_id"`
	Format      string `json:"format"`
	Path        string `json:"path"` // absolute path inside the data dir
	Filename    string `json:"filename"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	DownloadURL string `json:"download_url"`
	CreatedAt   string `json:"created_at"`
}

// Event is one entry of the event stream. Exactly one of
// Job / Video / Tools / Reason is set depending on Type.
type Event struct {
	ID    int64        `json:"id"`
	Type  string       `json:"type"`
	Job   *Job         `json:"job,omitempty"`
	Video *Video       `json:"video,omitempty"`
	Tools *ToolsStatus `json:"tools,omitempty"`
	// Reason is set for engine.stopping.
	Reason string `json:"reason,omitempty"`
}

// Event types.
const (
	EventJobUpdated     = "job.updated"
	EventVideoUpdated   = "video.updated"
	EventToolsUpdated   = "tools.updated"
	EventEngineStopping = "engine.stopping"
)

// Tool item kinds and states.
const (
	ToolKindTool  = "tool"
	ToolKindModel = "model"

	ToolReady       = "ready"
	ToolMissing     = "missing"
	ToolDownloading = "downloading"
	ToolFailed      = "failed"
)

// ToolItem is the state of one tool or model.
type ToolItem struct {
	Name     string `json:"name"` // manifest key, e.g. ffmpeg / whisper-cli / whisper-small-q5_1
	Kind     string `json:"kind"` // tool / model
	Version  string `json:"version,omitempty"`
	Status   string `json:"status"`
	Embedded bool   `json:"embedded"`
	// Lazy items are only needed for specific sources (deno → YouTube).
	Lazy bool `json:"lazy"`
	// Required is true when a local-file import needs this item with the
	// current configuration.
	Required      bool    `json:"required"`
	SizeBytes     int64   `json:"size_bytes"`     // installed size, 0 when missing
	DownloadBytes int64   `json:"download_bytes"` // expected download size
	Progress      float64 `json:"progress,omitempty"`
	Path          string  `json:"path,omitempty"`
	License       string  `json:"license,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// ToolsStatus is the body of GET /api/v1/tools.
type ToolsStatus struct {
	Items []ToolItem `json:"items"`
	// Ready is true when every Required item is ready.
	Ready bool `json:"ready"`
	// MissingDownloadBytes sums DownloadBytes of the Required missing items.
	MissingDownloadBytes int64  `json:"missing_download_bytes"`
	DefaultASRModel      string `json:"default_asr_model"`
}

// PrepareToolsRequest is the body of POST /api/v1/tools/prepare. Empty Items
// means every Required missing item.
type PrepareToolsRequest struct {
	Items []string `json:"items"`
}

// ToolImportRequest imports a manually downloaded tool or model file
// (CLI: tools import / tools import-model). POST /api/v1/tools/import.
type ToolImportRequest struct {
	Name string `json:"name"`
	Path string `json:"path"` // absolute local path, readable by the engine
}

// LLMTestResult is the body of POST /api/v1/settings/llm/test.
type LLMTestResult struct {
	OK bool `json:"ok"`
	// StructuredOutput is the level that worked: json_schema / json_object / prompt.
	StructuredOutput string `json:"structured_output,omitempty"`
	Model            string `json:"model,omitempty"`
	LatencyMs        int64  `json:"latency_ms"`
	Error            *Error `json:"error,omitempty"`
}

// LLMModelsRequest is the body of POST /api/v1/settings/llm/models. Both
// fields are optional overrides so the settings form can list models for
// values the user has typed but not saved yet; empty means the saved value.
type LLMModelsRequest struct {
	BaseURL string `json:"base_url,omitempty"`
	// APIKey is used for this one request only and is never stored.
	APIKey string `json:"api_key,omitempty"`
}

// LLMModelList is the model IDs an OpenAI-compatible endpoint offers.
type LLMModelList struct {
	BaseURL string   `json:"base_url"`
	Models  []string `json:"models"`
}
