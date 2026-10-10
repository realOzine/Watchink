// Package tools prepares the external programs and models Watchink needs
// (docs/v0.md D-06, §11): it downloads pinned releases listed in the built-in
// manifest, verifies their sha256, installs them atomically under the data
// directory, and extracts the whisper-cli binary embedded in the executable.
//
// The package never starts a process itself (§4.3): smoke tests run through an
// injected ExecFunc, and only after the file being run passed verification.
package tools

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

//go:embed manifest.json
var manifestJSON []byte

// Archive kinds understood by the installer.
const (
	ArchiveNone  = "none" // the download is the file itself
	ArchiveZip   = "zip"
	ArchiveTarGz = "tar.gz"
	ArchiveTarXz = "tar.xz" // TODO(linux): not needed on darwin, not implemented
)

// Manifest is the pinned list of tools and models (§11.1).
type Manifest struct {
	ManifestVersion int              `json:"manifest_version"`
	Tools           map[string]Tool  `json:"tools"`
	Models          map[string]Model `json:"models"`
}

// Tool is one external program.
type Tool struct {
	Version string `json:"version"`
	License string `json:"license,omitempty"`
	// Lazy tools are only fetched when a source needs them (deno → YouTube).
	Lazy bool `json:"lazy,omitempty"`
	// Embedded tools ship inside the executable and are extracted, not
	// downloaded.
	Embedded bool `json:"embedded,omitempty"`
	// SmokeArgs are the arguments of the post-install smoke test; the default
	// is ["--version"].
	SmokeArgs []string `json:"smoke_args,omitempty"`
	// Update describes where newer upstream releases are found (§11.5).
	Update    *UpdateSource       `json:"update,omitempty"`
	Platforms map[string]Platform `json:"platforms,omitempty"`
}

// Platform is the download of a tool for one "<os>-<arch>".
type Platform struct {
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size,omitempty"`
	Archive string `json:"archive"`
	// Files maps a path inside the archive (for "none": the download's file
	// name) to the file name it is installed under.
	Files map[string]string `json:"files"`
	// Bin is the installed file name of the executable; it defaults to the
	// tool name.
	Bin string `json:"bin,omitempty"`
}

// UpdateSource locates the latest upstream release of a tool.
type UpdateSource struct {
	// LatestURL redirects to ".../tag/<version>".
	LatestURL string `json:"latest_url"`
	// DownloadURL is a template with {version} and {asset}.
	DownloadURL string `json:"download_url"`
	// Checksums is the asset name of the sha256 list of a release.
	Checksums string `json:"checksums"`
}

// Model is one model file.
type Model struct {
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	File    string `json:"file,omitempty"`
	License string `json:"license,omitempty"`
}

// FileName returns the name the model is stored under.
func (m Model) FileName() string {
	if m.File != "" {
		return m.File
	}
	return path.Base(urlPath(m.URL))
}

var (
	nameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)
	sha256RE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// DefaultManifest returns the manifest built into the program.
func DefaultManifest() (*Manifest, error) { return ParseManifest(manifestJSON) }

// ParseManifest decodes and validates a manifest. Validation matters because
// names, versions and file names become path components.
func ParseManifest(data []byte) (*Manifest, error) {
	var mf Manifest
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil, fmt.Errorf("tools: parse manifest: %w", err)
	}
	if mf.ManifestVersion != 1 {
		return nil, fmt.Errorf("tools: unsupported manifest_version %d", mf.ManifestVersion)
	}
	for name, t := range mf.Tools {
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("tools: invalid tool name %q", name)
		}
		if _, dup := mf.Models[name]; dup {
			return nil, fmt.Errorf("tools: %q is both a tool and a model", name)
		}
		if !versionRE.MatchString(t.Version) {
			return nil, fmt.Errorf("tools: %s: invalid version %q", name, t.Version)
		}
		for plat, p := range t.Platforms {
			if err := p.validate(); err != nil {
				return nil, fmt.Errorf("tools: %s/%s: %w", name, plat, err)
			}
		}
	}
	for name, md := range mf.Models {
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("tools: invalid model name %q", name)
		}
		if !sha256RE.MatchString(md.SHA256) {
			return nil, fmt.Errorf("tools: %s: invalid sha256", name)
		}
		if f := md.FileName(); !safeFileName(f) {
			return nil, fmt.Errorf("tools: %s: invalid file name %q", name, f)
		}
	}
	return &mf, nil
}

func (p Platform) validate() error {
	if !sha256RE.MatchString(p.SHA256) {
		return fmt.Errorf("invalid sha256")
	}
	switch p.Archive {
	case ArchiveNone, ArchiveZip, ArchiveTarGz, ArchiveTarXz:
	default:
		return fmt.Errorf("unknown archive kind %q", p.Archive)
	}
	if len(p.Files) == 0 {
		return fmt.Errorf("no files")
	}
	if p.Archive == ArchiveNone && len(p.Files) != 1 {
		return fmt.Errorf("archive \"none\" needs exactly one file")
	}
	for _, dst := range p.Files {
		if !safeFileName(dst) {
			return fmt.Errorf("invalid installed file name %q", dst)
		}
	}
	if p.Bin != "" && !safeFileName(p.Bin) {
		return fmt.Errorf("invalid bin %q", p.Bin)
	}
	return nil
}

// safeFileName accepts a plain file name without any directory part.
func safeFileName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`) && filepath.IsLocal(s)
}

// CurrentPlatform is the manifest platform key of the running program.
func CurrentPlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// exeName returns the file name of the executable `name` on a manifest
// platform: Windows only runs files with an executable extension. Downloaded
// tools state their file names in the manifest (Platform.Bin); this is for
// the embedded ones, whose names are derived.
func exeName(platform, name string) string {
	if strings.HasPrefix(platform, "windows-") {
		return name + ".exe"
	}
	return name
}

// toolOrder is the display order of the well-known tools; others follow
// alphabetically.
var toolOrder = map[string]int{"ffmpeg": 1, "ffprobe": 2, "whisper-cli": 3, "yt-dlp": 4, "deno": 5}

func (mf *Manifest) toolNames() []string {
	names := make([]string, 0, len(mf.Tools))
	for n := range mf.Tools {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		oi, oj := toolOrder[names[i]], toolOrder[names[j]]
		if oi == 0 {
			oi = 100
		}
		if oj == 0 {
			oj = 100
		}
		if oi != oj {
			return oi < oj
		}
		return names[i] < names[j]
	})
	return names
}

func (mf *Manifest) modelNames() []string {
	names := make([]string, 0, len(mf.Models))
	for n := range mf.Models {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
