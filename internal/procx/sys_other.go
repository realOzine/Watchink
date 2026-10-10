//go:build !darwin && !linux && !windows

package procx

import (
	"errors"
	"fmt"
	"os/exec"
)

// errUnsupported is returned by every platform hook that has not been
// implemented: macOS, Linux and Windows have their own files, this one
// keeps every other GOOS compiling.
var errUnsupported = errors.New("procx: not implemented on this platform")

func setProcessGroup(cmd *exec.Cmd) {}

func terminateGroup(pgid int) error { return errUnsupported }

func killGroup(pgid int) error { return errUnsupported }

func groupAlive(pgid int) bool { return false }

// KillIfMatches is not implemented on this platform.
func KillIfMatches(pid int, exe string) (bool, error) { return false, errUnsupported }

// GuardMain is only needed on macOS; other platforms rely on Pdeathsig or a
// Job Object (§5.5).
func GuardMain(args []string) int {
	fmt.Println("watchink _guard is only available on macOS")
	return 2
}

// SpawnDetached is not implemented on this platform.
func SpawnDetached(exe string, args []string, env []string) error { return errUnsupported }

// OpenBrowser is not implemented on this platform.
func OpenBrowser(rawURL string) error { return errUnsupported }

// RevealInFileManager is not implemented on this platform.
func RevealInFileManager(path string) error { return errUnsupported }
