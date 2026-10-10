// Package cli builds the cobra command tree of docs/v0.md §13.4.
//
// The package talks to the engine exclusively through an injected
// app.Connector; everything that needs another package (running the engine,
// the desktop window, the MCP server, resolving the data directory) is
// injected through Deps, so the tree is fully testable with a fake app.Remote.
package cli

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"watchink/internal/app"
	"watchink/internal/version"
)

// ServeOptions is passed to Deps.RunServe (`watchink serve`).
type ServeOptions struct {
	// DataDir is the resolved data directory (see Deps.ResolveDataDir).
	DataDir string
	// Port is the --port flag; 0 means the configured engine port.
	Port int
	// Open asks the foreground engine to open the Web UI once it listens.
	Open bool
}

// MCPOptions is passed to Deps.RunMCP (`watchink mcp`).
type MCPOptions struct {
	// DataDir is the resolved data directory (see Deps.ResolveDataDir).
	DataDir string
	// NoSpawn mirrors the global --no-spawn flag.
	NoSpawn bool
}

// Deps carries everything the command tree needs from the rest of the
// program. Nil function fields disable the matching command with a clear
// error instead of panicking.
type Deps struct {
	// Connect connects to (and by default spawns) the engine (§5.2).
	Connect app.Connector

	// RunDesktop opens the desktop window; nil in nogui builds, where the
	// bare command prints help and `desktop` fails with a clear message.
	RunDesktop func(ctx context.Context, dataDir string) error
	// RunServe runs the engine in the foreground (`serve`).
	RunServe func(ctx context.Context, opts ServeOptions) error
	// RunEngine runs the background engine (hidden `engine run`).
	RunEngine func(ctx context.Context, dataDir string) error
	// RunMCP runs the MCP stdio server (`mcp`).
	RunMCP func(ctx context.Context, opts MCPOptions) error
	// Guard implements the hidden `_guard` command. It receives every
	// argument after `_guard` untouched (no flag parsing) and returns the
	// process exit code.
	Guard func(args []string) int

	// OpenBrowser opens a URL in the default browser (`web`).
	OpenBrowser func(url string) error
	// ResolveDataDir resolves the --data-dir flag value (possibly empty) to
	// the effective absolute data directory. When nil the flag value is
	// used verbatim.
	ResolveDataDir func(flag string) (string, error)
	// SetDataDir persists a new data directory in location.toml
	// (`config set-data-dir`).
	SetDataDir func(dir string) error
	// IsEngineRunning reports whether an engine currently owns dataDir. It
	// must not spawn one.
	IsEngineRunning func(dataDir string) bool
	// DesktopPrereqs lists the system libraries the desktop window needs on
	// this platform and whether each was found (`doctor`, §16.3). It is
	// optional; nil or an empty result means there is nothing to check, as on
	// macOS, which ships its WebView with the system.
	DesktopPrereqs func() []SystemPrereq

	// Stdin is the input for secrets and interactive setup; nil means no
	// input is available.
	Stdin io.Reader
	// IsTerminal reports whether Stdin is an interactive terminal.
	IsTerminal func() bool
	// ReadPassword reads one line from the terminal without echo. It is
	// optional: when nil, interactive secret prompts read a plain line from
	// Stdin (the terminal then echoes what is typed).
	ReadPassword func() (string, error)

	// Version and BuildID default to internal/version when empty.
	Version string
	BuildID string
}

// requester is recorded as jobs.requested_by for everything the CLI creates.
const requester = "cli"

// env is the per-invocation state shared by all commands: the injected
// dependencies plus the parsed global flags.
type env struct {
	deps Deps

	dataDir  string
	jsonMode bool
	verbose  bool
	noSpawn  bool
	timeout  durationFlag
}

// New builds the command tree. Commands return *app.Error (or an internal
// failure carrying an explicit exit code); use Execute to get the §13.4
// output contract and exit codes.
func New(deps Deps) *cobra.Command {
	root, _ := newRoot(deps)
	return root
}

func newRoot(deps Deps) (*cobra.Command, *env) {
	if deps.Version == "" {
		deps.Version = version.Version
	}
	if deps.BuildID == "" {
		deps.BuildID = version.BuildID
	}
	e := &env{deps: deps}

	root := &cobra.Command{
		Use:   "watchink",
		Short: "Watchink：把讲座视频变成带时间引用的笔记",
		Long: "Watchink 把本地视频或在线视频转写成字幕，并生成带时间引用的笔记。\n" +
			"不带参数运行时打开桌面窗口；其余子命令是连接本机 Engine 的命令行客户端。",
		SilenceUsage:  true,
		SilenceErrors: true,
		// The root deliberately has no Args validator: cobra then reports an
		// unknown first argument as "unknown command", a usage error.
		RunE: func(cmd *cobra.Command, args []string) error {
			if deps.RunDesktop == nil {
				// nogui build (§4.2): the bare command prints help.
				return cmd.Help()
			}
			return e.runDesktop(cmd)
		},
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&e.dataDir, "data-dir", "", "数据目录（默认按环境变量、location.toml、平台默认位置依次确定）")
	pf.BoolVar(&e.jsonMode, "json", false, "stdout 只输出一个 JSON 值")
	pf.BoolVar(&e.verbose, "verbose", false, "在 stderr 输出更多过程信息")
	pf.BoolVar(&e.noSpawn, "no-spawn", false, "Engine 未运行时不自动拉起")
	pf.Var(&e.timeout, "timeout", "连接 Engine 和单次请求的超时，如 30s、2m 或秒数（默认 15s 连接、请求不限时）")

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err) })
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if !e.jsonMode {
			defaultHelp(cmd, args)
			return
		}
		// §13.4: with --json stdout carries exactly one JSON value, so help
		// text is wrapped instead of printed raw.
		_ = writeJSON(cmd.OutOrStdout(), map[string]string{"help": cmd.UsageString()})
	})

	root.AddCommand(
		e.cmdDesktop(), e.cmdServe(), e.cmdWeb(), e.cmdMCP(), e.cmdGuard(),
		e.cmdSetup(), e.cmdImport(), e.cmdJobs(), e.cmdVideos(), e.cmdNote(),
		e.cmdTranscript(), e.cmdExport(), e.cmdConfig(), e.cmdLLM(),
		e.cmdTools(), e.cmdEngine(), e.cmdDoctor(), e.cmdVersion(),
	)
	return root, e
}

// Execute runs the CLI with args (without the program name), writes results
// to stdout and diagnostics to stderr following the §13.4 output contract,
// and returns the process exit code. It never calls os.Exit.
func Execute(ctx context.Context, deps Deps, args []string, stdout, stderr io.Writer) (exitCode int) {
	// `_guard` is dispatched before cobra sees anything so that its
	// arguments reach Guard byte-for-byte.
	if len(args) > 0 && args[0] == guardCommand {
		if deps.Guard == nil {
			return finish(app.E(app.CodeInvalidArgument, "此构建不支持 %s 命令", guardCommand), scanJSONFlag(args[:1]), stdout, stderr)
		}
		return deps.Guard(args[1:])
	}

	root, e := newRoot(deps)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if deps.Stdin != nil {
		root.SetIn(deps.Stdin)
	} else {
		root.SetIn(strings.NewReader(""))
	}
	err := root.ExecuteContext(ctx)
	// When flag parsing itself failed, --json may not have been recorded.
	return finish(err, e.jsonMode || scanJSONFlag(args), stdout, stderr)
}

// scanJSONFlag looks for --json among raw arguments (before a "--").
func scanJSONFlag(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "--json", "--json=true":
			return true
		}
	}
	return false
}

// failure is an error with an explicit exit code and optional extra JSON
// members next to "error" (e.g. the job a --wait gave up on).
type failure struct {
	err   *app.Error
	exit  int
	extra map[string]any
	// silent suppresses the error output: the command already wrote what
	// the contract requires (NDJSON follow mode).
	silent bool
}

func (f *failure) Error() string { return f.err.Error() }

// usageError marks err as a usage problem: INVALID_ARGUMENT, exit code 2.
func usageError(err error) error {
	var ae *app.Error
	if errors.As(err, &ae) {
		return err
	}
	return app.E(app.CodeInvalidArgument, "%s", err.Error()).
		WithHint("加 --help 查看该命令的用法")
}

// finish converts the error returned by the command tree into output and an
// exit code (§13.4).
func finish(err error, jsonMode bool, stdout, stderr io.Writer) int {
	if err == nil {
		return app.ExitOK
	}
	var f *failure
	if !errors.As(err, &f) {
		var ae *app.Error
		if errors.As(err, &ae) {
			f = &failure{err: ae, exit: app.ExitCode(ae.Code)}
		} else {
			// Anything else comes from cobra itself (unknown command,
			// wrong argument count): a usage error.
			ae = app.AsError(usageError(err))
			f = &failure{err: ae, exit: app.ExitUsage}
		}
	}
	if f.silent {
		return f.exit
	}
	if jsonMode {
		out := map[string]any{"error": f.err}
		for k, v := range f.extra {
			out[k] = v
		}
		_ = writeJSON(stdout, out)
		return f.exit
	}
	printHumanError(stderr, f.err)
	return f.exit
}

// durationFlag accepts Go durations ("30s") and bare seconds ("30").
type durationFlag time.Duration

func (d *durationFlag) String() string {
	if *d == 0 {
		return ""
	}
	return time.Duration(*d).String()
}

func (d *durationFlag) Type() string { return "duration" }

func (d *durationFlag) Set(s string) error {
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		if n < 0 {
			return errors.New("超时不能为负数")
		}
		*d = durationFlag(time.Duration(n * float64(time.Second)))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return errors.New("无法解析时长，应形如 30s、2m 或秒数")
	}
	*d = durationFlag(v)
	return nil
}

// call is the per-command context handed to command bodies.
type call struct {
	*env
	cmd  *cobra.Command
	ctx  context.Context
	args []string
	out  io.Writer
	// errw is the raw stderr; progress is where progress and log lines go
	// (discarded with --json unless --verbose, §13.4).
	errw     io.Writer
	progress io.Writer
}

// run adapts a command body to cobra, normalizing every returned error to
// *app.Error or *failure.
func (e *env) run(fn func(c *call) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		c := &call{env: e, cmd: cmd, ctx: cmd.Context(), args: args, out: cmd.OutOrStdout(), errw: cmd.ErrOrStderr()}
		if c.ctx == nil {
			c.ctx = context.Background()
		}
		c.progress = c.errw
		if e.jsonMode && !e.verbose {
			c.progress = io.Discard
		}
		err := fn(c)
		if err == nil {
			return nil
		}
		var f *failure
		if errors.As(err, &f) {
			return f
		}
		return app.AsError(err)
	}
}

// connect opens the engine connection. forceNoSpawn is set by commands that
// must never start an engine (`engine status`, `engine stop`).
func (c *call) connect(forceNoSpawn bool) (app.Remote, error) {
	if c.deps.Connect == nil {
		return nil, app.E(app.CodeEngineUnavailable, "此构建没有可用的 Engine 连接方式")
	}
	if c.verbose {
		c.logf("正在连接 Engine…")
	}
	return c.deps.Connect(c.ctx, app.ConnectOptions{
		DataDir: c.dataDir,
		NoSpawn: c.noSpawn || forceNoSpawn,
		// The same commands must also reach an engine of another build:
		// stopping it is how a version mismatch is resolved (§5.2).
		AnyBuild:  forceNoSpawn,
		Timeout:   time.Duration(c.timeout),
		Requester: requester,
	})
}

// withRemote connects, runs fn and closes the connection.
func (c *call) withRemote(fn func(r app.Remote) error) error {
	r, err := c.connect(false)
	if err != nil {
		return err
	}
	defer r.Close()
	return fn(r)
}

// reqCtx bounds one plain request with --timeout when it was given.
func (c *call) reqCtx() (context.Context, context.CancelFunc) {
	if c.timeout > 0 {
		return context.WithTimeout(c.ctx, time.Duration(c.timeout))
	}
	return context.WithCancel(c.ctx)
}

// resolveDataDir returns the effective data directory for commands that
// work on it directly.
func (c *call) resolveDataDir() (string, error) {
	if c.deps.ResolveDataDir == nil {
		return c.dataDir, nil
	}
	dir, err := c.deps.ResolveDataDir(c.dataDir)
	if err != nil {
		return "", app.AsError(err)
	}
	return dir, nil
}

// Argument validators returning usage errors (INVALID_ARGUMENT, exit 2).

func exactArgs(n int, usage string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return app.E(app.CodeInvalidArgument, "参数个数不对：需要 %d 个，实际 %d 个", n, len(args)).
				WithHint("用法：watchink " + usage)
		}
		return nil
	}
}

func noArgs(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return app.E(app.CodeInvalidArgument, "该命令不接受位置参数").WithHint("加 --help 查看该命令的用法")
	}
	return nil
}

// group builds a parent command: it prints help when called bare and
// rejects unknown subcommands as usage errors.
func group(use, short string, subs ...*cobra.Command) *cobra.Command {
	g := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return app.E(app.CodeInvalidArgument, "未知的子命令 %q", args[0]).
					WithHint("运行 `" + cmd.CommandPath() + " --help` 查看可用的子命令")
			}
			return cmd.Help()
		},
	}
	g.AddCommand(subs...)
	return g
}

func oneOf(flag, value string, allowed ...string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return app.E(app.CodeInvalidArgument, "--%s 的取值无效", flag).
		WithHint("可选值："+strings.Join(allowed, " / ")).WithDetail("allowed", allowed)
}
