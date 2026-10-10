//go:build desktop

package desktop

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWindowsQuote(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Tools\Watchink\watchink.com`:                  `C:\Tools\Watchink\watchink.com`,
		`C:\Program Files\Watchink\watchink.com`:          `"C:\Program Files\Watchink\watchink.com"`,
		`C:\Users\张三\AppData\Local\Watchink\watchink.com`: `"C:\Users\张三\AppData\Local\Watchink\watchink.com"`,
		`C:\Users\o'neil\watchink.com`:                    `"C:\Users\o'neil\watchink.com"`,
		`C:\a&b\watchink.com`:                             `"C:\a&b\watchink.com"`,
		``:                                                `""`,
		`\\server\share\Watchink (portable)\watchink.com`: `"\\server\share\Watchink (portable)\watchink.com"`,
	} {
		if got := windowsQuote(in); got != want {
			t.Errorf("windowsQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestConsoleSibling(t *testing.T) {
	dir := t.TempDir()
	gui := filepath.Join(dir, "Watchink.exe")
	if err := os.WriteFile(gui, []byte("gui"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A lone executable (a development build) stays what it is.
	if got := consoleSibling(gui); got != gui {
		t.Errorf("without a twin: %s", got)
	}
	// A directory of that name is not an executable.
	twin := filepath.Join(dir, ConsoleExecutableName)
	if err := os.Mkdir(twin, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := consoleSibling(gui); got != gui {
		t.Errorf("twin is a directory: %s", got)
	}
	os.Remove(twin)
	if err := os.WriteFile(twin, []byte("console"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := consoleSibling(gui); got != twin {
		t.Errorf("with a twin: %s, want %s", got, twin)
	}
	// Other platforms never switch executables.
	if runtime.GOOS != "windows" && agentExecutable(gui) != gui {
		t.Error("agentExecutable changed the executable outside Windows")
	}
}

// TestWindowsAgentSetup is the Windows counterpart of TestAgentSetupInfo: the
// snippets must start the console twin and quote the way cmd.exe and
// PowerShell understand.
func TestWindowsAgentSetup(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only")
	}
	dir := filepath.Join(t.TempDir(), "My Apps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gui, twin := filepath.Join(dir, "Watchink.exe"), filepath.Join(dir, ConsoleExecutableName)
	for _, p := range []string{gui, twin} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s, err := newApp(Options{Executable: gui}).AgentSetupInfo()
	if err != nil {
		t.Fatal(err)
	}
	if s.Executable != twin || len(s.MCPCommand) != 2 || s.MCPCommand[0] != twin || s.MCPCommand[1] != "mcp" {
		t.Fatalf("%+v", s)
	}
	if want := `claude mcp add watchink -- "` + twin + `" mcp`; s.Snippets[0].Content != want {
		t.Fatalf("quoting: %s", s.Snippets[0].Content)
	}
	// JSON needs the backslashes escaped; the decoder gives the path back.
	if !strings.Contains(s.Snippets[1].Content, strings.ReplaceAll(twin, `\`, `\\`)) {
		t.Fatalf("claude desktop snippet: %s", s.Snippets[1].Content)
	}
}
