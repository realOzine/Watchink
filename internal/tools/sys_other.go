//go:build !windows

package tools

import (
	"errors"
	"os"
	"syscall"
)

// isDiskFull reports whether err means the volume is out of space.
func isDiskFull(err error) bool { return errors.Is(err, syscall.ENOSPC) }

// rename is os.Rename; only Windows needs to retry it (sys_windows.go).
func rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// makeLink makes link a symbolic link to the file target, written as the
// relative path relTarget so the tools directory can be moved as a whole.
func makeLink(target, relTarget, link string) error { return os.Symlink(relTarget, link) }
