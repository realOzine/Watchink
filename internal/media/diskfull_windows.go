//go:build windows

package media

import (
	"errors"

	"golang.org/x/sys/windows"
)

// osDiskFull recognises the Windows error codes for a full volume. Go does
// not map them to syscall.ENOSPC, which on Windows is only what C runtimes
// report.
func osDiskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}
