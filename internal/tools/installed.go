package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// installedFile is the content of <data>/tools/installed.json (§11.3 step 7):
// the record of what passed verification and which version is in use.
type installedFile struct {
	Version int                        `json:"version"`
	Tools   map[string]*installedTool  `json:"tools"`
	Models  map[string]*installedModel `json:"models"`
}

type installedTool struct {
	// Active is the version Path returns.
	Active string `json:"active"`
	// Previous is the version that was active before the last switch; it is
	// what Rollback returns to (§11.5).
	Previous string `json:"previous,omitempty"`
	// Pinned is set when the user chose Active (update / rollback), so it
	// wins over the manifest version.
	Pinned   bool                         `json:"pinned,omitempty"`
	Versions map[string]*installedVersion `json:"versions"`
}

type installedVersion struct {
	// SHA256 is the hash of the verified download (or embedded binary).
	SHA256 string `json:"sha256"`
	// Bin is the executable's file name inside the version directory.
	Bin         string    `json:"bin"`
	BinSHA256   string    `json:"bin_sha256"`
	BinSize     int64     `json:"bin_size"`
	Size        int64     `json:"size"` // all installed files
	Source      string    `json:"source"`
	InstalledAt time.Time `json:"installed_at"`
}

type installedModel struct {
	File        string    `json:"file"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	Source      string    `json:"source"`
	InstalledAt time.Time `json:"installed_at"`
}

func newInstalledFile() *installedFile {
	return &installedFile{Version: 1, Tools: map[string]*installedTool{}, Models: map[string]*installedModel{}}
}

func (m *Manager) installedPath() string { return filepath.Join(m.toolsDir(), "installed.json") }

// loadInstalled reads installed.json. A missing or unreadable file is treated
// as "nothing installed": the worst case is that verified items are installed
// again.
func (m *Manager) loadInstalled() *installedFile {
	data, err := os.ReadFile(m.installedPath())
	if err != nil {
		return newInstalledFile()
	}
	f := newInstalledFile()
	if err := json.Unmarshal(data, f); err != nil {
		m.log.Warn("tools: installed.json is corrupt, ignoring it", "err", err)
		return newInstalledFile()
	}
	if f.Tools == nil {
		f.Tools = map[string]*installedTool{}
	}
	if f.Models == nil {
		f.Models = map[string]*installedModel{}
	}
	for _, t := range f.Tools {
		if t != nil && t.Versions == nil {
			t.Versions = map[string]*installedVersion{}
		}
	}
	return f
}

// updateInstalled applies fn to installed.json and writes it back atomically
// (temp file + fsync + rename), so a crash leaves either the old or the new
// file, never a partial one.
func (m *Manager) updateInstalled(fn func(*installedFile)) error {
	m.instMu.Lock()
	defer m.instMu.Unlock()
	f := m.loadInstalled()
	fn(f)
	f.Version = 1
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return ioErr(err, "写入 installed.json")
	}
	return ioErr(writeFileAtomic(m.installedPath(), append(data, '\n'), 0o644), "写入 installed.json")
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return rename(tmp.Name(), path)
}
