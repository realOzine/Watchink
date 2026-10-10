//go:build darwin || linux

package procx

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// This file holds what macOS and Linux share: signalling process groups,
// detached spawning and the argument checks of the desktop helpers. How a
// child is tied to the engine's lifetime and how a pid is matched to
// its executable differ per platform: see sys_darwin.go and sys_linux.go.

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

// killTree sends SIGKILL to pid, and to its whole process group when pid
// leads one (children are started as group leaders). It returns true when a
// kill was sent.
func killTree(pid int) (bool, error) {
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

// SpawnDetached starts a fully detached background process: new session, no
// controlling terminal, stdio on /dev/null. It is used to launch
// `watchink engine run` from a client. A nil env inherits the
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
	return openURL(u.String())
}

// RevealInFileManager shows path in the platform file manager (Finder on
// macOS). The caller is responsible for restricting path to the data
// directory.
func RevealInFileManager(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("procx: reveal needs an absolute path")
	}
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	return revealPath(path)
}
