//go:build windows

package procx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"watchink/internal/app"
)

// These tests cannot run on the macOS development machine; they are compiled
// there (`GOOS=windows go test -c`) and executed by the Windows CI job.

const (
	helperEnv  = "PROCX_TEST_HELPER"
	helperExit = "PROCX_TEST_EXIT"
	helperFile = "PROCX_TEST_FILE"
)

// TestMain lets the test binary play the child processes (re-exec pattern),
// so the tests need no shell and no quoting:
//
//	echo    read one line from stdin, report it with WK_VAR and the working
//	        directory on stdout, write "warn" to stderr, exit with helperExit
//	sleep   sleep for five minutes
//	tree    start a `sleep` child, print its pid, wait for it
//	orphan  run a `tree` through a Runner, print "<tree pid> <sleep pid>" and
//	        exit at once, abandoning the job like a crashed engine would
//	touch   create the file named by helperFile
func TestMain(m *testing.M) {
	self, _ := os.Executable()
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "echo":
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		wd, _ := os.Getwd()
		fmt.Printf("in:%s\nv=%s\n%s\n", strings.TrimSpace(line), os.Getenv("WK_VAR"), wd)
		fmt.Fprintln(os.Stderr, "warn "+os.Getenv("WK_SECRET"))
		code, _ := strconv.Atoi(os.Getenv(helperExit))
		os.Exit(code)
	case "sleep":
		time.Sleep(5 * time.Minute)
	case "tree":
		cmd := exec.Command(self)
		cmd.Env = helperEnviron("sleep")
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(cmd.Process.Pid)
		_ = cmd.Wait()
	case "orphan":
		var treePid int
		r := New(Options{OnStart: func(pid int, _, _, _ string) { treePid = pid }})
		go r.Run(context.Background(), Cmd{Path: self, Env: helperEnviron("tree"), OnStdoutLine: func(line string) {
			fmt.Printf("%d %s\n", treePid, strings.TrimSpace(line))
			os.Exit(0) // die without cleaning up: the job handle is closed by the system
		}})
		time.Sleep(30 * time.Second)
		os.Exit(1)
	case "touch":
		if err := os.WriteFile(os.Getenv(helperFile), []byte("ok"), 0o644); err != nil {
			os.Exit(1)
		}
	}
	os.Exit(0)
}

func helperEnviron(mode string, extra ...string) []string {
	return append(append(os.Environ(), helperEnv+"="+mode), extra...)
}

func selfExe(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
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

func liveJobs() int {
	jobs.Lock()
	defer jobs.Unlock()
	return len(jobs.m)
}

// pidLine collects the pid a `tree` helper prints.
type pidLine struct {
	once sync.Once
	pid  int
	got  chan struct{}
}

func newPidLine() *pidLine { return &pidLine{got: make(chan struct{})} }

func (p *pidLine) line(s string) {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		p.once.Do(func() { p.pid = n; close(p.got) })
	}
}

func (p *pidLine) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-p.got:
	case <-time.After(20 * time.Second):
		t.Fatal("child tree did not report its pid")
	}
	return p.pid
}

func TestRunSuccessAndCallbacks(t *testing.T) {
	self := selfExe(t)
	dir := t.TempDir()
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
	res, err := r.Run(context.Background(), Cmd{
		Path:         self,
		Dir:          dir,
		Env:          helperEnviron("echo", "WK_VAR=42"),
		Stdin:        strings.NewReader("hello\n"),
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
	if len(out) != 3 || out[0] != "in:hello" || out[1] != "v=42" || !samePath(out[2], dir) {
		t.Errorf("stdout lines = %q (dir %s)", out, dir)
	}
	if len(errLines) != 1 || strings.TrimSpace(errLines[0]) != "warn" || strings.TrimSpace(res.StderrTail) != "warn" {
		t.Errorf("stderr lines = %q, tail = %q", errLines, res.StderrTail)
	}
	if len(started) != 1 || len(exited) != 1 || started[0] != res.PID || exited[0] != res.PID {
		t.Errorf("hooks: started=%v exited=%v pid=%d", started, exited, res.PID)
	}
	if !samePath(gotExe, self) || gotJob != "job_1" || gotStage != "probe" {
		t.Errorf("OnStart labels = %q %q %q", gotExe, gotJob, gotStage)
	}
	if len(r.Live()) != 0 || liveJobs() != 0 {
		t.Errorf("after exit: Live() = %v, %d job objects", r.Live(), liveJobs())
	}
}

func TestRunNonZeroExit(t *testing.T) {
	r := New(Options{Secrets: []string{"topsecretvalue"}})
	res, err := r.Run(context.Background(), Cmd{
		Path:  selfExe(t),
		Env:   helperEnviron("echo", helperExit+"=7", "WK_SECRET=topsecretvalue"),
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
	if strings.TrimSpace(tail) != "warn ***" {
		t.Errorf("stderr_tail = %q", tail)
	}
	if res == nil || res.ExitCode != 7 || res.StderrTail != tail {
		t.Errorf("result = %+v", res)
	}
}

func TestRunMissingTool(t *testing.T) {
	r := New(Options{})
	_, err := r.Run(context.Background(), Cmd{Path: filepath.Join(t.TempDir(), "no-such-tool.exe")})
	if !app.IsCode(err, app.CodeToolMissing) {
		t.Fatalf("err = %v, want TOOL_MISSING", err)
	}
}

// A cancel terminates the whole job: the child and what it started.
func TestCancelKillsProcessTree(t *testing.T) {
	r := New(Options{})
	pl := newPidLine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		res *Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := r.Run(ctx, Cmd{Path: selfExe(t), Env: helperEnviron("tree"), OnStdoutLine: pl.line, JobID: "job_c"})
		done <- outcome{res, err}
	}()
	grandchild := pl.wait(t)
	if !pidAlive(grandchild) {
		t.Fatalf("grandchild %d not running before cancel", grandchild)
	}
	if n := liveJobs(); n != 1 {
		t.Fatalf("%d job objects while one child runs", n)
	}

	begin := time.Now()
	cancel()
	var o outcome
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if took := time.Since(begin); took > 2*time.Second {
		t.Errorf("cancel took %v", took)
	}
	if !errors.Is(o.err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", o.err)
	}
	waitGone(t, fmt.Sprintf("grandchild %d", grandchild), func() bool { return pidAlive(grandchild) }, 2*time.Second)
	if o.res != nil && pidAlive(o.res.PID) {
		t.Errorf("child %d still alive", o.res.PID)
	}
	if liveJobs() != 0 {
		t.Errorf("%d job objects left after cancel", liveJobs())
	}
}

func TestKillJobAndKillAll(t *testing.T) {
	r := New(Options{})
	run := func(job string, pl *pidLine) chan error {
		done := make(chan error, 1)
		go func() {
			_, err := r.Run(context.Background(), Cmd{Path: selfExe(t), Env: helperEnviron("tree"), OnStdoutLine: pl.line, JobID: job})
			done <- err
		}()
		return done
	}
	plA, plB := newPidLine(), newPidLine()
	doneA, doneB := run("job_a", plA), run("job_b", plB)
	pidA, pidB := plA.wait(t), plB.wait(t)
	if n := len(r.Live()); n != 2 {
		t.Fatalf("Live() has %d entries, want 2", n)
	}

	r.KillJob("job_a")
	select {
	case err := <-doneA:
		if !app.IsCode(err, app.CodeToolFailed) || !errors.Is(err, ErrKilled) {
			t.Errorf("job_a err = %v, want TOOL_FAILED caused by ErrKilled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("job_a did not stop after KillJob")
	}
	waitGone(t, "job_a grandchild", func() bool { return pidAlive(pidA) }, 2*time.Second)
	if !pidAlive(pidB) {
		t.Fatal("KillJob(job_a) killed job_b")
	}

	r.KillAll()
	select {
	case <-doneB:
	case <-time.After(10 * time.Second):
		t.Fatal("job_b did not stop after KillAll")
	}
	waitGone(t, "job_b grandchild", func() bool { return pidAlive(pidB) }, 2*time.Second)
}

// §5.5: when the engine dies without cleaning up, the system closes its job
// handles and KILL_ON_JOB_CLOSE takes the children with it.
func TestJobDiesWithItsOwner(t *testing.T) {
	cmd := exec.Command(selfExe(t))
	cmd.Env = helperEnviron("orphan")
	out, err := cmd.Output() // returns once the stand-in engine has died
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	var tree, sleeper int
	if _, err := fmt.Sscan(string(out), &tree, &sleeper); err != nil || tree <= 0 || sleeper <= 0 {
		t.Fatalf("helper output %q", out)
	}
	waitGone(t, fmt.Sprintf("child %d", tree), func() bool { return pidAlive(tree) }, 5*time.Second)
	waitGone(t, fmt.Sprintf("grandchild %d", sleeper), func() bool { return pidAlive(sleeper) }, 5*time.Second)
}

func TestKillIfMatches(t *testing.T) {
	ping, err := exec.LookPath("ping")
	if err != nil {
		t.Skip("needs ping.exe")
	}
	cmdExe, err := exec.LookPath("cmd")
	if err != nil {
		t.Skip("needs cmd.exe")
	}
	c := exec.Command(ping, "-n", "300", "127.0.0.1")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })

	if killed, err := KillIfMatches(c.Process.Pid, cmdExe); killed || err != nil {
		t.Fatalf("wrong exe: killed=%v err=%v", killed, err)
	}
	if !pidAlive(c.Process.Pid) {
		t.Fatal("process killed despite executable mismatch")
	}
	// The recorded path may differ in case from what the system reports.
	if killed, err := KillIfMatches(c.Process.Pid, strings.ToUpper(ping)); !killed || err != nil {
		t.Fatalf("matching exe: killed=%v err=%v", killed, err)
	}
	_ = c.Wait()
	if c.ProcessState.ExitCode() != killExitCode {
		t.Errorf("process state = %v, want killed", c.ProcessState)
	}

	// Gone pid, own pid and the system pids are never touched.
	self := selfExe(t)
	for _, pid := range []int{c.Process.Pid, os.Getpid(), 4, 0, -5} {
		if killed, err := KillIfMatches(pid, self); killed || err != nil {
			t.Errorf("KillIfMatches(%d) = %v, %v", pid, killed, err)
		}
	}
}

// There is no guard on Windows, so a process running the current executable
// is an engine, a CLI or the desktop window: never a leftover tool.
func TestKillIfMatchesSparesOwnExecutable(t *testing.T) {
	self := selfExe(t)
	c := exec.Command(self)
	c.Env = helperEnviron("sleep")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	if killed, err := KillIfMatches(c.Process.Pid, self); killed || err != nil {
		t.Errorf("own executable: killed=%v err=%v", killed, err)
	}
	if !pidAlive(c.Process.Pid) {
		t.Error("a process running the current executable was killed")
	}
}

func TestSpawnDetached(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "spawned")
	if err := SpawnDetached(selfExe(t), nil, helperEnviron("touch", helperFile+"="+marker)); err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached process did not run")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := SpawnDetached(filepath.Join(t.TempDir(), "missing.exe"), nil, nil); err == nil {
		t.Error("spawning a missing executable succeeded")
	}
}

func TestShellHelpersRejectBadInput(t *testing.T) {
	for _, u := range []string{"file:///C:/Windows/win.ini", "javascript:alert(1)", "", "https://"} {
		if err := OpenBrowser(u); err == nil {
			t.Errorf("OpenBrowser(%q) accepted", u)
		}
	}
	if err := RevealInFileManager("relative.txt"); err == nil {
		t.Error("RevealInFileManager accepted a relative path")
	}
	if err := RevealInFileManager(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("RevealInFileManager accepted a missing path")
	}
	if code := GuardMain([]string{"--", "cmd"}); code != 2 {
		t.Errorf("GuardMain = %d, want 2", code)
	}
}
