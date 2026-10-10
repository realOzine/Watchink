package store

import (
	"errors"
	"syscall"
)

// Windows reports a full disk with its own error numbers rather than ENOSPC.
const (
	errorHandleDiskFull syscall.Errno = 39  // ERROR_HANDLE_DISK_FULL
	errorDiskFull       syscall.Errno = 112 // ERROR_DISK_FULL
)

func osDiskFull(err error) bool {
	return errors.Is(err, errorDiskFull) || errors.Is(err, errorHandleDiskFull)
}
