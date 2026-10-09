package tools

import (
	"embed"
	"io/fs"
)

// embeddedBin holds the whisper-cli builds produced by
// third_party/whisper/build.sh (docs/v0.md D-06, §16.1). The binaries are not
// committed; a fresh clone only has the .gitkeep files, in which case
// whisper-cli is reported as TOOL_MISSING at runtime.
//
//go:embed all:embedded/bin
var embeddedBin embed.FS

// embeddedFS returns the embedded tree rooted at "<os>-<arch>/".
func embeddedFS() fs.FS {
	sub, err := fs.Sub(embeddedBin, "embedded/bin")
	if err != nil {
		panic(err) // the path is a compile-time constant
	}
	return sub
}
