//go:build darwin

package procx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"watchink/internal/app"
)

const helperEnv = "PROCX_TEST_HELPER"

// TestMain lets the test binary play two extra roles (re-exec pattern):
//
//	<test binary> _guard -- cmd...        the guard, exactly as the engine runs it
//	PROCX_TEST_HELPER=orphan <test binary> start a guarded tree, print the guard
//	                                      pid and die, leaving the guard orphaned
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == GuardSubcommand {
		os.Exit(GuardMain(os.Args[2:]))
	}
	if os.Getenv(helperEnv) == "orphan" {
		self, _ := os.Executable()
		cmd := exec.Command(self, GuardSubcommand, "--", "/bin/sh", "-c", "sleep 300 & sleep 300 & wait")
		cmd.Env = append(os.Environ(), helperEnv+"=")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(cmd.Process.Pid)
		time.Sleep(400 * time.Millisecond) // let the shell start its sleeps
		os.Exit(0)                         // die without waiting: the guard is now an orphan
	}
	os.Exit(m.Run())
}

func pidAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

func waitGone(t *testing.T, what string, alive func() bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for alive() {
		if time.Now().After(deadline) {
			t.Fatalf("%s still alive after %v", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pidCollector gathers the pids a test shell prints with `echo $!`.
type pidCollector struct {
	mu   sync.Mutex
	pids []int
	got  chan struct{}
	want int
}

func newPidCollector(want int) *pidCollector {
	return &pidCollector{got: make(chan struct{}), want: want}
}

func (p *pidCollector) line(s string) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pids = append(p.pids, n)
	if len(p.pids) == p.want {
		close(p.got)
	}
}

func (p *pidCollector) wait(t *testing.T) []int {
	t.Helper()
	select {
	case <-p.got:
	case <-time.After(10 * time.Second):
		t.Fatal("child tree did not report its pids")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.pids...)
}

func TestRunSuccessAndCallbacks(t *testing.T) {
	var started, exited []int
	var gotExe, gotJob, gotStage string
	r := New(Options{
		OnStart: func(pid int, exe, jobID, stage string) {
			started = append(started, pid)
			gotExe, gotJob, gotStage = exe, jobID, stage
		},
		OnExit: func(pid int) { exited = append(exited, pid) },
	})
	var out, errLines []string
	var raw strings.Builder
	res, err := r.Run(context.Background(), Cmd{
		Path:         "/bin/sh",
		Args:         []string{"-c", `read x; echo "in:$x"; echo "v=$WK_VAR"; pwd; echo warn 1>&2`},
		Dir:          "/usr",
		Env:          []string{"WK_VAR=42"},
		Stdin:        strings.NewReader("hello\n"),
		Stdout:       &raw,
		OnStdoutLine: func(s string) { out = append(out, s) },
		OnStderrLine: func(s string) { errLines = append(errLines, s) },
		JobID:        "job_1",
		Stage:        "probe",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || res.PID <= 0 {
		t.Errorf("result = %+v", res)
	}
	if want := "in:hello|v=42|/usr"; strings.Join(out, "|") != want {
		t.Errorf("stdout lines = %q, want %q", out, want)
	}
	if raw.String() != "in:hello\nv=42\n/usr\n" {
		t.Errorf("raw stdout = %q", raw.String())
	}
	if strings.Join(errLines, "|") != "warn" || res.StderrTail != "warn\n" {
		t.Errorf("stderr lines = %q, tail = %q", errLines, res.StderrTail)
	}
	if len(started) != 1 || len(exited) != 1 || started[0] != res.PID || exited[0] != res.PID {
		t.Errorf("hooks: started=%v exited=%v pid=%d", started, exited, res.PID)
	}
	if gotExe != "/bin/sh" || gotJob != "job_1" || gotStage != "probe" {
		t.Errorf("OnStart labels = %q %q %q", gotExe, gotJob, gotStage)
	}
	if len(r.Live()) != 0 {
		t.Errorf("Live() = %v after exit", r.Live())
	}
}

func TestRunNonZeroExit(t *testing.T) {
	r := New(Options{Secrets: []string{"topsecretvalue"}})
	res, err := r.Run(context.Background(), Cmd{
		Path:  "/bin/sh",
		Args:  []string{"-c", `echo "boom topsecretvalue Authorization: Bearer abcdef123" 1>&2; exit 7`},
		Stage: "download",
	})
	if !app.IsCode(err, app.CodeToolFailed) {
		t.Fatalf("err = %v, want TOOL_FAILED", err)
	}
	e := app.AsError(err)
	if e.Stage != "download" || e.Details["exit_code"] != 7 {
		t.Errorf("error = %+v", e)
	}
	tail, _ := e.Details["stderr_tail"].(string)
	if tail != "boom *** Authorization: Bearer ***\n" {
		t.Errorf("stderr_tail = %q", tail)
	}
	if res == nil || res.ExitCode != 7 || res.StderrTail != tail {
		t.Errorf("result = %+v", res)
	}
}

func TestRunMissingTool(t *testing.T) {
	r := New(Options{})
	_, err := r.Run(context.Background(), Cmd{Path: "/nonexistent/lt-tool"})
	if !app.IsCode(err, app.CodeToolMissing) {
		t.Fatalf("err = %v, want TOOL_MISSING", err)
	}
}

func TestRunStderrTailTruncation(t *testing.T) {
	r := New(Options{})
	// ~200 KiB of stderr followed by a marker: only the last 64 KiB survive.
	script := `i=0; while [ $i -lt 2000 ]; do printf '%0100d\n' $i 1>&2; i=$((i+1)); done; echo TAIL-MARKER 1>&2; exit 3`
	res, err := r.Run(context.Background(), Cmd{Path: "/bin/sh", Args: []string{"-c", script}})
	if !app.IsCode(err, app.CodeToolFailed) {
		t.Fatalf("err = %v", err)
	}
	if len(res.StderrTail) != StderrTailBytes {
		t.Errorf("tail length = %d, want %d", len(res.StderrTail), StderrTailBytes)
	}
	if !strings.HasSuffix(res.StderrTail, "TAIL-MARKER\n") {
		t.Errorf("tail does not end with the marker: %q", res.StderrTail[len(res.StderrTail)-40:])
	}
	if strings.Contains(res.StderrTail, fmt.Sprintf("%0100d", 0)) {
		t.Error("tail still contains the first line")
	}
}

func testCancelKillsTree(t *testing.T, r *Runner, script string, maxWait time.Duration) {
	t.Helper()
	pc := newPidCollector(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		res *Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := r.Run(ctx, Cmd{Path: "/bin/sh", Args: []string{"-c", script}, OnStdoutLine: pc.line, JobID: "job_c"})
		done <- outcome{res, err}
	}()
	pids := pc.wait(t)
	for _, pid := range pids {
		if !pidAlive(pid) {
			t.Fatalf("grandchild %d not running before cancel", pid)
		}
	}

	begin := time.Now()
	cancel()
	var o outcome
	select {
	case o = <-done:
	case <-time.After(maxWait + 5*time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if took := time.Since(begin); took > maxWait {
		t.Errorf("cancel took %v, want <= %v", took, maxWait)
	}
	if !errors.Is(o.err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", o.err)
	}
	for _, pid := range pids {
		pid := pid
		waitGone(t, fmt.Sprintf("grandchild %d", pid), func() bool { return pidAlive(pid) }, time.Second)
	}
	if o.res != nil && groupAlive(o.res.PID) {
		t.Errorf("process group %d still has members", o.res.PID)
	}
}

const treeScript = `sleep 300 & echo $!; sleep 300 & echo $!; wait`

func TestCancelKillsProcessTree(t *testing.T) {
	// SIGTERM is enough here, so this must finish well inside the 3 s grace.
	testCancelKillsTree(t, New(Options{}), treeScript, 2*time.Second)
}

func TestCancelEscalatesToSIGKILL(t *testing.T) {
	// The whole tree ignores SIGTERM (an ignored signal is inherited across
	// exec), so only the SIGKILL after the grace period can stop it.
	r := New(Options{TermGrace: 300 * time.Millisecond})
	testCancelKillsTree(t, r, `trap '' TERM; `+treeScript, 3*time.Second)
}

func TestCancelThroughGuard(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var startedExe string
	r := New(Options{GuardExe: self, OnStart: func(_ int, exe, _, _ string) { startedExe = exe }})
	testCancelKillsTree(t, r, treeScript, 2*time.Second)
	if startedExe != self {
		t.Errorf("OnStart exe = %q, want the guard %q", startedExe, self)
	}

	r = New(Options{GuardExe: self, TermGrace: 300 * time.Millisecond})
	testCancelKillsTree(t, r, `trap '' TERM; `+treeScript, 3*time.Second)
}

func TestGuardPassesThroughIOAndExitCode(t *testing.T) {
	self, _ := os.Executable()
	r := New(Options{GuardExe: self})
	var out []string
	res, err := r.Run(context.Background(), Cmd{
		Path:         "/bin/sh",
		Args:         []string{"-c", `read x; echo "got $x"; echo oops 1>&2; exit 5`},
		Stdin:        strings.NewReader("ping\n"),
		OnStdoutLine: func(s string) { out = append(out, s) },
	})
	if !app.IsCode(err, app.CodeToolFailed) {
		t.Fatalf("err = %v", err)
	}
	if res.ExitCode != 5 || strings.Join(out, "|") != "got ping" || res.StderrTail != "oops\n" {
		t.Errorf("res = %+v, out = %q", res, out)
	}

	res, err = r.Run(context.Background(), Cmd{Path: "/usr/bin/true"})
	if err != nil || res.ExitCode != 0 {
		t.Errorf("true through guard: %+v, %v", res, err)
	}
}

func TestGuardKillsGroupWhenParentDies(t *testing.T) {
	self, _ := os.Executable()
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), helperEnv+"=orphan")
	out, err := cmd.Output() // returns once the intermediate parent has died
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	guardPid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || guardPid <= 1 {
		t.Fatalf("helper output %q", out)
	}
	t.Cleanup(func() { _ = unix.Kill(-guardPid, unix.SIGKILL) })

	// The guard leads the group that holds the shell and both sleeps. It
	// polls every 250 ms and the tree dies on SIGTERM, so the group must be
	// gone long before the 3 s SIGKILL escalation.
	waitGone(t, fmt.Sprintf("guard group %d", guardPid), func() bool { return groupAlive(guardPid) }, 3*time.Second)
}

func TestKillJobAndKillAll(t *testing.T) {
	r := New(Options{})
	run := func(job string, pc *pidCollector) chan error {
		done := make(chan error, 1)
		go func() {
			_, err := r.Run(context.Background(), Cmd{
				Path: "/bin/sh", Args: []string{"-c", `trap '' TERM; sleep 300 & echo $!; wait`},
				OnStdoutLine: pc.line, JobID: job,
			})
			done <- err
		}()
		return done
	}
	pcA, pcB := newPidCollector(1), newPidCollector(1)
	doneA, doneB := run("job_a", pcA), run("job_b", pcB)
	pidA, pidB := pcA.wait(t)[0], pcB.wait(t)[0]
	if n := len(r.Live()); n != 2 {
		t.Fatalf("Live() has %d entries, want 2", n)
	}

	r.KillJob("job_a")
	select {
	case err := <-doneA:
		if !app.IsCode(err, app.CodeToolFailed) || !errors.Is(err, ErrKilled) {
			t.Errorf("job_a err = %v, want TOOL_FAILED caused by ErrKilled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("job_a did not stop after KillJob")
	}
	waitGone(t, "job_a child", func() bool { return pidAlive(pidA) }, time.Second)
	if !pidAlive(pidB) {
		t.Fatal("KillJob(job_a) killed job_b")
	}

	r.KillAll()
	select {
	case <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("job_b did not stop after KillAll")
	}
	waitGone(t, "job_b child", func() bool { return pidAlive(pidB) }, time.Second)
}

func TestKillIfMatches(t *testing.T) {
	start := func() *exec.Cmd {
		cmd := exec.Command("/bin/sleep", "300")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}

	c := start()
	if killed, err := KillIfMatches(c.Process.Pid, "/bin/ls"); killed || err != nil {
		t.Fatalf("wrong exe: killed=%v err=%v", killed, err)
	}
	if !pidAlive(c.Process.Pid) {
		t.Fatal("process killed despite executable mismatch")
	}
	if killed, err := KillIfMatches(c.Process.Pid, "/bin/sleep"); !killed || err != nil {
		t.Fatalf("matching exe: killed=%v err=%v", killed, err)
	}
	_ = c.Wait()
	if c.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Errorf("process state = %v, want killed", c.ProcessState)
	}

	// Gone pid, own pid and pid 1 are never touched.
	for _, pid := range []int{c.Process.Pid, os.Getpid(), 1, 0, -5} {
		self, _ := os.Executable()
		if killed, err := KillIfMatches(pid, self); killed || err != nil {
			t.Errorf("KillIfMatches(%d) = %v, %v", pid, killed, err)
		}
	}
}

// A process running the current executable is only killed when it is a guard.
func TestKillIfMatchesOwnExecutableNeedsGuard(t *testing.T) {
	self, _ := os.Executable()

	// Not a guard: the test binary sleeping inside a test.
	plain := exec.Command(self, "-test.run", "^TestHelperSleep$")
	plain.Env = append(os.Environ(), helperEnv+"=sleep")
	if err := plain.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Process.Kill(); _ = plain.Wait() })

	guard := exec.Command(self, GuardSubcommand, "--", "/bin/sleep", "300")
	guard.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := guard.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(-guard.Process.Pid, unix.SIGKILL); _ = guard.Wait() })
	time.Sleep(300 * time.Millisecond)

	if killed, err := KillIfMatches(plain.Process.Pid, self); killed || err != nil {
		t.Errorf("non-guard process: killed=%v err=%v", killed, err)
	}
	if killed, err := KillIfMatches(guard.Process.Pid, self); !killed || err != nil {
		t.Errorf("guard process: killed=%v err=%v", killed, err)
	}
	_ = guard.Wait() // reap it: a zombie leader still counts as a group member
	waitGone(t, "guard group", func() bool { return groupAlive(guard.Process.Pid) }, 2*time.Second)
}

func TestHelperSleep(t *testing.T) {
	if os.Getenv(helperEnv) != "sleep" {
		t.Skip("helper process only")
	}
	time.Sleep(30 * time.Second)
}

func TestSpawnDetached(t *testing.T) {
	marker := t.TempDir() + "/marker"
	err := SpawnDetached("/bin/sh", []string{"-c", `ps -o pgid=,sess= -p $$ > /dev/null; echo "$$ $WK_DETACHED" > "$0"`, marker}, []string{"WK_DETACHED=yes", "PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(marker)
		if err == nil && strings.HasSuffix(strings.TrimSpace(string(b)), " yes") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached process did not write the marker (%v, %q)", err, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := SpawnDetached("/nonexistent/lt-engine", nil, nil); err == nil {
		t.Error("SpawnDetached with a missing executable returned nil")
	}
}

func TestSpawnDetachedNewSession(t *testing.T) {
	out := t.TempDir() + "/sid"
	// A session leader has pid == pgid; ps prints both for the shell itself.
	if err := SpawnDetached("/bin/sh", []string{"-c", `ps -o pid=,pgid= -p $$ > "$0.tmp" && mv "$0.tmp" "$0"; sleep 1`, out}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(out); err == nil {
			f := strings.Fields(string(b))
			if len(f) != 2 || f[0] != f[1] {
				t.Fatalf("pid/pgid = %q, want equal (own session and group)", b)
			}
			pid, _ := strconv.Atoi(f[0])
			if sid, err := unix.Getsid(pid); err == nil && sid != pid {
				t.Errorf("sid = %d, want %d", sid, pid)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("detached process produced no output")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOpenValidation(t *testing.T) {
	// Only the argument validation is tested: actually opening a browser or
	// Finder needs a GUI session.
	for _, u := range []string{"", "file:///etc/passwd", "javascript:alert(1)", "-R", "http://"} {
		if err := OpenBrowser(u); err == nil {
			t.Errorf("OpenBrowser(%q) = nil, want error", u)
		}
	}
	if err := RevealInFileManager("relative/path"); err == nil {
		t.Error("RevealInFileManager(relative) = nil")
	}
	if err := RevealInFileManager(t.TempDir() + "/missing"); err == nil {
		t.Error("RevealInFileManager(missing) = nil")
	}
}

func TestRunRejectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := false
	r := New(Options{OnStart: func(int, string, string, string) { started = true }})
	_, err := r.Run(ctx, Cmd{Path: "/bin/sh", Args: []string{"-c", "exit 0"}})
	if !errors.Is(err, context.Canceled) || started {
		t.Errorf("err = %v, started = %v", err, started)
	}
}
