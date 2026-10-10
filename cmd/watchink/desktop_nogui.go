//go:build !desktop

package main

import "context"

// runDesktop is nil in builds without the desktop tag: the bare command
// prints help instead of opening a window (§4.2).
var runDesktop func(ctx context.Context, dataDir string) error

// webView is the `doctor` probe of the system WebView; there is no window to
// run on it in this build.
var webView func() (name, version string, required bool)
