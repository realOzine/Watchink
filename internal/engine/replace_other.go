//go:build !windows

package engine

import "os"

// replaceFile renames src over dst. rename(2) replaces dst atomically even
// while readers have it open; only Windows needs more (replace_windows.go).
func replaceFile(src, dst string) error { return os.Rename(src, dst) }
