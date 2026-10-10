//go:build desktop

package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"watchink/internal/app"
)

// TokenProvider is the optional interface the app.Remote returned by
// Options.Connect must also implement: the WebView talks to the engine over
// HTTP itself and therefore needs the bearer token, which app.Remote
// does not expose.
type TokenProvider interface {
	Token() string
}

// EngineInfo is the result of the EngineInfo binding.
type EngineInfo struct {
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
	Version string `json:"version"`
}

// Snippet is one ready-to-paste client configuration.
type Snippet struct {
	Name     string `json:"name"`
	Language string `json:"language"` // bash / json
	Content  string `json:"content"`
}

// AgentSetup is the result of the AgentSetupInfo binding.
type AgentSetup struct {
	Executable string    `json:"executable"`
	MCPCommand []string  `json:"mcp_command"`
	Snippets   []Snippet `json:"snippets"`
}

// VideoExtensions are the extensions offered by the PickVideoFile dialog. The
// filter is a convenience only; the engine probes the file.
var VideoExtensions = []string{
	"mp4", "m4v", "mov", "mkv", "webm", "avi", "flv", "wmv", "ts", "m2ts", "mts", "mpg", "mpeg", "3gp", "ogv",
}

// engineCheckTimeout bounds the liveness probe of an existing connection.
const engineCheckTimeout = 5 * time.Second

// App is the single struct bound into the WebView. Its exported methods are
// exactly the six bindings, reachable from the frontend as
// window.go.desktop.App.<Method>; do not add exported methods.
//
// A failed call rejects with an Error whose message is the JSON text of an
// app.Error (see formatError): JSON.parse(err.message).
type App struct {
	opts Options
	log  *slog.Logger

	// Native capabilities, injected so the bindings are testable without a
	// window. Run wires them to the Wails runtime.
	pickFile func(ctx context.Context) (string, error)
	saveFile func(ctx context.Context, defaultName string) (string, error)
	openURL  func(ctx context.Context, url string)

	mu     sync.Mutex
	ctx    context.Context // Wails application context, set at startup
	remote app.Remote
}

func newApp(opts Options) *App {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &App{opts: opts, log: log, ctx: context.Background()}
}

func (a *App) context() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ctx
}

func (a *App) setContext(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
}

// engine returns a live connection and the engine's self-description,
// connecting on first use and reconnecting (which respawns the engine)
// when the previous engine has gone away — e.g. after an idle exit.
func (a *App) engine(ctx context.Context) (app.Remote, *app.EngineInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.remote != nil {
		cctx, cancel := context.WithTimeout(ctx, engineCheckTimeout)
		info, err := a.remote.EngineInfo(cctx)
		cancel()
		if err == nil {
			return a.remote, info, nil
		}
		a.log.Warn("engine connection lost, reconnecting", "error", err)
		_ = a.remote.Close()
		a.remote = nil
	}
	if a.opts.Connect == nil {
		return nil, nil, app.E(app.CodeInternal, "桌面端未配置 Engine 连接器")
	}
	r, err := a.opts.Connect(ctx, app.ConnectOptions{DataDir: a.opts.DataDir, Requester: "desktop"})
	if err != nil {
		return nil, nil, err
	}
	info, err := r.EngineInfo(ctx)
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	a.remote = r
	return r, info, nil
}

// close drops the engine connection. The engine itself keeps running: closing
// the window only ends the desktop process.
func (a *App) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.remote != nil {
		_ = a.remote.Close()
		a.remote = nil
	}
}

// EngineInfo makes sure the engine is running and returns its base URL and
// bearer token; the frontend then calls the HTTP API directly.
func (a *App) EngineInfo() (EngineInfo, error) {
	r, info, err := a.engine(a.context())
	if err != nil {
		return EngineInfo{}, err
	}
	tp, ok := r.(TokenProvider)
	if !ok || tp.Token() == "" {
		return EngineInfo{}, app.E(app.CodeInternal, "Engine 连接未提供访问令牌")
	}
	base := r.BaseURL()
	if base == "" {
		base = info.BaseURL
	}
	version := a.opts.Version
	if version == "" {
		version = info.Version
	}
	return EngineInfo{BaseURL: strings.TrimRight(base, "/"), Token: tp.Token(), Version: version}, nil
}

// PickVideoFile shows the native open dialog filtered to video files and
// returns the absolute path, or "" when the user cancels.
func (a *App) PickVideoFile() (string, error) {
	if a.pickFile == nil {
		return "", app.E(app.CodeInternal, "文件选择框不可用")
	}
	p, err := a.pickFile(a.context())
	if err != nil {
		return "", app.E(app.CodeInternal, "无法打开文件选择框：%v", err).WithCause(err)
	}
	return p, nil
}

// SaveExport shows the native save dialog and copies the export from the
// engine to the chosen location. The desktop process writes the file itself
// because the engine only writes inside the data dir. It returns the
// written path, or "" when the user cancels.
func (a *App) SaveExport(exportID, suggestedName string) (string, error) {
	if exportID == "" {
		return "", app.E(app.CodeInvalidArgument, "缺少 export_id")
	}
	if a.saveFile == nil {
		return "", app.E(app.CodeInternal, "保存对话框不可用")
	}
	ctx := a.context()
	r, _, err := a.engine(ctx)
	if err != nil {
		return "", err
	}
	// Look the export up before bothering the user with a dialog; the stream
	// is reopened afterwards so that no connection idles while they choose.
	rc, meta, err := r.OpenExport(ctx, exportID)
	if err != nil {
		return "", err
	}
	rc.Close()

	name := sanitizeFileName(suggestedName)
	if name == "" && meta != nil {
		name = sanitizeFileName(meta.Filename)
	}
	if name == "" {
		name = "watchink-export"
	}
	dest, err := a.saveFile(ctx, name)
	if err != nil {
		return "", app.E(app.CodeInternal, "无法打开保存对话框：%v", err).WithCause(err)
	}
	if dest == "" {
		return "", nil
	}
	if !filepath.IsAbs(dest) {
		return "", app.E(app.CodeInternal, "保存对话框返回了非绝对路径")
	}

	rc, meta, err = r.OpenExport(ctx, exportID)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	if err := writeExport(dest, rc, meta); err != nil {
		return "", err
	}
	return dest, nil
}

// sanitizeFileName reduces a suggested name to a bare file name.
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, `\`, "/"))
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == ':' {
			return -1
		}
		return r
	}, name)
	if name == "." || name == ".." {
		return ""
	}
	return name
}

// writeExport streams src to dest atomically: a temp file in the target
// directory is verified against the export's size and SHA-256 and then
// renamed, so a failed download never leaves a truncated file behind.
func writeExport(dest string, src io.Reader, meta *app.Export) error {
	f, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*.tmp")
	if err != nil {
		return app.E(app.CodeInternal, "无法在目标位置创建文件：%v", err).WithCause(err).
			WithHint("请确认对该文件夹有写入权限")
	}
	tmp := f.Name()
	fail := func(e *app.Error) error {
		f.Close()
		os.Remove(tmp)
		return e
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), src)
	if err != nil {
		return fail(app.E(app.CodeInternal, "写入导出文件失败：%v", err).WithCause(err))
	}
	if meta != nil {
		if meta.Bytes > 0 && n != meta.Bytes {
			return fail(app.E(app.CodeInternal, "导出文件不完整：收到 %d 字节，应为 %d 字节", n, meta.Bytes))
		}
		if meta.SHA256 != "" && !strings.EqualFold(meta.SHA256, hex.EncodeToString(h.Sum(nil))) {
			return fail(app.E(app.CodeChecksumMismatch, "导出文件校验失败，请重试"))
		}
	}
	// CreateTemp uses 0600; an exported document is an ordinary user file.
	if err := f.Chmod(0o644); err != nil {
		return fail(app.E(app.CodeInternal, "写入导出文件失败：%v", err).WithCause(err))
	}
	if err := f.Sync(); err != nil {
		return fail(app.E(app.CodeInternal, "写入导出文件失败：%v", err).WithCause(err))
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return app.E(app.CodeInternal, "写入导出文件失败：%v", err).WithCause(err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return app.E(app.CodeInternal, "无法保存到 %s：%v", dest, err).WithCause(err)
	}
	return nil
}

// within reports whether path equals root or lies below it, comparing whole
// path components.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RevealInFileManager shows a file or folder in Finder (Explorer on Windows).
// Only paths that are inside the engine's data dir after resolving symlinks
// are accepted.
func (a *App) RevealInFileManager(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return app.E(app.CodeInvalidArgument, "路径必须是绝对路径")
	}
	if a.opts.Reveal == nil {
		return app.E(app.CodeInternal, "当前平台不支持在文件管理器中显示")
	}
	_, info, err := a.engine(a.context())
	if err != nil {
		return err
	}
	if info.DataDir == "" {
		return app.E(app.CodeInternal, "Engine 未报告数据目录")
	}
	root, err := filepath.EvalSymlinks(info.DataDir)
	if err != nil {
		return app.E(app.CodeInternal, "无法解析数据目录：%v", err).WithCause(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return app.E(app.CodeNotFound, "路径不存在：%s", path)
		}
		return app.E(app.CodePathNotAllowed, "无法访问该路径：%s", path)
	}
	if !within(root, resolved) {
		return app.E(app.CodePathNotAllowed, "只能显示数据目录内的文件：%s", path)
	}
	if err := a.opts.Reveal(resolved); err != nil {
		return app.E(app.CodeInternal, "无法在文件管理器中显示：%v", err).WithCause(err)
	}
	return nil
}

// OpenExternal opens an http or https URL in the default browser.
func (a *App) OpenExternal(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return app.E(app.CodeInvalidArgument, "只能打开 http 或 https 链接")
	}
	if a.openURL == nil {
		return app.E(app.CodeInternal, "无法打开外部链接")
	}
	a.openURL(a.context(), u.String())
	return nil
}

// AgentSetupInfo returns what a user needs to connect an MCP client: the
// absolute executable path, the stdio server command, and config snippets.
//
// Snippet formats follow the client docs: Claude Code's
// `claude mcp add [options] <name> -- <command> [args...]`, and the
// `mcpServers` object of Claude Desktop's claude_desktop_config.json (the
// same shape as a project .mcp.json).
func (a *App) AgentSetupInfo() (AgentSetup, error) {
	exe := a.opts.Executable
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return AgentSetup{}, app.E(app.CodeInternal, "无法确定可执行文件路径：%v", err).WithCause(err)
		}
	}
	exe, err := filepath.Abs(exe)
	if err != nil {
		return AgentSetup{}, app.E(app.CodeInternal, "无法确定可执行文件路径：%v", err).WithCause(err)
	}
	exe = agentExecutable(exe)
	cmd := []string{exe, "mcp"}

	cfg := map[string]any{
		"mcpServers": map[string]any{
			"watchink": map[string]any{"command": cmd[0], "args": cmd[1:]},
		},
	}
	js, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return AgentSetup{}, app.E(app.CodeInternal, "无法生成配置片段").WithCause(err)
	}
	return AgentSetup{
		Executable: exe,
		MCPCommand: cmd,
		Snippets: []Snippet{
			{Name: "Claude Code", Language: "bash", Content: "claude mcp add watchink -- " + shellQuote(exe) + " mcp"},
			{Name: "Claude Desktop", Language: "json", Content: string(js)},
		},
	}, nil
}

// shellQuote quotes s for the user's shell when it contains anything but
// plain path characters: a POSIX shell, or cmd.exe / PowerShell on Windows.
func shellQuote(s string) string {
	if runtime.GOOS == "windows" {
		return windowsQuote(s)
	}
	safe := s != ""
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("/._-+=:@%", r):
		default:
			safe = false
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// formatError is the Wails ErrorFormatter: every binding failure reaches
// JavaScript as the JSON text of an app.Error. The Wails v2 runtime
// rejects the promise with `new Error(<that text>)`, so the frontend reads it
// with JSON.parse(err.message). It must stay a string: an object would be
// stringified to "[object Object]" by that wrapper.
func formatError(err error) any {
	if err == nil {
		return nil
	}
	b, mErr := json.Marshal(app.AsError(err))
	if mErr != nil {
		return `{"code":"INTERNAL","message":"internal error","retryable":true,"user_action_required":false}`
	}
	return string(b)
}
