// Package version holds the build identity, set with -ldflags -X at build time.
package version

var (
	// Version is the semantic version of the program.
	Version = "0.0.1"
	// BuildID is "<commit>-<build date>"; clients compare it with the running
	// engine's build_id (docs/v0.md §5.2).
	BuildID = "dev"
)
