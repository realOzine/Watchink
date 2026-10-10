// Package sysreq checks the system libraries the desktop window needs at
// runtime (docs/v0.md D-11, §16.3). Only Linux has any: the WebView and the
// media decoders come from distribution packages there, while macOS ships
// WKWebView with the system. The check only looks at files; it never starts
// a process (§4.3).
package sysreq

import (
	"os"
	"path/filepath"
)

// Requirement is one system library and whether it was found.
type Requirement struct {
	// Name says what the library is for.
	Name string
	// Package is the Ubuntu 24.04 package that provides it.
	Package string
	// Optional requirements do not keep the window from opening: without
	// them only video playback inside it fails (D-11).
	Optional bool
	// Found is filled in by the check.
	Found bool

	file   string // file name of the shared object
	plugin bool   // a GStreamer plugin, looked up in the plugin directories
}

// linuxDesktop is the §16.3 list for Linux.
var linuxDesktop = []Requirement{
	{Name: "WebKitGTK 4.1", Package: "libwebkit2gtk-4.1-0", file: "libwebkit2gtk-4.1.so.0"},
	{Name: "GTK 3", Package: "libgtk-3-0t64", file: "libgtk-3.so.0"},
	{Name: "GStreamer good 插件（MP4 解封装）", Package: "gstreamer1.0-plugins-good", Optional: true, file: "libgstisomp4.so", plugin: true},
	{Name: "GStreamer libav 插件（H.264 / AAC 解码）", Package: "gstreamer1.0-libav", Optional: true, file: "libgstlibav.so", plugin: true},
}

// multiarch maps GOARCH to the Debian multiarch directory name.
var multiarch = map[string]string{"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu"}

// linuxLibDirs lists where shared libraries are installed: the directories
// of LD_LIBRARY_PATH, then the Debian/Ubuntu multiarch directories, then the
// lib64 / lib layout of other distributions.
func linuxLibDirs(goarch string, getenv func(string) string) []string {
	dirs := filepath.SplitList(getenv("LD_LIBRARY_PATH"))
	if triplet := multiarch[goarch]; triplet != "" {
		dirs = append(dirs, "/usr/lib/"+triplet, "/lib/"+triplet, "/usr/local/lib/"+triplet)
	}
	return append(dirs, "/usr/lib64", "/usr/lib", "/usr/local/lib", "/lib64", "/lib")
}

// linuxPluginDirs lists where GStreamer 1.0 looks for plugins: its two path
// variables, then gstreamer-1.0 under every library directory.
func linuxPluginDirs(libDirs []string, getenv func(string) string) []string {
	dirs := append(filepath.SplitList(getenv("GST_PLUGIN_PATH")), filepath.SplitList(getenv("GST_PLUGIN_SYSTEM_PATH"))...)
	for _, d := range libDirs {
		dirs = append(dirs, filepath.Join(d, "gstreamer-1.0"))
	}
	return dirs
}

// check returns a copy of reqs with Found filled in.
func check(reqs []Requirement, libDirs, pluginDirs []string) []Requirement {
	out := make([]Requirement, len(reqs))
	for i, r := range reqs {
		dirs := libDirs
		if r.plugin {
			dirs = pluginDirs
		}
		for _, d := range dirs {
			if d == "" {
				continue
			}
			if st, err := os.Stat(filepath.Join(d, r.file)); err == nil && !st.IsDir() {
				r.Found = true
				break
			}
		}
		out[i] = r
	}
	return out
}
