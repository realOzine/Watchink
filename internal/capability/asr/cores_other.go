//go:build !darwin && !linux && !windows

package asr

import "runtime"

// physicalCores falls back to the logical CPU count.
func physicalCores() int { return runtime.NumCPU() }
