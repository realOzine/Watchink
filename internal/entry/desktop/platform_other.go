//go:build desktop && !linux

package desktop

// windowIcon is only used on Linux: the macOS icon comes from the .app
// bundle.
func windowIcon() []byte { return nil }

// preflight has nothing to check: a session without a display only exists on
// Linux.
func preflight() error { return nil }
