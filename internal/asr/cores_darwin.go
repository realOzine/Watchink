//go:build darwin

package asr

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// physicalCores reads hw.physicalcpu through sysctl(3), not a subprocess.
func physicalCores() int {
	if n, err := unix.SysctlUint32("hw.physicalcpu"); err == nil && n > 0 {
		return int(n)
	}
	return runtime.NumCPU()
}
