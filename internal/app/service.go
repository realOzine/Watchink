package app

import (
	"context"
	"io"
	"time"
)

// Service is the complete set of v0 use cases. The engine implements it
// in-process; internal/client implements it over HTTP, so the CLI, the MCP
// server and the desktop shell are written against this interface only.
//
// Every method returns *Error (see errors.go) for business failures.
type Service interface {
	EngineInfo(ctx context.Context) (*EngineInfo, error)

	// Import applies the dedup policy of docs/v0.md §7.5 and returns at once.
	Import(ctx context.Context, req ImportRequest) (*ImportResponse, error)
	// CreateUpload stores a raw byte stream as a pending upload (Web import).
	// size may be -1 when unknown.
	CreateUpload(ctx context.Context, filename string, size int64, body io.Reader) (*Upload, error)

	ListJobs(ctx context.Context, req ListJobsRequest) (*JobList, error)
	GetJob(ctx context.Context, req GetJobRequest) (*Job, error)
	CancelJob(ctx context.Context, jobID string) (*Job, error)
	RetryJob(ctx context.Context, jobID string) (*Job, error)

	ListVideos(ctx context.Context, req ListVideosRequest) (*VideoList, error)
	GetVideo(ctx context.Context, videoID string) (*Video, error)
	GetNote(ctx context.Context, req GetNoteRequest) (*Note, error)
	GetTranscript(ctx context.Context, req GetTranscriptRequest) (*Transcript, error)
	GetPlayback(ctx context.Context, videoID string) (*Playback, error)

	// CreateExport writes the export under <data>/exports/ and registers it.
	CreateExport(ctx context.Context, req ExportRequest) (*Export, error)
	// OpenExport streams a registered export; the caller closes the reader.
	OpenExport(ctx context.Context, exportID string) (io.ReadCloser, *Export, error)

	GetSettings(ctx context.Context) (*Settings, error)
	// PatchSettings applies a JSON merge patch shaped like Settings, e.g.
	// {"llm": {"model": "x"}}; unknown keys are INVALID_ARGUMENT.
	PatchSettings(ctx context.Context, patch map[string]any) (*Settings, error)
	// SetSecret stores a write-only secret; an empty value deletes it.
	SetSecret(ctx context.Context, name, value string) error
	TestLLM(ctx context.Context) (*LLMTestResult, error)
	// ListLLMModels asks the configured (or overridden) endpoint which
	// models it offers, for the model dropdown of the settings form.
	ListLLMModels(ctx context.Context, req LLMModelsRequest) (*LLMModelList, error)

	GetTools(ctx context.Context) (*ToolsStatus, error)
	// PrepareTools creates (or returns the active) prepare_tools job.
	PrepareTools(ctx context.Context, req PrepareToolsRequest) (*Job, error)
	// UpdateTool installs the latest upstream release of a tool (v0: yt-dlp).
	UpdateTool(ctx context.Context, name string) (*ToolsStatus, error)
	// ImportTool installs a manually downloaded tool or model file.
	ImportTool(ctx context.Context, req ToolImportRequest) (*ToolsStatus, error)

	// Subscribe streams events with ID > afterID until ctx is done. reset is
	// true when afterID is older than the replay buffer (or newer than the
	// current engine run) and the caller must do a full refresh.
	Subscribe(ctx context.Context, afterID int64) (events <-chan Event, reset bool, err error)
}

// PlaybackFile is an opened, range-servable playback file.
type PlaybackFile struct {
	Content io.ReadSeekCloser
	Name    string
	Mime    string
	ModTime time.Time
}

// Local is the engine-side service: Service plus the operations that only
// make sense inside the engine process. internal/api serves a Local.
type Local interface {
	Service

	// OpenPlayback validates the signed media token (§10.2) and opens the
	// playback file of the video.
	OpenPlayback(ctx context.Context, videoID, mediaToken string) (*PlaybackFile, error)
	// WebEnabled reports whether the optional browser UI is switched on
	// (settings web.enabled). While it is off, only bearer-token clients
	// (desktop, CLI, MCP) may use the HTTP API.
	WebEnabled() bool
	// ActiveJobs counts queued + running + cancelling jobs (idle exit, §5.4).
	ActiveJobs(ctx context.Context) (int, error)
	// Stop stops accepting work and interrupts running stages; force kills
	// child processes immediately instead of waiting up to 10 s (§5.4).
	Stop(ctx context.Context, force bool) error
}

// Remote is what a client process holds after connecting to an engine.
type Remote interface {
	Service

	BaseURL() string
	// Shutdown asks the engine to exit (`engine stop`).
	Shutdown(ctx context.Context, force bool) error
	Close() error
}

// ConnectOptions controls engine discovery (docs/v0.md §5.2).
type ConnectOptions struct {
	// DataDir is the --data-dir flag value; empty means resolve by
	// WATCHINK_DATA_DIR, location.toml, then the platform default.
	DataDir string
	// NoSpawn fails with ENGINE_UNAVAILABLE instead of starting an engine.
	NoSpawn bool
	// Timeout bounds discovery + spawn; 0 means 15 s.
	Timeout time.Duration
	// Requester is recorded as jobs.requested_by: desktop / web / cli / mcp.
	Requester string
	// AnyBuild accepts a running engine of another build instead of
	// replacing it or failing with ENGINE_VERSION_MISMATCH. `engine status`
	// and `engine stop` set it: they are how a mismatch gets resolved.
	AnyBuild bool
}

// Connector connects to (and by default spawns) the engine of a data dir.
// internal/client.Connect is the production implementation.
type Connector func(ctx context.Context, opts ConnectOptions) (Remote, error)
