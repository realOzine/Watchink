//go:build !windows

package procx

import "os/exec"

// attachGroup and releaseGroup bracket the life of a child's process group.
// On Unix the group is just the child's pgid, created by setProcessGroup at
// fork time, so there is nothing to set up or free; Windows keeps a Job
// Object handle per child (sys_windows.go).
func attachGroup(cmd *exec.Cmd) error { return nil }

func releaseGroup(pid int) {}
