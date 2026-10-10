//go:build windows

package engine

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Retry budget of replaceFile.
const (
	replaceAttempts   = 40
	replaceRetryDelay = 25 * time.Millisecond
)

// replaceFile renames src over dst. Unlike on Unix, Windows refuses to
// replace a file that another process has open, and clients polling for a
// starting engine read engine.json every 100 ms (§5.2 step 3). Each read
// holds the file for well under a millisecond, so a short retry is enough.
func replaceFile(src, dst string) error {
	var err error
	for i := 0; i < replaceAttempts; i++ {
		err = os.Rename(src, dst)
		if err == nil || !(errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
			errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
			errors.Is(err, windows.ERROR_LOCK_VIOLATION)) {
			return err
		}
		time.Sleep(replaceRetryDelay)
	}
	return err
}
