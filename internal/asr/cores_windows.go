//go:build windows

package asr

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetLogicalProcessorInformation = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetLogicalProcessorInformation")

// relationProcessorCore is the Relationship value of an entry that describes
// one physical core.
const relationProcessorCore = 0

// logicalProcessorInfo is SYSTEM_LOGICAL_PROCESSOR_INFORMATION. The trailing
// union (cache, NUMA node, …) is two ULONGLONGs wide and is not read.
type logicalProcessorInfo struct {
	ProcessorMask uintptr
	Relationship  uint32
	_             [2]uint64
}

// physicalCores counts the RelationProcessorCore entries reported by
// GetLogicalProcessorInformation, not a subprocess. The call only covers the
// processor group of the calling thread (at most 64 logical processors),
// which is also what whisper-cli's threads are scheduled on by default.
func physicalCores() int {
	if n := countPhysicalCores(); n > 0 {
		return n
	}
	return runtime.NumCPU()
}

func countPhysicalCores() int {
	if procGetLogicalProcessorInformation.Find() != nil {
		return 0
	}
	// The first call reports the buffer size it needs.
	var size uint32
	procGetLogicalProcessorInformation.Call(0, uintptr(unsafe.Pointer(&size)))
	entry := uint32(unsafe.Sizeof(logicalProcessorInfo{}))
	if size < entry {
		return 0
	}
	buf := make([]logicalProcessorInfo, size/entry)
	size = uint32(len(buf)) * entry
	ok, _, _ := procGetLogicalProcessorInformation.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if ok == 0 {
		return 0
	}
	cores := 0
	for _, info := range buf[:size/entry] {
		if info.Relationship == relationProcessorCore {
			cores++
		}
	}
	return cores
}
