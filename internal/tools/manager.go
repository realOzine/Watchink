package tools

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"watchink/internal/app"
)

// EnvWhisperBin overrides the embedded whisper-cli with an external
// executable (local development).
const EnvWhisperBin = "WATCHINK_WHISPER_BIN"

// Well-known manifest names.
const (
	ToolFFmpeg     = "ffmpeg"
	ToolFFprobe    = "ffprobe"
	ToolYtDlp      = "yt-dlp"
	ToolDeno       = "deno"
	ToolWhisperCLI = "whisper-cli"
	ModelSileroVAD = "silero-vad"

	ModelLargeV3TurboQ5 = "whisper-large-v3-turbo-q5_0"
	ModelSmallQ5        = "whisper-small-q5_1"
)

// ExecFunc runs path with args and returns its combined output. The package
// may not create processes itself, so the caller injects one backed by
// internal/procx. It is only ever called on files that passed verification.
type ExecFunc func(ctx context.Context, path string, args ...string) (output []byte, err error)

// Options configures a Manager.
type Options struct {
	// DataDir is <data>; tools live in <data>/tools, models in <data>/models.
	DataDir string
	// Exec runs the post-install smoke tests. Required.
	Exec ExecFunc
	// Mirrors and Proxy are the initial network settings; see SetNetwork.
	Mirrors map[string]string
	Proxy   string
	// OnChange, if set, is called (without locks held) whenever an item
	// starts, finishes or fails installing, so the caller can publish
	// tools.updated. It is not called for progress ticks.
	OnChange func()
	Logger   *slog.Logger

	// The fields below exist for tests; zero values select the built-ins.
	Manifest   *Manifest
	Platform   string       // "<os>-<arch>"
	Embedded   fs.FS        // rooted at "<os>-<arch>/"
	HTTPClient *http.Client // replaces the proxy-aware client
}

// Manager installs and locates tools and models under one data directory.
// All methods are safe for concurrent use. One process is expected to own a
// data directory (the engine).
type Manager struct {
	dataDir  string
	exec     ExecFunc
	manifest *Manifest
	platform string
	embedded fs.FS
	client   *http.Client
	onChange func()
	log      *slog.Logger

	attempts     int
	retryDelay   time.Duration
	idleTimeout  time.Duration
	smokeTimeout time.Duration

	netMu   sync.RWMutex
	mirrors map[string]string
	proxy   string

	instMu sync.Mutex // serializes installed.json read-modify-write

	mu      sync.Mutex
	flights map[string]*flight
	states  map[string]itemState
}

// flight is one in-progress installation of an item; concurrent Ensure calls
// for the same item share it instead of downloading twice.
type flight struct {
	done chan struct{}
	err  error

	mu        sync.Mutex
	listeners []func(done, total int64)
}

// itemState is the transient state shown by Status on top of what is on disk.
type itemState struct {
	status      string // app.ToolDownloading / app.ToolFailed
	done, total int64
	err         string
}

// New creates a Manager and its directories.
func New(opts Options) (*Manager, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf("tools: DataDir is required")
	}
	if opts.Exec == nil {
		return nil, fmt.Errorf("tools: Exec is required")
	}
	m := &Manager{
		dataDir:      opts.DataDir,
		exec:         opts.Exec,
		manifest:     opts.Manifest,
		platform:     opts.Platform,
		embedded:     opts.Embedded,
		client:       opts.HTTPClient,
		onChange:     opts.OnChange,
		log:          opts.Logger,
		attempts:     4,
		retryDelay:   time.Second,
		idleTimeout:  60 * time.Second,
		smokeTimeout: 2 * time.Minute,
		flights:      map[string]*flight{},
		states:       map[string]itemState{},
	}
	if m.log == nil {
		m.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if m.manifest == nil {
		mf, err := DefaultManifest()
		if err != nil {
			return nil, err
		}
		m.manifest = mf
	}
	if m.platform == "" {
		m.platform = CurrentPlatform()
	}
	if m.embedded == nil {
		m.embedded = embeddedFS()
	}
	if m.client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = m.proxyFor
		tr.ResponseHeaderTimeout = 60 * time.Second
		m.client = &http.Client{Transport: tr}
	}
	m.SetNetwork(opts.Mirrors, opts.Proxy)

	for _, d := range []string{m.stagingDir(), m.modelsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, ioErr(err, "创建工具目录")
		}
	}
	m.cleanStaging()
	return m, nil
}

func (m *Manager) toolsDir() string   { return filepath.Join(m.dataDir, "tools") }
func (m *Manager) modelsDir() string  { return filepath.Join(m.dataDir, "models") }
func (m *Manager) stagingDir() string { return filepath.Join(m.toolsDir(), ".staging") }
func (m *Manager) partDir() string    { return filepath.Join(m.stagingDir(), "dl") }
func (m *Manager) binDir() string     { return filepath.Join(m.toolsDir(), "bin") }

func (m *Manager) versionDir(name, version string) string {
	return filepath.Join(m.toolsDir(), name, version)
}

// cleanStaging removes what crashed installs left behind. Partial downloads
// are kept for two weeks so they can be resumed; work directories only need
// to outlive a running install.
func (m *Manager) cleanStaging() {
	sweep := func(dir string, maxAge time.Duration, skip string) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() == skip {
				continue
			}
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < maxAge {
				continue
			}
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	sweep(m.stagingDir(), time.Hour, "dl")
	sweep(m.partDir(), 14*24*time.Hour, "")
}

// DefaultASRModel returns the manifest ID of the default transcription model
// for this platform (decided by measurements).
func (m *Manager) DefaultASRModel() string { return defaultASRModel(m.platform) }

func defaultASRModel(platform string) string {
	switch platform {
	case "darwin-arm64":
		// on an Apple M3 Pro with Metal: see third_party/whisper/NOTES.md.
		return ModelLargeV3TurboQ5
	default:
		// TODO(windows) TODO(linux): not measured yet; CPU-only
		// transcription gets the small model until it is.
		return ModelSmallQ5
	}
}

// Needed lists the manifest names a source of the given type requires:
// ffmpeg, ffprobe, whisper-cli and the ASR model always; silero-vad when VAD
// is on; yt-dlp for URL sources; deno only for YouTube. An empty
// asrModel means the platform default. app.SourceURL, the not yet classified
// import type, counts as "not YouTube".
func (m *Manager) Needed(sourceType string, asrModel string, vad bool) []string {
	if asrModel == "" {
		asrModel = m.DefaultASRModel()
	}
	names := []string{ToolFFmpeg, ToolFFprobe}
	switch sourceType {
	case app.SourceURL, app.SourceBilibili:
		names = append(names, ToolYtDlp)
	case app.SourceYouTube:
		names = append(names, ToolYtDlp, ToolDeno)
	}
	names = append(names, ToolWhisperCLI)
	if vad {
		names = append(names, ModelSileroVAD)
	}
	return append(names, asrModel)
}

// toolInfo is the on-disk state of one tool.
type toolInfo struct {
	version string // the version Path would use, or the wanted one
	path    string
	size    int64
	ready   bool
	// problem explains a tool that cannot be installed at all on this build.
	problem *app.Error
}

// wantedVersion picks the version a tool should run: the user's choice when
// pinned by an update or rollback, else the manifest's.
func wantedVersion(t Tool, rec *installedTool) string {
	if rec != nil && rec.Pinned && rec.Active != "" && rec.Versions[rec.Active] != nil {
		return rec.Active
	}
	return t.Version
}

func (m *Manager) toolInfo(inst *installedFile, name string) toolInfo {
	t := m.manifest.Tools[name]
	rec := inst.Tools[name]
	info := toolInfo{version: wantedVersion(t, rec)}

	if name == ToolWhisperCLI {
		if p := os.Getenv(EnvWhisperBin); p != "" {
			st, err := os.Stat(p)
			if err != nil || st.IsDir() {
				info.problem = app.E(app.CodeToolMissing, "%s 指向的文件不存在：%s", EnvWhisperBin, p).
					WithHint("请修正或取消该环境变量。")
				return info
			}
			info.path, info.size, info.ready = p, st.Size(), true
			return info
		}
	}

	var wantSHA string
	if t.Embedded {
		sum, err := m.embeddedSHA(name)
		if err != nil {
			info.problem = err
			return info
		}
		wantSHA = sum
	} else if _, ok := t.Platforms[m.platform]; !ok {
		info.problem = m.errNoPlatform(name)
		return info
	}

	if rec == nil || rec.Active != info.version {
		return info
	}
	v := rec.Versions[info.version]
	if v == nil || (wantSHA != "" && v.SHA256 != wantSHA) {
		// For an embedded tool a different hash means the program was rebuilt
		// with another binary under the same version: extract again.
		return info
	}
	p := filepath.Join(m.versionDir(name, info.version), v.Bin)
	st, err := os.Stat(p)
	if err != nil || st.Size() != v.BinSize {
		return info
	}
	info.path, info.size, info.ready = p, v.Size, true
	return info
}

func (m *Manager) errNoPlatform(name string) *app.Error {
	return app.E(app.CodeToolMissing, "%s 暂不支持当前平台（%s）", name, m.platform).
		WithHint("清单中没有该平台的下载地址。").WithDetail("tool", name).WithDetail("platform", m.platform)
}

// embeddedFile is the path of an embedded binary inside m.embedded; its
// sha256 sits next to it as "<file>.sha256".
func (m *Manager) embeddedFile(name string) string {
	return m.platform + "/" + exeName(m.platform, name)
}

// embeddedSHA returns the recorded sha256 of an embedded binary, or
// TOOL_MISSING when this build was made without it.
func (m *Manager) embeddedSHA(name string) (string, *app.Error) {
	missing := func() *app.Error {
		osName, arch, _ := strings.Cut(m.platform, "-")
		return app.E(app.CodeToolMissing, "此构建未内嵌 %s（%s）", name, m.platform).
			WithHint(fmt.Sprintf("请先运行 third_party/whisper/build.sh %s %s 后重新构建，或设置环境变量 %s 指向可用的 whisper-cli。", osName, arch, EnvWhisperBin)).
			WithDetail("tool", name).WithDetail("platform", m.platform)
	}
	file := m.embeddedFile(name)
	if st, err := fs.Stat(m.embedded, file); err != nil || st.IsDir() {
		return "", missing()
	}
	data, err := fs.ReadFile(m.embedded, file+".sha256")
	if err != nil {
		return "", missing()
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || !sha256RE.MatchString(fields[0]) {
		return "", missing()
	}
	return fields[0], nil
}

func (m *Manager) modelInfo(inst *installedFile, name string) (path string, sum string, size int64, ready bool) {
	md := m.manifest.Models[name]
	rec := inst.Models[name]
	if rec == nil || rec.SHA256 != md.SHA256 || rec.File != md.FileName() {
		return "", "", 0, false
	}
	p := filepath.Join(m.modelsDir(), name, rec.File)
	st, err := os.Stat(p)
	if err != nil || st.Size() != rec.Size {
		return "", "", 0, false
	}
	return p, rec.SHA256, rec.Size, true
}

func errNotInstalled(name string) *app.Error {
	return app.E(app.CodeToolMissing, "%s 尚未安装", name).
		WithHint("请运行 watchink tools prepare，或在设置页准备工具和模型。").WithDetail("tool", name)
}

// Path returns the executable of an installed tool, or TOOL_MISSING.
func (m *Manager) Path(name string) (string, error) {
	if _, ok := m.manifest.Tools[name]; !ok {
		return "", errUnknownItem(name)
	}
	info := m.toolInfo(m.loadInstalled(), name)
	if info.problem != nil {
		return "", info.problem
	}
	if !info.ready {
		return "", errNotInstalled(name)
	}
	return info.path, nil
}

// BinDir returns a directory holding links to the active ffmpeg and ffprobe
// (and every other installed tool) under their plain names. yt-dlp's
// --ffmpeg-location needs both programs in one directory, while each
// tool is installed in its own versioned one.
func (m *Manager) BinDir() string { return m.binDir() }

// ModelPath returns the file and sha256 of an installed model, or
// TOOL_MISSING.
func (m *Manager) ModelPath(id string) (path string, sha256 string, err error) {
	if _, ok := m.manifest.Models[id]; !ok {
		return "", "", errUnknownItem(id)
	}
	p, sum, _, ready := m.modelInfo(m.loadInstalled(), id)
	if !ready {
		return "", "", errNotInstalled(id)
	}
	return p, sum, nil
}

// Version returns the version of a tool: the installed active one, or the
// manifest's when it is not installed. It is "" for models and unknown names.
func (m *Manager) Version(name string) string {
	t, ok := m.manifest.Tools[name]
	if !ok {
		return ""
	}
	return wantedVersion(t, m.loadInstalled().Tools[name])
}

// InstalledVersions lists the versions of a tool recorded in installed.json
// and which of them is active.
func (m *Manager) InstalledVersions(name string) (versions []string, active string) {
	rec := m.loadInstalled().Tools[name]
	if rec == nil {
		return nil, ""
	}
	for v := range rec.Versions {
		versions = append(versions, v)
	}
	sortStrings(versions)
	return versions, rec.Active
}

// Status reports every tool and model. Required marks what a local-file
// import needs with the given ASR model ("" = platform default) and VAD
// setting.
func (m *Manager) Status(asrModel string, vad bool) app.ToolsStatus {
	required := map[string]bool{}
	for _, n := range m.Needed(app.SourceFile, asrModel, vad) {
		required[n] = true
	}
	inst := m.loadInstalled()
	m.mu.Lock()
	states := make(map[string]itemState, len(m.states))
	for k, v := range m.states {
		states[k] = v
	}
	m.mu.Unlock()

	out := app.ToolsStatus{Ready: true, DefaultASRModel: m.DefaultASRModel(), Items: []app.ToolItem{}}
	add := func(it app.ToolItem) {
		it.Required = required[it.Name]
		if it.Status != app.ToolReady {
			if st, ok := states[it.Name]; ok {
				it.Status = st.status
				if st.status == app.ToolDownloading && st.total > 0 {
					it.Progress = float64(st.done) / float64(st.total)
				}
				if st.err != "" {
					it.Error = st.err
				}
			}
			if it.Required {
				out.Ready = false
				out.MissingDownloadBytes += it.DownloadBytes
			}
		}
		out.Items = append(out.Items, it)
	}
	for _, name := range m.manifest.toolNames() {
		t := m.manifest.Tools[name]
		info := m.toolInfo(inst, name)
		it := app.ToolItem{
			Name: name, Kind: app.ToolKindTool, Version: info.version, Status: app.ToolMissing,
			Embedded: t.Embedded, Lazy: t.Lazy, License: t.License,
			DownloadBytes: t.Platforms[m.platform].Size,
		}
		switch {
		case info.ready:
			it.Status, it.Path, it.SizeBytes = app.ToolReady, info.path, info.size
		case info.problem != nil:
			it.Error = info.problem.Message
		}
		add(it)
	}
	for _, name := range m.manifest.modelNames() {
		md := m.manifest.Models[name]
		it := app.ToolItem{Name: name, Kind: app.ToolKindModel, Status: app.ToolMissing, License: md.License, DownloadBytes: md.Size}
		if p, _, size, ready := m.modelInfo(inst, name); ready {
			it.Status, it.Path, it.SizeBytes = app.ToolReady, p, size
		}
		add(it)
	}
	// An unknown configured model can never become ready; say so instead of
	// reporting Ready.
	for n := range required {
		_, isTool := m.manifest.Tools[n]
		_, isModel := m.manifest.Models[n]
		if !isTool && !isModel {
			out.Ready = false
		}
	}
	return out
}

// Ensure installs every named tool or model that is not ready yet, in order,.
// progress, if not nil, receives the bytes downloaded so far
// and the expected total per item (total is 0 when unknown); it may be called
// from other goroutines when calls overlap. A cancelled ctx returns ctx.Err().
func (m *Manager) Ensure(ctx context.Context, names []string, progress func(name string, done, total int64)) error {
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, isTool := m.manifest.Tools[name]
		_, isModel := m.manifest.Models[name]
		if !isTool && !isModel {
			return errUnknownItem(name)
		}
		var report func(done, total int64)
		if progress != nil {
			report = func(done, total int64) { progress(name, done, total) }
		}
		err := m.exclusive(ctx, name, true, report, func(report func(done, total int64)) error {
			if isTool {
				return m.ensureTool(ctx, name, report)
			}
			return m.ensureModel(ctx, name, report)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) isReady(name string) bool {
	inst := m.loadInstalled()
	if _, ok := m.manifest.Tools[name]; ok {
		return m.toolInfo(inst, name).ready
	}
	_, _, _, ready := m.modelInfo(inst, name)
	return ready
}

// exclusive runs fn as the only installer of name. With join set (Ensure), a
// caller that finds the item ready returns at once, and one that finds an
// install in progress waits for it and shares its result; when that install
// was cancelled by its own caller, the waiter takes over. Without join
// (Import, update) the caller waits its turn and then always runs fn.
func (m *Manager) exclusive(ctx context.Context, name string, join bool, report func(done, total int64), fn func(report func(done, total int64)) error) error {
	for {
		m.mu.Lock()
		f := m.flights[name]
		if f == nil {
			if join && m.isReady(name) {
				m.mu.Unlock()
				return nil
			}
			f = &flight{done: make(chan struct{})}
			if report != nil {
				f.listeners = append(f.listeners, report)
			}
			m.flights[name] = f
			m.states[name] = itemState{status: app.ToolDownloading}
			m.mu.Unlock()
			m.changed()

			err := fn(func(done, total int64) { m.progress(name, f, done, total) })

			m.mu.Lock()
			delete(m.flights, name)
			if err != nil && !isCtxErr(err) {
				m.states[name] = itemState{status: app.ToolFailed, err: app.AsError(err).Message}
			} else {
				delete(m.states, name)
			}
			f.err = err
			m.mu.Unlock()
			close(f.done)
			m.changed()
			return err
		}
		if join && report != nil {
			f.mu.Lock()
			f.listeners = append(f.listeners, report)
			f.mu.Unlock()
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.done:
		}
		if !join {
			continue
		}
		if f.err == nil {
			return nil
		}
		if isCtxErr(f.err) && ctx.Err() == nil {
			continue // the other caller gave up; this one has not
		}
		return f.err
	}
}

func (m *Manager) progress(name string, f *flight, done, total int64) {
	m.mu.Lock()
	if st, ok := m.states[name]; ok && st.status == app.ToolDownloading {
		st.done, st.total = done, total
		m.states[name] = st
	}
	m.mu.Unlock()
	f.mu.Lock()
	listeners := append([]func(done, total int64){}, f.listeners...)
	f.mu.Unlock()
	for _, l := range listeners {
		l(done, total)
	}
}

func (m *Manager) changed() {
	if m.onChange != nil {
		m.onChange()
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
