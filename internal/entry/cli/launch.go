package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// Launch is a command that turns this process into another role (the
// desktop window, an engine, the MCP server) instead of acting as a client
// of the engine. What each role does is decided where the program is put
// together (cmd/watchink); this package only gives such commands a place in
// the command tree, the global flags and the common error output.
type Launch struct {
	// Parent is "" for a top-level command, or the name of an existing
	// command group to attach to (e.g. "engine" for `engine run`).
	Parent string
	Use    string
	Short  string
	Hidden bool
	// Setup may register the command's own flags.
	Setup func(cmd *cobra.Command)
	Run   func(ctx context.Context, g Globals) error
}

// Globals are the global flags a Launch needs.
type Globals struct {
	// DataDir is the resolved data directory (see Deps.ResolveDataDir).
	DataDir string
	// NoSpawn mirrors --no-spawn.
	NoSpawn bool
}

// launch adapts a role function to cobra: it resolves the data directory
// and routes errors through the same handling as every other command.
func (e *env) launch(fn func(ctx context.Context, g Globals) error) func(*cobra.Command, []string) error {
	return e.run(func(c *call) error {
		dir, err := c.resolveDataDir()
		if err != nil {
			return err
		}
		return fn(c.ctx, Globals{DataDir: dir, NoSpawn: c.noSpawn})
	})
}

// addLaunchers attaches Deps.Launch to the tree.
func (e *env) addLaunchers(root *cobra.Command) {
	for _, l := range e.deps.Launch {
		cmd := &cobra.Command{Use: l.Use, Short: l.Short, Hidden: l.Hidden, Args: noArgs, RunE: e.launch(l.Run)}
		if l.Setup != nil {
			l.Setup(cmd)
		}
		parent := root
		if l.Parent != "" {
			for _, c := range root.Commands() {
				if c.Name() == l.Parent {
					parent = c
				}
			}
		}
		parent.AddCommand(cmd)
	}
}
