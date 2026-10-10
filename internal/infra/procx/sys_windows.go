//go:build windows

package procx

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has neither process groups that can be signalled nor SIGTERM. The
// "group" of a child is a Job Object: every child is started
// suspended, assigned to a fresh job created with KILL_ON_JOB_CLOSE, and only
// then resumed, so nothing it spawns can escape the job. The engine holds the
// only handle to the job: cancelling is TerminateJobObject, and when the
// engine dies — however abruptly — the handle is closed by the system, which
// terminates the whole tree. No `_guard` wrapper is needed.

// killExitCode is the exit status given to terminated processes; it matches
// what os.Process.Kill uses.
const killExitCode = 1

// job is the Job Object of one live child.
type job struct {
	handle windows.Handle
	// process keeps the child's process object referenced until releaseGroup,
	// so its pid cannot be reused while the pid is a key of jobs.
	process windows.Handle
}

// jobs maps the pid of every live Runner child to its Job Object. The Runner
// identifies groups by the leader's pid on every platform; here the pid is
// only a key.
var jobs = struct {
	sync.Mutex
	m map[int]*job
}{m: map[int]*job{}}

// setProcessGroup prepares the child to be put into a job by attachGroup:
// it starts suspended, and without a console window of its own (the engine
// usually has no console, and a console tool would otherwise pop one up).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW,
	}
}

// attachGroup puts the freshly started (still suspended) child into a new
// KILL_ON_JOB_CLOSE job and lets it run. On error the caller kills the child.
func attachGroup(cmd *exec.Cmd) error {
	pid := cmd.Process.Pid
	const access = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE |
		windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE
	process, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("procx: open child %d: %w", pid, err)
	}
	handle, err := newKillOnCloseJob()
	if err != nil {
		windows.CloseHandle(process)
		return err
	}
	if err := windows.AssignProcessToJobObject(handle, process); err != nil {
		windows.CloseHandle(handle)
		windows.CloseHandle(process)
		return fmt.Errorf("procx: assign child %d to job: %w", pid, err)
	}
	jobs.Lock()
	jobs.m[pid] = &job{handle: handle, process: process}
	jobs.Unlock()
	if err := resumeProcess(pid); err != nil {
		releaseGroup(pid) // closing the job kills the suspended child
		return err
	}
	return nil
}

// releaseGroup closes the job of a finished child. Anything still inside is
// terminated by KILL_ON_JOB_CLOSE.
func releaseGroup(pid int) {
	jobs.Lock()
	j := jobs.m[pid]
	delete(jobs.m, pid)
	jobs.Unlock()
	if j != nil {
		windows.CloseHandle(j.handle)
		windows.CloseHandle(j.process)
	}
}

func newKillOnCloseJob() (windows.Handle, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("procx: create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(handle, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(handle)
		return 0, fmt.Errorf("procx: configure job object: %w", err)
	}
	return handle, nil
}

// resumeProcess resumes the threads of a process created with
// CREATE_SUSPENDED. os/exec does not expose the main thread handle, so the
// (single) thread is found through a thread snapshot.
func resumeProcess(pid int) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("procx: thread snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	resumed := 0
	for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		thread, terr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if terr != nil {
			return fmt.Errorf("procx: open thread of child %d: %w", pid, terr)
		}
		_, terr = windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		if terr != nil {
			return fmt.Errorf("procx: resume child %d: %w", pid, terr)
		}
		resumed++
	}
	// Thread32Next ends the walk with ERROR_NO_MORE_FILES.
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("procx: thread snapshot: %w", err)
	}
	if resumed == 0 {
		return fmt.Errorf("procx: child %d has no thread to resume", pid)
	}
	return nil
}

// terminateGroup is the Windows half of a cancel. There is no SIGTERM to
// send, so the polite step and the forceful one are the same call.
func terminateGroup(pid int) error { return killGroup(pid) }

// killGroup terminates every process in the child's job. A pid without a job
// (already released) is not an error.
func killGroup(pid int) error {
	jobs.Lock()
	defer jobs.Unlock()
	j := jobs.m[pid]
	if j == nil {
		return nil
	}
	return windows.TerminateJobObject(j.handle, killExitCode)
}

// jobAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// groupAlive reports whether any process is left in the child's job.
func groupAlive(pid int) bool {
	jobs.Lock()
	defer jobs.Unlock()
	j := jobs.m[pid]
	if j == nil {
		return false
	}
	var info jobAccounting
	err := windows.QueryInformationJobObject(j.handle, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	return err == nil && info.ActiveProcesses > 0
}

// processImage returns the full path of the executable pid runs.
func processImage(process windows.Handle) (string, bool) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(process, 0, &buf[0], &size); err != nil {
		return "", false
	}
	return windows.UTF16ToString(buf[:size]), true
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	if strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) {
		return true
	}
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(sa, sb)
}

// GuardMain is the `_guard` subcommand, which only exists on macOS. Windows
// does not wrap children: their Job Object dies with the engine.
func GuardMain(args []string) int {
	fmt.Fprintln(os.Stderr, "watchink _guard is only available on macOS")
	return 2
}

// SpawnDetached starts a fully detached background process: no console, its
// own process group (so a Ctrl+C in the caller's console does not reach it),
// stdio on NUL. It is used to launch `watchink engine run` from a client.
// A nil env inherits the caller's environment. The process is not
// waited for.
//
// A client may itself run inside a job that kills its members when the
// client's host exits (terminals and MCP hosts do this), and the engine must
// outlive its clients. So the engine is first started outside that
// job; hosts that forbid breaking away make CreateProcess fail with
// ERROR_ACCESS_DENIED, in which case it is started inside the job after all.
func SpawnDetached(exe string, args []string, env []string) error {
	const flags = windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW
	err := spawnDetached(exe, args, env, flags|windows.CREATE_BREAKAWAY_FROM_JOB)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		err = spawnDetached(exe, args, env, flags)
	}
	if err != nil {
		return fmt.Errorf("procx: spawn %s: %w", filepath.Base(exe), err)
	}
	return nil
}

func spawnDetached(exe string, args []string, env []string, flags uint32) error {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// OpenBrowser opens an http(s) URL in the default browser.
func OpenBrowser(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("procx: refusing to open non-http URL")
	}
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return fmt.Errorf("procx: open: %w", err)
	}
	// rundll32 hands the URL to the shell's protocol handler and exits; no
	// cmd.exe is involved, so nothing in the URL is interpreted.
	cmd := exec.Command(filepath.Join(sys, "rundll32.exe"), "url.dll,FileProtocolHandler", u.String())
	return startAndForget(cmd, "open")
}

// RevealInFileManager selects path in Explorer. The caller is responsible for
// restricting path to the data directory.
func RevealInFileManager(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("procx: reveal needs an absolute path")
	}
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	path = filepath.Clean(path)
	if strings.ContainsRune(path, '"') {
		return fmt.Errorf("procx: reveal: invalid path")
	}
	win, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		return fmt.Errorf("procx: reveal: %w", err)
	}
	explorer := filepath.Join(win, "explorer.exe")
	cmd := exec.Command(explorer)
	// Explorer parses its own command line: the path must be quoted after
	// "/select," and not as a whole argument, which is how Go would quote a
	// path containing spaces.
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + explorer + `" /select,"` + path + `"`}
	return startAndForget(cmd, "reveal")
}

// startAndForget starts a short-lived shell helper without waiting for it:
// explorer.exe exits with status 1 even on success, and neither helper
// reports anything useful.
func startAndForget(cmd *exec.Cmd, what string) error {
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("procx: %s: %w", what, err)
	}
	return cmd.Process.Release()
}
