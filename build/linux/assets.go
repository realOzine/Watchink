// Package linuxassets embeds the Linux desktop integration files that the
// single-ELF release carries inside the executable (docs/v0.md §16.3).
//
// watchink.png is the app icon (build/appicon.png) at 256x256.
package linuxassets

import _ "embed"

// Icon is the PNG shown as the window icon.
//
//go:embed watchink.png
var Icon []byte
