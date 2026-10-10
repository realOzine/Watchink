//go:build linux

package procx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMain lets the test binary play one extra role (re-exec pattern):
//
//	PROCX_TEST_HELPER=orphan <test binary>  start a child through a Runner,
//	                                        exactly as the engine does, print
//	                                        its pid and die without cleaning up
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "orphan" {
		r := New(Options{OnStart: func(pid int, _, _, _ string) { fmt.Println(pid) }})
		go func() {
			_, _ = r.Run(context.Background(), Cmd{Path: "/bin/sleep", Args: []string{"300"}})
		}()
		time.Sleep(400 * time.Millisecond) // let the child start and its pid be printed
		os.Exit(0)                         // die like a killed engine: no cancel, no wait
	}
	os.Exit(m.Run())
}

// procRunning reports whether pid is a live process. Unlike kill(pid, 0) it
// does not count a zombie that nobody has reaped yet, which is what a killed
// orphan stays until init gets to it.
func procRunning(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// "<pid> (<comm>) <state> …": comm may itself contain parentheses.
	rest := string(data[strings.LastIndexByte(string(data), ')')+1:])
	f := strings.Fields(rest)
	return len(f) > 0 && f[0] != "Z" && f[0] != "X"
}

// when the engine dies without a chance to clean up, the kernel kills
// its children through Pdeathsig.
func TestPdeathsigKillsChildWhenParentDies(t *testing.T) {
	self, _ := os.Executable()
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), helperEnv+"=orphan")
	out, err := cmd.Output() // returns once the helper has died
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 1 {
		t.Fatalf("helper output %q", out)
	}
	t.Cleanup(func() { _ = unix.Kill(-pid, unix.SIGKILL) })
	waitGone(t, fmt.Sprintf("orphaned child %d", pid), func() bool { return procRunning(pid) }, 3*time.Second)
}

func TestSetProcessGroup(t *testing.T) {
	cmd := exec.Command("/bin/true")
	setProcessGroup(cmd)
	if a := cmd.SysProcAttr; a == nil || !a.Setpgid || a.Pdeathsig != syscall.SIGKILL {
		t.Errorf("SysProcAttr = %+v, want Setpgid and Pdeathsig=SIGKILL", cmd.SysProcAttr)
	}
}

func TestGuardMainIsAStub(t *testing.T) {
	if code := GuardMain([]string{"--", "/bin/true"}); code != 2 {
		t.Errorf("GuardMain = %d, want 2", code)
	}
}

func TestFileURI(t *testing.T) {
	cases := map[string]string{
		"/home/u/.local/share/watchink/library/vid_1/note.md": "file:///home/u/.local/share/watchink/library/vid_1/note.md",
		"/data/My Videos/a,b.mp4":                             "file:///data/My%20Videos/a%2Cb.mp4",
		`/data/it's "x"\y`:                                    "file:///data/it%27s%20%22x%22%5Cy",
		"/data/笔记":                                            "file:///data/%E7%AC%94%E8%AE%B0",
	}
	for path, want := range cases {
		if got := fileURI(path); got != want {
			t.Errorf("fileURI(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestShowItemsCommands(t *testing.T) {
	got := showItemsCommands("file:///data/a%2Cb")
	want := [][]string{
		{"dbus-send", "--session", "--print-reply", "--type=method_call", "--reply-timeout=5000",
			"--dest=org.freedesktop.FileManager1", "/org/freedesktop/FileManager1",
			"org.freedesktop.FileManager1.ShowItems", "array:string:file:///data/a%2Cb", "string:"},
		{"gdbus", "call", "--session", "--timeout", "5",
			"--dest", "org.freedesktop.FileManager1", "--object-path", "/org/freedesktop/FileManager1",
			"--method", "org.freedesktop.FileManager1.ShowItems", "['file:///data/a%2Cb']", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("showItemsCommands =\n%q\nwant\n%q", got, want)
	}
}

func TestStartHelper(t *testing.T) {
	if err := startHelper("true"); err != nil {
		t.Errorf("helper that succeeds: %v", err)
	}
	if err := startHelper("false"); err == nil {
		t.Error("helper that fails at once returned nil")
	}
	if err := startHelper("watchink-no-such-helper"); err == nil {
		t.Error("missing helper returned nil")
	}
	// A helper that stays in the foreground must not block the caller.
	begin := time.Now()
	if err := startHelper("sleep", "4"); err != nil {
		t.Errorf("long-running helper: %v", err)
	}
	if took := time.Since(begin); took > helperGrace+time.Second {
		t.Errorf("startHelper blocked for %v", took)
	}
}
