// Package procx is the only package allowed to create child processes.
// It runs external tools from an argv array (never
// through a shell), puts every child in its own process group (a Job Object on
// Windows) so that a cancel reaches the whole tree, keeps the last 64 KiB of
// stderr (redacted) for error details, and — on macOS — wraps children in the
// hidden `watchink _guard` subcommand so they cannot outlive a crashed engine.
package procx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"watchink/internal/app"
)

const (
	// StderrTailBytes is how much of a child's stderr is kept.
	StderrTailBytes = 64 << 10
	// DefaultTermGrace is the delay between SIGTERM and SIGKILL.
	DefaultTermGrace = 3 * time.Second
	// GuardSubcommand is the hidden CLI subcommand that runs GuardMain.
	GuardSubcommand = "_guard"

	// pipeWaitDelay bounds how long Run waits for the output pipes after the
	// child has exited; a descendant that escaped the process group could
	// otherwise keep them open forever.
	pipeWaitDelay = 2 * time.Second
	maxLineBytes  = 64 << 10
)

// ErrKilled is the cause of the error returned by Run when the child was
// force-killed through KillAll or KillJob rather than by its context.
var ErrKilled = errors.New("procx: child force-killed")

// Options configures a Runner.
type Options struct {
	// GuardExe is the path of the watchink executable. When set, every
	// child is started as `<GuardExe> _guard -- <path> <args...>` so that it
	// dies with the engine (macOS). When empty the command is started
	// directly, which is what tests use.
	GuardExe string
	// OnStart is called after a child has started. pid is the process-group
	// leader started by the Runner and exe is that process's executable: the
	// tool itself, or GuardExe when a guard is used.
	OnStart func(pid int, exe, jobID, stage string)
	// OnExit is called once the child has been reaped and its group is gone.
	OnExit func(pid int)
	// TermGrace overrides DefaultTermGrace (tests).
	TermGrace time.Duration
	// Secrets are always masked in stderr tails, besides the generic patterns.
	Secrets []string
}

// Cmd describes one child process.
type Cmd struct {
	// Path is the executable; a bare name is looked up in PATH.
	Path string
	// Args are the arguments, without argv[0].
	Args []string
	// Dir is the working directory ("" = the engine's).
	Dir string
	// Env is the full environment; nil inherits the engine's environment.
	Env []string
	// Stdin is the child's standard input; nil means /dev/null.
	Stdin io.Reader
	// Stdout and Stderr optionally receive the raw output.
	Stdout io.Writer
	Stderr io.Writer
	// OnStdoutLine / OnStderrLine are called for every output line (split on
	// "\n" or "\r", without the terminator, empty lines skipped). The two
	// callbacks run on different goroutines; each one is called sequentially.
	// Lines are NOT redacted: parse them, do not log them.
	OnStdoutLine func(line string)
	OnStderrLine func(line string)
	// JobID and Stage label the child for the registry and for errors. Empty
	// values are filled from the context (see WithLabels).
	JobID string
	Stage string
	// Secrets are masked in this command's stderr tail.
	Secrets []string
}

// Result is the outcome of a finished child.
type Result struct {
	// PID is the process started by the Runner (the guard when one is used).
	PID int
	// ExitCode is the exit status, or -1 when the child died from a signal.
	ExitCode int
	// StderrTail is the last 64 KiB of stderr, redacted.
	StderrTail string
	// Duration is the wall time of the run.
	Duration time.Duration
}

type labelsKey struct{}

type labels struct{ jobID, stage string }

// WithLabels returns a context carrying the job id and stage of the work in
// progress. Run uses them for commands whose JobID / Stage are empty, so
// packages that build commands (media, asr) do not need to know about jobs.
func WithLabels(ctx context.Context, jobID, stage string) context.Context {
	return context.WithValue(ctx, labelsKey{}, labels{jobID, stage})
}

// Runner starts child processes and tracks the live ones.
type Runner struct {
	opts Options

	mu       sync.Mutex
	children map[int]*child
}

type child struct {
	pid    int
	jobID  string
	killed bool
}

// New returns a Runner.
func New(opts Options) *Runner {
	if opts.TermGrace <= 0 {
		opts.TermGrace = DefaultTermGrace
	}
	return &Runner{opts: opts, children: map[int]*child{}}
}

// Run starts the command and waits for it.
//
//   - Exit code 0: (result, nil).
//   - Non-zero exit: (result, *app.Error TOOL_FAILED) with exit_code and
//     stderr_tail in the details; the result is still returned so callers can
//     classify the stderr themselves.
//   - ctx cancelled: the process group gets SIGTERM, then SIGKILL after the
//     grace period; the error wraps ctx.Err().
//   - Executable missing: *app.Error TOOL_MISSING.
func (r *Runner) Run(ctx context.Context, c Cmd) (*Result, error) {
	name := filepath.Base(c.Path)
	if l, ok := ctx.Value(labelsKey{}).(labels); ok {
		if c.JobID == "" {
			c.JobID = l.jobID
		}
		if c.Stage == "" {
			c.Stage = l.stage
		}
	}
	if c.Path == "" {
		return nil, app.E(app.CodeInternal, "procx: 未指定可执行文件").WithStage(c.Stage)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("procx: %s: %w", name, err)
	}

	cmd, startExe, err := r.command(c, name)
	if err != nil {
		return nil, err
	}
	out := wireOutput(cmd, c)
	started := time.Now()
	ch, err := r.start(cmd, c, name, startExe)
	if err != nil {
		return nil, err
	}
	waitErr, cancelled := r.wait(ctx, cmd, ch.pid)
	killed := r.forget(ch)
	out.flush()

	secrets := append(append([]string{}, r.opts.Secrets...), c.Secrets...)
	res := &Result{
		PID:        ch.pid,
		ExitCode:   cmd.ProcessState.ExitCode(),
		StderrTail: Redact(out.tail.String(), secrets...),
		Duration:   time.Since(started),
	}
	if cancelled || ctx.Err() != nil {
		return res, fmt.Errorf("procx: %s: %w", name, ctx.Err())
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		// The child itself finished; only an inherited pipe was still open.
		waitErr = nil
	}
	if res.ExitCode == 0 && waitErr == nil {
		return res, nil
	}
	e := app.E(app.CodeToolFailed, "外部工具 %s 执行失败（退出码 %d）", name, res.ExitCode).
		WithStage(c.Stage).
		WithDetail("tool", name).
		WithDetail("exit_code", res.ExitCode).
		WithDetail("stderr_tail", res.StderrTail)
	switch {
	case killed:
		e = e.WithCause(ErrKilled)
	case waitErr != nil:
		e = e.WithCause(waitErr)
	}
	return res, e
}

// command resolves the tool and builds the process to start: the tool
// itself, or the guard wrapping it when one is configured. startExe is the
// program that is actually executed.
func (r *Runner) command(c Cmd, name string) (cmd *exec.Cmd, startExe string, err error) {
	exe, err := exec.LookPath(c.Path)
	if err != nil {
		return nil, "", app.E(app.CodeToolMissing, "找不到外部工具 %s", name).
			WithStage(c.Stage).WithDetail("tool", name).WithDetail("path", c.Path).WithCause(err)
	}
	startExe, argv := exe, c.Args
	if r.opts.GuardExe != "" {
		startExe = r.opts.GuardExe
		argv = append([]string{GuardSubcommand, "--", exe}, c.Args...)
	}
	cmd = exec.Command(startExe, argv...) // argv array, never a shell
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin = c.Stdin
	cmd.WaitDelay = pipeWaitDelay
	setProcessGroup(cmd)
	return cmd, startExe, nil
}

// output is where a child's stdout and stderr go while it runs.
type output struct {
	tail     *tailBuffer // the end of stderr, kept for the error message
	outLines *lineWriter
	errLines *lineWriter
}

// flush delivers a last line that did not end with a newline.
func (o *output) flush() {
	if o.outLines != nil {
		o.outLines.flush()
	}
	if o.errLines != nil {
		o.errLines.flush()
	}
}

// wireOutput connects the child's streams to the caller's writers and line
// callbacks; stderr is always also kept in the tail buffer.
func wireOutput(cmd *exec.Cmd, c Cmd) *output {
	o := &output{tail: newTailBuffer(StderrTailBytes)}
	var outs []io.Writer
	if c.Stdout != nil {
		outs = append(outs, c.Stdout)
	}
	if c.OnStdoutLine != nil {
		o.outLines = &lineWriter{fn: c.OnStdoutLine}
		outs = append(outs, o.outLines)
	}
	errs := []io.Writer{o.tail}
	if c.Stderr != nil {
		errs = append(errs, c.Stderr)
	}
	if c.OnStderrLine != nil {
		o.errLines = &lineWriter{fn: c.OnStderrLine}
		errs = append(errs, o.errLines)
	}
	if len(outs) > 0 {
		cmd.Stdout = fanOut(outs)
	}
	cmd.Stderr = fanOut(errs)
	return o
}

// start launches the process, puts it in its group and registers it so that
// KillJob / KillAll and the OnStart hook can see it.
func (r *Runner) start(cmd *exec.Cmd, c Cmd, name, startExe string) (*child, error) {
	failed := func(code app.Code, err error) error {
		return app.E(code, "无法启动外部工具 %s", name).
			WithStage(c.Stage).WithDetail("tool", name).WithDetail("error", Redact(err.Error())).WithCause(err)
	}
	if err := cmd.Start(); err != nil {
		code := app.CodeToolFailed
		if errors.Is(err, os.ErrNotExist) {
			code = app.CodeToolMissing
		}
		return nil, failed(code, err)
	}
	pid := cmd.Process.Pid
	// Platforms whose process group is a separate kernel object (the Windows
	// Job Object) create it here; elsewhere this is a no-op.
	if err := attachGroup(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, failed(app.CodeToolFailed, err)
	}
	ch := &child{pid: pid, jobID: c.JobID}
	r.mu.Lock()
	r.children[pid] = ch
	r.mu.Unlock()
	if r.opts.OnStart != nil {
		r.opts.OnStart(pid, startExe, c.JobID, c.Stage)
	}
	return ch, nil
}

// wait blocks until the process exits. When ctx is cancelled first it asks
// the whole group to terminate, escalates to a kill after the grace period,
// and reports cancelled. In every case nothing of the group is left running
// when it returns.
func (r *Runner) wait(ctx context.Context, cmd *exec.Cmd, pid int) (waitErr error, cancelled bool) {
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case waitErr = <-waitCh:
	case <-ctx.Done():
		cancelled = true
		termAt := time.Now()
		_ = terminateGroup(pid)
		timer := time.NewTimer(r.opts.TermGrace)
		select {
		case waitErr = <-waitCh:
			timer.Stop()
		case <-timer.C:
			_ = killGroup(pid)
			waitErr = <-waitCh
		}
		// The leader is gone, but descendants may still be finishing their
		// own SIGTERM handling: give them the rest of the grace period.
		for groupAlive(pid) && time.Since(termAt) < r.opts.TermGrace {
			time.Sleep(20 * time.Millisecond)
		}
	}
	// Whatever is left in the group (background grandchildren, or processes
	// ignoring SIGTERM) must not outlive the stage.
	_ = killGroup(pid)
	return waitErr, cancelled
}

// forget unregisters a finished child and reports whether KillJob / KillAll
// had killed it.
func (r *Runner) forget(ch *child) (killed bool) {
	r.mu.Lock()
	killed = ch.killed
	delete(r.children, ch.pid)
	r.mu.Unlock()
	releaseGroup(ch.pid)
	if r.opts.OnExit != nil {
		r.opts.OnExit(ch.pid)
	}
	return killed
}

// KillAll force-kills (SIGKILL) the process group of every live child. The
// corresponding Run calls return a TOOL_FAILED error whose cause is ErrKilled
// (or the context error when their context is cancelled as well).
func (r *Runner) KillAll() {
	r.killWhere(func(*child) bool { return true })
}

// KillJob force-kills the process groups of the children labelled with jobID.
func (r *Runner) KillJob(jobID string) {
	r.killWhere(func(c *child) bool { return c.jobID == jobID })
}

func (r *Runner) killWhere(match func(*child) bool) {
	r.mu.Lock()
	var pids []int
	for _, c := range r.children {
		if match(c) {
			c.killed = true
			pids = append(pids, c.pid)
		}
	}
	r.mu.Unlock()
	for _, pid := range pids {
		_ = killGroup(pid)
	}
}

// Live returns the pids of the children that are currently running.
func (r *Runner) Live() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	pids := make([]int, 0, len(r.children))
	for pid := range r.children {
		pids = append(pids, pid)
	}
	return pids
}

// fanOut is io.MultiWriter that never reports a short write or an error: a
// failing consumer must not make os/exec abort the copy and stall the child.
type fanOut []io.Writer

func (f fanOut) Write(p []byte) (int, error) {
	for _, w := range f {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return len(p), nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return len(p), nil
}

// String returns the tail as valid UTF-8: a multi-byte character cut by the
// truncation is dropped rather than turned into garbage.
func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buf
	for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.RuneStart(b[0]); i++ {
		b = b[1:]
	}
	return strings.ToValidUTF8(string(b), "�")
}

// lineWriter splits a byte stream into lines for a callback. Both "\n" and
// "\r" end a line because tools redraw progress with carriage returns.
type lineWriter struct {
	fn  func(string)
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' || b == '\r' {
			w.flush()
			continue
		}
		w.buf = append(w.buf, b)
		if len(w.buf) >= maxLineBytes {
			w.flush()
		}
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) == 0 {
		return
	}
	line := strings.ToValidUTF8(string(w.buf), "�")
	w.buf = w.buf[:0]
	w.fn(line)
}
