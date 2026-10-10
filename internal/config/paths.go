// Package config resolves the data and config directories and loads the
// settings and secrets files (docs/v0.md §6.1, §12).
package config

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/pelletier/go-toml/v2"
)

// EnvDataDir overrides the data directory.
const EnvDataDir = "WATCHINK_DATA_DIR"

// EnvConfigDir overrides the user config directory (tests and isolated runs).
const EnvConfigDir = "WATCHINK_CONFIG_DIR"

// ConfigDir returns <cfg>, the per-user directory holding secrets.toml and
// the optional location.toml.
func ConfigDir() (string, error) {
	if d := os.Getenv(EnvConfigDir); d != "" {
		return filepath.Abs(d)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Watchink"), nil
	case "windows":
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "Watchink"), nil
		}
		return filepath.Join(home, "AppData", "Roaming", "Watchink"), nil
	default:
		if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
			return filepath.Join(d, "watchink"), nil
		}
		return filepath.Join(home, ".config", "watchink"), nil
	}
}

// DefaultDataDir returns the platform default for <data>.
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Watchink", "Data"), nil
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "Watchink", "Data"), nil
		}
		return filepath.Join(home, "AppData", "Local", "Watchink", "Data"), nil
	default:
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			return filepath.Join(d, "watchink"), nil
		}
		return filepath.Join(home, ".local", "share", "watchink"), nil
	}
}

// LocationFile is <cfg>/location.toml, which may point at a custom data dir.
func LocationFile() (string, error) {
	cfg, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "location.toml"), nil
}

type locationFile struct {
	DataDir string `toml:"data_dir"`
}

// ResolveDataDir picks the data directory: flag > WATCHINK_DATA_DIR >
// location.toml > platform default. The result is absolute; the directory is
// not created.
func ResolveDataDir(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	if d := os.Getenv(EnvDataDir); d != "" {
		return filepath.Abs(d)
	}
	if p, err := LocationFile(); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			var loc locationFile
			if toml.Unmarshal(b, &loc) == nil && loc.DataDir != "" {
				return filepath.Abs(loc.DataDir)
			}
		}
	}
	return DefaultDataDir()
}

// WriteLocation records a custom data dir in <cfg>/location.toml
// (`config set-data-dir`). An empty dir removes the file.
func WriteLocation(dataDir string) error {
	p, err := LocationFile()
	if err != nil {
		return err
	}
	if dataDir == "" {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	b, err := toml.Marshal(locationFile{DataDir: abs})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
