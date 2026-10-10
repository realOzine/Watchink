//go:build !linux

package sysreq

// Desktop returns nil: this platform has no system prerequisite to look for.
// macOS ships WKWebView and its media stack with the system.
func Desktop() []Requirement { return nil }
