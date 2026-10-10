package app

// Settings is the non-secret configuration stored in <data>/config.toml and
// exposed by GET / PATCH /api/v1/settings. The struct carries
// both TOML and JSON tags so the file and the API share one shape. Durations
// and sizes are strings ("10m", "8GiB").
type Settings struct {
	Engine   EngineSettings   `json:"engine" toml:"engine"`
	ASR      ASRSettings      `json:"asr" toml:"asr"`
	LLM      LLMSettings      `json:"llm" toml:"llm"`
	Note     NoteSettings     `json:"note" toml:"note"`
	Media    MediaSettings    `json:"media" toml:"media"`
	Tools    ToolsSettings    `json:"tools" toml:"tools"`
	Network  NetworkSettings  `json:"network" toml:"network"`
	Download DownloadSettings `json:"download" toml:"download"`
	Security SecuritySettings `json:"security" toml:"security"`
	Web      WebSettings      `json:"web" toml:"web"`

	// Read-only, filled by the engine; never written to config.toml.
	DataDir   string `json:"data_dir,omitempty" toml:"-"`
	ConfigDir string `json:"config_dir,omitempty" toml:"-"`
}

type EngineSettings struct {
	Port       int    `json:"port" toml:"port"`
	IdleExit   string `json:"idle_exit" toml:"idle_exit"`
	AutoResume bool   `json:"auto_resume" toml:"auto_resume"`
}

type ASRSettings struct {
	Engine   string `json:"engine" toml:"engine"`
	Model    string `json:"model" toml:"model"`       // manifest ID; empty = platform default
	Language string `json:"language" toml:"language"` // auto / zh / en
	Threads  int    `json:"threads" toml:"threads"`   // 0 = auto
	VAD      bool   `json:"vad" toml:"vad"`
}

type LLMSettings struct {
	Provider         string  `json:"provider" toml:"provider"`
	BaseURL          string  `json:"base_url" toml:"base_url"`
	Model            string  `json:"model" toml:"model"`
	APIKeyEnv        string  `json:"api_key_env" toml:"api_key_env"`
	StructuredOutput string  `json:"structured_output" toml:"structured_output"` // auto / json_schema / json_object / prompt
	Temperature      float64 `json:"temperature" toml:"temperature"`
	Timeout          string  `json:"timeout" toml:"timeout"`
	MaxConcurrency   int     `json:"max_concurrency" toml:"max_concurrency"`

	// Read-only credential state (secrets are write-only).
	APIKeyConfigured bool   `json:"api_key_configured" toml:"-"`
	APIKeyLast4      string `json:"api_key_last4,omitempty" toml:"-"`
	// APIKeySource is env / env:<NAME> / file / "" (not configured).
	APIKeySource string `json:"api_key_source,omitempty" toml:"-"`
}

type NoteSettings struct {
	ChunkMaxTokens   int    `json:"chunk_max_tokens" toml:"chunk_max_tokens"`
	ChunkMaxDuration string `json:"chunk_max_duration" toml:"chunk_max_duration"`
}

type MediaSettings struct {
	KeepAudio         bool `json:"keep_audio" toml:"keep_audio"`
	PlaybackMaxHeight int  `json:"playback_max_height" toml:"playback_max_height"`
}

type ToolsSettings struct {
	AutoPrepare bool `json:"auto_prepare" toml:"auto_prepare"`
}

type NetworkSettings struct {
	Proxy string `json:"proxy" toml:"proxy"`
}

type DownloadSettings struct {
	// Mirrors rewrites download URLs by host name, e.g.
	// "huggingface.co" = "https://hf-mirror.com".
	Mirrors map[string]string `json:"mirrors" toml:"mirrors"`
}

type SecuritySettings struct {
	ImportRoots   []string `json:"import_roots" toml:"import_roots"`
	MaxUploadSize string   `json:"max_upload_size" toml:"max_upload_size"`
}

// WebSettings controls the optional browser UI.
type WebSettings struct {
	// Enabled serves the Web UI at the engine address to browsers on this
	// machine, without a login. Off by default; the desktop app, the CLI
	// and MCP never depend on it.
	Enabled bool `json:"enabled" toml:"enabled"`
}

// DefaultSettings returns the defaults listed.
func DefaultSettings() Settings {
	return Settings{
		Engine: EngineSettings{Port: DefaultEnginePort, IdleExit: "10m", AutoResume: true},
		// VAD is off by default: with it, whisper.cpp v1.9.5 was measured to
		// shift the first segment after a removed silence to the start of the
		// silence, which breaks citation times.
		ASR: ASRSettings{Engine: "whispercpp", Language: "auto", VAD: false},
		LLM: LLMSettings{
			Provider: "openai-compatible", StructuredOutput: "auto",
			Temperature: 0.2, Timeout: "120s", MaxConcurrency: 3,
		},
		Note:     NoteSettings{ChunkMaxTokens: 3000, ChunkMaxDuration: "10m"},
		Media:    MediaSettings{KeepAudio: false, PlaybackMaxHeight: 720},
		Tools:    ToolsSettings{AutoPrepare: true},
		Download: DownloadSettings{Mirrors: map[string]string{}},
		Security: SecuritySettings{ImportRoots: []string{"~"}, MaxUploadSize: "8GiB"},
	}
}

// SecretLLMAPIKey is the only secret name accepted by SetSecret in v0.
const SecretLLMAPIKey = "llm_api_key"
