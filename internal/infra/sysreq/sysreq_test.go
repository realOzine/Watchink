package sysreq

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func found(reqs []Requirement) map[string]bool {
	out := map[string]bool{}
	for _, r := range reqs {
		out[r.Package] = r.Found
	}
	return out
}

func TestCheckLinuxDesktop(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "usr/lib/x86_64-linux-gnu")
	libDirs := []string{"", filepath.Join(root, "missing"), lib}
	pluginDirs := linuxPluginDirs(libDirs, func(string) string { return "" })

	// Nothing installed.
	got := check(linuxDesktop, libDirs, pluginDirs)
	if len(got) != 4 {
		t.Fatalf("%d requirements, want four", len(got))
	}
	for _, r := range got {
		if r.Found {
			t.Errorf("%s reported as found in an empty tree", r.Package)
		}
		if r.Name == "" || r.Package == "" {
			t.Errorf("incomplete requirement %+v", r)
		}
	}

	// The WebView is there, the decoders are not.
	touch(t, filepath.Join(lib, "libwebkit2gtk-4.1.so.0"))
	touch(t, filepath.Join(lib, "libgtk-3.so.0"))
	// A plugin file outside the plugin directory, and a directory with the
	// right name, must not count.
	touch(t, filepath.Join(lib, "libgstlibav.so"))
	if err := os.MkdirAll(filepath.Join(lib, "gstreamer-1.0", "libgstisomp4.so"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"libwebkit2gtk-4.1-0": true, "libgtk-3-0t64": true, "gstreamer1.0-plugins-good": false, "gstreamer1.0-libav": false}
	if got := found(check(linuxDesktop, libDirs, pluginDirs)); !reflect.DeepEqual(got, want) {
		t.Errorf("found = %v, want %v", got, want)
	}

	// Decoders in a GST_PLUGIN_PATH directory.
	extra := filepath.Join(root, "plugins")
	touch(t, filepath.Join(extra, "libgstisomp4.so"))
	touch(t, filepath.Join(extra, "libgstlibav.so"))
	pluginDirs = linuxPluginDirs(libDirs, func(k string) string {
		if k == "GST_PLUGIN_PATH" {
			return extra
		}
		return ""
	})
	for pkg, ok := range found(check(linuxDesktop, libDirs, pluginDirs)) {
		if !ok {
			t.Errorf("%s not found with everything installed", pkg)
		}
	}
	// The shared list is not modified, and only the decoders are optional.
	for i, r := range linuxDesktop {
		if r.Found {
			t.Errorf("check wrote Found into the shared list (%s)", r.Package)
		}
		if r.Optional != (i >= 2) {
			t.Errorf("%s: Optional = %v", r.Package, r.Optional)
		}
	}
}

func TestLinuxLibDirs(t *testing.T) {
	env := func(k string) string {
		if k == "LD_LIBRARY_PATH" {
			return "/opt/a" + string(filepath.ListSeparator) + "/opt/b"
		}
		return ""
	}
	none := func(string) string { return "" }
	got := linuxLibDirs("amd64", env)
	want := []string{"/opt/a", "/opt/b", "/usr/lib/x86_64-linux-gnu", "/lib/x86_64-linux-gnu", "/usr/local/lib/x86_64-linux-gnu", "/usr/lib64", "/usr/lib", "/usr/local/lib", "/lib64", "/lib"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("amd64 dirs = %v", got)
	}
	if got := linuxLibDirs("arm64", none); got[0] != "/usr/lib/aarch64-linux-gnu" {
		t.Errorf("arm64 dirs = %v", got)
	}
	if got := linuxLibDirs("riscv64", none); got[0] != "/usr/lib64" {
		t.Errorf("unknown arch dirs = %v", got)
	}
}

func TestDesktopOnThisPlatform(t *testing.T) {
	got := Desktop()
	if runtime.GOOS == "linux" {
		if len(got) != len(linuxDesktop) {
			t.Errorf("Desktop() = %+v", got)
		}
		return
	}
	if got != nil {
		t.Errorf("Desktop() = %+v, want nil outside Linux", got)
	}
}
