//go:build !desktop

package main

import (
	"context"

	"watchink/internal/cli"
)

// runDesktop is nil in builds without the desktop tag: the bare command
// prints help instead of opening a window.
var runDesktop func(ctx context.Context, dataDir string) error

// desktopPrereqs is nil as well: without a window there is nothing to check.
var desktopPrereqs func() []cli.SystemPrereq

// webView is the `doctor` probe of the system WebView; there is no window to
// run on it in this build.
var webView func() (name, version string, required bool)
