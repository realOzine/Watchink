// Package linuxassets embeds the Linux desktop integration files that the
// single-ELF release carries inside the executable (docs/v0.md §16.3).
//
// watchink.png is build/appicon.png scaled to 256x256
// (`sips -Z 256 build/appicon.png --out build/linux/watchink.png` on macOS,
// `convert build/appicon.png -resize 256x256 build/linux/watchink.png`
// elsewhere); regenerate it whenever the app icon changes.
package linuxassets

import _ "embed"

// Icon is the PNG shown as the window icon.
//
//go:embed watchink.png
var Icon []byte
