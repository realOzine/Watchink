package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"watchink/internal/app"
	"watchink/internal/entry/cli"
)

// rolesDeps is the part of main's wiring that concerns roles; nothing in it
// starts an engine or a window unless the matching command is run.
func rolesDeps() cli.Deps {
	return cli.Deps{
		Launch: roles(), Default: defaultRole(), HasDesktop: runDesktop != nil,
		ResolveDataDir: func(flag string) (string, error) { return "/data", nil },
	}
}

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = cli.Execute(context.Background(), rolesDeps(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// TestEveryRoleCommandExists: the commands that pick a role are in the
// command tree, next to the client commands of their group.
func TestEveryRoleCommandExists(t *testing.T) {
	root := cli.New(rolesDeps())
	for _, path := range []string{"desktop", "serve", "mcp", "engine run", "engine status"} {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil || cmd.CommandPath() != "watchink "+path {
			t.Errorf("command %q not found (%v)", path, err)
		}
	}
	for _, flag := range []string{"port", "open"} {
		serve, _, _ := root.Find([]string{"serve"})
		if serve.Flags().Lookup(flag) == nil {
			t.Errorf("serve --%s missing", flag)
		}
	}
	// `engine run` is started by clients, not typed by people.
	if _, out, _ := run(t, "engine", "--help"); strings.Contains(out, "后台 Engine") {
		t.Errorf("engine run is listed in help:\n%s", out)
	}
}

// TestRolesWithoutADesktopWindow describes a build without the desktop tag,
// which is what `go test` builds by default: the bare command prints help
// and `desktop` explains itself instead of being an unknown command.
func TestRolesWithoutADesktopWindow(t *testing.T) {
	if runDesktop != nil {
		t.Skip("this build has the desktop window")
	}
	if defaultRole() != nil {
		t.Fatal("a build without a window must not have a default role")
	}
	code, out, _ := run(t)
	if code != app.ExitOK || !strings.Contains(out, "Usage") || !strings.Contains(out, "serve") {
		t.Errorf("bare command: exit %d\n%s", code, out)
	}
	code, _, errOut := run(t, "desktop")
	if code != app.ExitUsage || !strings.Contains(errOut, "不包含桌面界面") {
		t.Errorf("desktop: exit %d, stderr %q", code, errOut)
	}
}
