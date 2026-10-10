//go:build linux

package asr

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// cpuSysfs is where the kernel describes the CPU topology.
const cpuSysfs = "/sys/devices/system/cpu"

// physicalCores counts the physical cores from sysfs (no subprocess), falling
// back to the logical CPU count when the topology is not exposed, as in some
// containers and VMs.
func physicalCores() int {
	// Never more than the CPUs this process may run on (affinity, cpusets).
	if n := countPhysicalCores(cpuSysfs); n > 0 {
		return min(n, runtime.NumCPU())
	}
	return runtime.NumCPU()
}

// countPhysicalCores returns the number of distinct (physical_package_id,
// core_id) pairs among the cpuN directories under root, so SMT siblings count
// once. Offline CPUs have no topology directory and are skipped. It returns 0
// when nothing could be read.
func countPhysicalCores(root string) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	cores := map[[2]string]struct{}{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "cpu") || len(name) == 3 || strings.Trim(name[3:], "0123456789") != "" {
			continue // cpufreq, cpuidle, …
		}
		topo := filepath.Join(root, name, "topology")
		core, err := os.ReadFile(filepath.Join(topo, "core_id"))
		if err != nil {
			continue
		}
		// A missing package id (seen on some ARM kernels) is treated as one
		// package rather than as a reason to drop the CPU.
		pkg, _ := os.ReadFile(filepath.Join(topo, "physical_package_id"))
		cores[[2]string{strings.TrimSpace(string(pkg)), strings.TrimSpace(string(core))}] = struct{}{}
	}
	return len(cores)
}
