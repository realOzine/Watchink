//go:build linux

package sysreq

import (
	"os"
	"runtime"
)

// Desktop reports the system libraries of the desktop window.
func Desktop() []Requirement {
	libDirs := linuxLibDirs(runtime.GOARCH, os.Getenv)
	return check(linuxDesktop, libDirs, linuxPluginDirs(libDirs, os.Getenv))
}
