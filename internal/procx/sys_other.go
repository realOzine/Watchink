//go:build !darwin && !linux && !windows

package procx

import (
	"errors"
	"fmt"
	"os/exec"
)

// errUnsupported is returned by every platform hook that has not been
// implemented yet. v0 is implemented and verified on macOS only.
var errUnsupported = errors.New("procx: not implemented on this platform")

// TODO(linux): new process group + Pdeathsig=SIGKILL (§5.5).
// TODO(windows): Job Object with KILL_ON_JOB_CLOSE (§5.5).
func setProcessGroup(cmd *exec.Cmd) {}

// TODO(linux): SIGTERM to the process group.
// TODO(windows): TerminateJobObject.
func terminateGroup(pgid int) error { return errUnsupported }

// TODO(linux): SIGKILL to the process group.
// TODO(windows): TerminateJobObject.
func killGroup(pgid int) error { return errUnsupported }

// TODO(linux) TODO(windows): report whether the group / job still has members.
func groupAlive(pgid int) bool { return false }

// KillIfMatches is not implemented on this platform.
//
// TODO(linux): compare /proc/<pid>/exe. TODO(windows): QueryFullProcessImageName.
func KillIfMatches(pid int, exe string) (bool, error) { return false, errUnsupported }

// GuardMain is only needed on macOS; other platforms rely on Pdeathsig or a
// Job Object (§5.5).
func GuardMain(args []string) int {
	fmt.Println("watchink _guard is only available on macOS")
	return 2
}

// SpawnDetached is not implemented on this platform.
//
// TODO(linux): Setsid. TODO(windows): DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP.
func SpawnDetached(exe string, args []string, env []string) error { return errUnsupported }

// OpenBrowser is not implemented on this platform.
//
// TODO(linux): xdg-open. TODO(windows): rundll32 url.dll,FileProtocolHandler.
func OpenBrowser(rawURL string) error { return errUnsupported }

// RevealInFileManager is not implemented on this platform.
//
// TODO(linux): org.freedesktop.FileManager1 / xdg-open on the parent directory.
// TODO(windows): explorer.exe /select,<path>.
func RevealInFileManager(path string) error { return errUnsupported }
