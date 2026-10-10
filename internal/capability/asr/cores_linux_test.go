//go:build linux

package asr

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// fakeSysfs builds a cpu topology tree: one [package, core] pair per logical
// CPU; a nil pair is an offline CPU without a topology directory.
func fakeSysfs(t *testing.T, cpus [][]string) string {
	t.Helper()
	root := t.TempDir()
	for i, c := range cpus {
		dir := filepath.Join(root, "cpu"+strconv.Itoa(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if c == nil {
			continue
		}
		topo := filepath.Join(dir, "topology")
		if err := os.Mkdir(topo, 0o755); err != nil {
			t.Fatal(err)
		}
		if c[0] != "" {
			if err := os.WriteFile(filepath.Join(topo, "physical_package_id"), []byte(c[0]+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(topo, "core_id"), []byte(c[1]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Siblings of the cpuN directories that must be ignored.
	for _, d := range []string{"cpufreq", "cpuidle", "cpu"} {
		if err := os.MkdirAll(filepath.Join(root, d, "topology"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCountPhysicalCores(t *testing.T) {
	cases := []struct {
		name string
		cpus [][]string
		want int
	}{
		{"4 cores, 8 threads", [][]string{{"0", "0"}, {"0", "1"}, {"0", "2"}, {"0", "3"}, {"0", "0"}, {"0", "1"}, {"0", "2"}, {"0", "3"}}, 4},
		{"two sockets reuse core ids", [][]string{{"0", "0"}, {"0", "1"}, {"1", "0"}, {"1", "1"}}, 4},
		{"offline cpu", [][]string{{"0", "0"}, nil, {"0", "1"}}, 2},
		{"no package id", [][]string{{"", "0"}, {"", "1"}, {"", "1"}}, 2},
		{"no topology at all", [][]string{nil, nil}, 0},
	}
	for _, c := range cases {
		if got := countPhysicalCores(fakeSysfs(t, c.cpus)); got != c.want {
			t.Errorf("%s: %d cores, want %d", c.name, got, c.want)
		}
	}
	if got := countPhysicalCores(filepath.Join(t.TempDir(), "missing")); got != 0 {
		t.Errorf("missing root: %d", got)
	}
}

func TestPhysicalCoresOnThisMachine(t *testing.T) {
	if n := physicalCores(); n < 1 || n > runtime.NumCPU() {
		t.Errorf("physicalCores() = %d with %d logical CPUs", n, runtime.NumCPU())
	}
}
