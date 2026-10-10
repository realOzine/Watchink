//go:build windows

package tools

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// isDiskFull reports whether err means the volume is out of space.
func isDiskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}

// Rename retry budget: see rename.
const (
	renameAttempts   = 20
	renameRetryDelay = 100 * time.Millisecond
)

// rename is os.Rename, retried for a short while when Windows refuses it
// because another process has the file (or a file below the directory) open
// without share-delete access. That is what an antivirus does to a program
// that was just written and run for its smoke test, a moment before the
// install moves it into place (step 6).
func rename(oldpath, newpath string) error {
	var err error
	for i := 0; i < renameAttempts; i++ {
		if err = os.Rename(oldpath, newpath); err == nil || !isBusy(err) {
			return err
		}
		time.Sleep(renameRetryDelay)
	}
	return err
}

func isBusy(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// makeLink makes link refer to the file target. Creating a symbolic link
// needs a privilege ordinary Windows users do not have, so the entry is a
// hard link (same volume: both live under <data>/tools), or a copy when the
// filesystem has no hard links (exFAT, some network shares). relTarget, the
// symlink text used elsewhere, is not needed.
func makeLink(target, relTarget, link string) error {
	if err := os.Link(target, link); err == nil {
		return nil
	}
	src, err := os.Open(target)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(link, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(link)
	}
	return err
}
