//go:build !darwin && !linux && !windows

package asr

import "runtime"

// physicalCores falls back to the logical CPU count.
//
// TODO(linux): count distinct core ids from /sys/devices/system/cpu.
// TODO(windows): GetLogicalProcessorInformation.
func physicalCores() int { return runtime.NumCPU() }
