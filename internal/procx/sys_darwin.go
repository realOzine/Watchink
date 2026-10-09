//go:build darwin

package procx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// guardPollInterval is how often the guard checks whether its parent died.
const guardPollInterval = 250 * time.Millisecond

// setProcessGroup makes the child the leader of a new process group, so one
// signal to -pid reaches the tool and everything it spawned (§5.5).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateGroup(pgid int) error { return signalGroup(pgid, unix.SIGTERM) }

func killGroup(pgid int) error { return signalGroup(pgid, unix.SIGKILL) }

func signalGroup(pgid int, sig unix.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("procx: refusing to signal process group %d", pgid)
	}
	err := unix.Kill(-pgid, sig)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

// groupAlive reports whether any process is left in the group.
func groupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	err := unix.Kill(-pgid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// processInfo returns the executable path and argv of pid, read from the
// kern.procargs2 sysctl (no subprocess). ok is false when the process does
// not exist or belongs to another user.
func processInfo(pid int) (exe string, argv []string, ok bool) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 4 {
		return "", nil, false
	}
	// Layout: int32 argc, exec path, NUL padding, argv[0..argc), environment.
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end <= 0 {
		return "", nil, false
	}
	exe = string(rest[:end])
	rest = bytes.TrimLeft(rest[end:], "\x00")
	for i := 0; i < argc && len(rest) > 0; i++ {
		end = bytes.IndexByte(rest, 0)
		if end < 0 {
			end = len(rest)
		}
		argv = append(argv, string(rest[:end]))
		if end == len(rest) {
			break
		}
		rest = rest[end+1:]
	}
	return exe, argv, true
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(sa, sb)
}

// KillIfMatches kills a child left over from a previous engine, but only when
// the process with that pid still runs the recorded executable (§7.6 step 3):
// a recycled pid must never be killed. It returns true when a kill was sent.
//
// Because guarded children are recorded with the watchink executable
// itself, a process running the current executable is only killed when it is
// a `_guard` invocation; the engine, CLI and desktop processes are never hit.
func KillIfMatches(pid int, exe string) (bool, error) {
	if pid <= 1 || pid == os.Getpid() || pid == os.Getppid() {
		return false, nil
	}
	actual, argv, ok := processInfo(pid)
	if !ok || !samePath(actual, exe) {
		return false, nil
	}
	if self, err := os.Executable(); err == nil && samePath(actual, self) {
		if len(argv) < 2 || argv[1] != GuardSubcommand {
			return false, nil
		}
	}
	// Children are started as group leaders, so take the whole tree with them.
	if pgid, err := unix.Getpgid(pid); err == nil && pgid == pid {
		if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			return false, err
		}
		return true, nil
	}
	if err := unix.Kill(pid, unix.SIGKILL); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// GuardMain implements the hidden `watchink _guard [--] <path> <args...>`
// subcommand and returns the process exit code (§5.5, macOS).
//
// macOS has no Pdeathsig, so the engine starts every tool through this small
// supervisor: it runs the real command as its child inside the guard's own
// process group, forwards termination signals, and polls its parent pid. When
// the parent changes (the engine died and the guard was re-parented to pid 1)
// it terminates the whole group, itself included.
func GuardMain(args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: watchink _guard -- <command> [args...]")
		return 2
	}
	parent := os.Getppid()
	if parent <= 1 {
		// The engine died before we even started: do not start an orphan.
		return 1
	}

	// The Runner starts the guard as a group leader. When started any other
	// way, try to become one; never signal a group we do not lead.
	self := os.Getpid()
	ownGroup := unix.Getpgrp() == self
	if !ownGroup {
		ownGroup = unix.Setpgid(0, 0) == nil && unix.Getpgrp() == self
	}

	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, unix.SIGTERM, unix.SIGINT, unix.SIGHUP, unix.SIGQUIT)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if !ownGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "watchink _guard: %v\n", err)
		return 127
	}
	group := self
	if !ownGroup {
		group = cmd.Process.Pid
	}

	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	ticker := time.NewTicker(guardPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return guardExitCode(cmd)
		case sig := <-sigs:
			// A signal sent to the group already reached the child; one sent
			// to the guard alone did not, so always pass it on.
			_ = cmd.Process.Signal(sig)
		case <-ticker.C:
			if os.Getppid() == parent {
				continue
			}
			// Orphaned: same escalation as a cancel (§5.5).
			_ = signalGroup(group, unix.SIGTERM)
			select {
			case <-done:
			case <-time.After(DefaultTermGrace):
			}
			_ = signalGroup(group, unix.SIGKILL) // kills the guard too when it leads the group
			return 128 + int(unix.SIGKILL)
		}
	}
}

func guardExitCode(cmd *exec.Cmd) int {
	ps := cmd.ProcessState
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// SpawnDetached starts a fully detached background process: new session, no
// controlling terminal, stdio on /dev/null. It is used to launch
// `watchink engine run` from a client (§5.2). A nil env inherits the
// caller's environment. The process is not waited for.
func SpawnDetached(exe string, args []string, env []string) error {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("procx: spawn %s: %w", filepath.Base(exe), err)
	}
	return cmd.Process.Release()
}

// OpenBrowser opens an http(s) URL in the default browser.
func OpenBrowser(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("procx: refusing to open non-http URL")
	}
	return runOpen(u.String())
}

// RevealInFileManager selects path in Finder. The caller is responsible for
// restricting path to the data directory (§15).
func RevealInFileManager(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("procx: reveal needs an absolute path")
	}
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	return runOpen("-R", path)
}

func runOpen(args ...string) error {
	cmd := exec.Command("/usr/bin/open", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("procx: open: %w: %s", err, Redact(string(bytes.TrimSpace(out))))
	}
	return nil
}
