//go:build !desktop

package main

import "context"

// runDesktop is nil in builds without the desktop tag: the bare command
// prints help instead of opening a window (§4.2).
var runDesktop func(ctx context.Context, dataDir string) error
