//go:build darwin

package procx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"watchink/internal/app"
)

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
