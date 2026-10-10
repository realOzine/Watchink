//go:build linux

package procx

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// helperGrace is how long a desktop helper (xdg-open) is watched for an
	// early failure before it is left running on its own.
	helperGrace = 2 * time.Second
	// dbusTimeout bounds the FileManager1 call, activation included.
	dbusTimeout = 5 * time.Second
)

// setProcessGroup makes the child the leader of a new process group, so one
// signal to -pid reaches the tool and everything it spawned, and asks the
// kernel to SIGKILL it when the engine dies (§5.5). No guard process is
// needed on Linux.
//
// Pdeathsig is tied to the thread that forked, not to the process. That is
// fine here: the Go runtime only ends an OS thread when a goroutine exits
// while locked to it, and Run is never called from such a goroutine.
//
// Pdeathsig reaches the direct child only. Descendants that outlive it are
// not signalled by the kernel; see docs/spikes/v0-linux.md.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// deletedSuffix is what the kernel appends to /proc/<pid>/exe when the file
// was unlinked or replaced while the process runs (a tool update does that).
const deletedSuffix = " (deleted)"

// processExe returns the executable of pid as /proc/<pid>/exe names it. ok is
// false when the process does not exist, is a zombie, or belongs to another
// user.
func processExe(pid int) (link, exe string, ok bool) {
	link = "/proc/" + strconv.Itoa(pid) + "/exe"
	target, err := os.Readlink(link)
	if err != nil || target == "" {
		return "", "", false
	}
	return link, strings.TrimSuffix(target, deletedSuffix), true
}

// KillIfMatches kills a child left over from a previous engine, but only when
// the process with that pid still runs the recorded executable (§7.6 step 3):
// a recycled pid must never be killed. It returns true when a kill was sent.
//
// Children are recorded with the tool's own path on Linux (there is no
// guard), so a process running the current executable is never a leftover
// child: the engine, CLI and desktop processes are never hit.
func KillIfMatches(pid int, exe string) (bool, error) {
	if pid <= 1 || pid == os.Getpid() || pid == os.Getppid() {
		return false, nil
	}
	link, actual, ok := processExe(pid)
	// The link itself is compared too: stat follows it to the running inode
	// even when the path it prints no longer resolves.
	if !ok || !(samePath(actual, exe) || samePath(link, exe)) {
		return false, nil
	}
	if self, err := os.Executable(); err == nil && (samePath(actual, self) || samePath(link, self)) {
		return false, nil
	}
	return killTree(pid)
}

// GuardMain is the `watchink _guard` subcommand, which only macOS uses: on
// Linux the kernel ends the children of a dead engine through Pdeathsig
// (§5.5), so the Runner never starts a guard and this only reports misuse.
func GuardMain(args []string) int {
	fmt.Fprintln(os.Stderr, "watchink _guard is only used on macOS; Linux relies on Pdeathsig")
	return 2
}

// openURL hands the URL to the desktop's default handler.
func openURL(u string) error { return startHelper("xdg-open", u) }

// revealPath asks the file manager to select path through the
// org.freedesktop.FileManager1 D-Bus interface (Nautilus, Dolphin, Nemo,
// Thunar … implement it). Without a D-Bus client or a file manager that
// answers, the parent directory is opened instead.
func revealPath(path string) error {
	for _, argv := range showItemsCommands(fileURI(path)) {
		exe, err := exec.LookPath(argv[0])
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), dbusTimeout)
		err = exec.CommandContext(ctx, exe, argv[1:]...).Run()
		cancel()
		if err == nil {
			return nil
		}
	}
	return startHelper("xdg-open", filepath.Dir(path))
}

// showItemsCommands returns the argv of the FileManager1.ShowItems call for
// each supported D-Bus client, in order of preference.
func showItemsCommands(uri string) [][]string {
	const (
		dest   = "org.freedesktop.FileManager1"
		object = "/org/freedesktop/FileManager1"
		method = "org.freedesktop.FileManager1.ShowItems"
	)
	return [][]string{
		{"dbus-send", "--session", "--print-reply", "--type=method_call",
			"--reply-timeout=" + strconv.FormatInt(dbusTimeout.Milliseconds(), 10),
			"--dest=" + dest, object, method, "array:string:" + uri, "string:"},
		{"gdbus", "call", "--session", "--timeout", strconv.Itoa(int(dbusTimeout.Seconds())),
			"--dest", dest, "--object-path", object, "--method", method, "['" + uri + "']", ""},
	}
}

// fileURI returns the file:// URI of an absolute path. Besides the usual
// percent-encoding (which already covers quotes and backslashes), commas are
// escaped: dbus-send splits array elements on them.
func fileURI(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	return strings.ReplaceAll(u.String(), ",", "%2C")
}

// startHelper starts a desktop helper detached from this process (own
// session, stdio on /dev/null) and reports a failure only when the helper
// exits with an error right away. xdg-open may stay in the foreground for as
// long as the program it launched runs, so it cannot simply be waited for.
func startHelper(name string, args ...string) error {
	exe, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("procx: %s not found (install xdg-utils): %w", name, err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("procx: %s: %w", name, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }() // also reaps the helper later on
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("procx: %s: %w", name, err)
		}
	case <-time.After(helperGrace):
	}
	return nil
}
