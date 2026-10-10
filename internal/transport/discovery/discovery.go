// Package discovery is how an engine and its clients find each other inside
// one data directory, without either importing the other:
//
//   - <data>/run/engine.lock is held by the engine for as long as it lives;
//     whether an engine exists is decided by this lock alone.
//   - <data>/run/engine.json tells a client where the engine listens and
//     which token to use.
//
// The engine calls AcquireLock and Write; clients call IsRunning and Read.
package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// Run modes of an engine, recorded in Info.Mode.
const (
	ModeBackground = "background"
	ModeForeground = "foreground"
)

// ErrAlreadyRunning is returned by AcquireLock (and so by engine.Run) when
// another process holds the engine
// lock of the data directory. A spawned `engine run` that loses the start
// race (step 4) exits quietly on it.
var ErrAlreadyRunning = errors.New("engine: another engine already owns this data directory")

// Info is the content of <data>/run/engine.json. It only tells a
// client where the engine listens and which token to use; whether an engine
// is alive is decided by the lock alone.
type Info struct {
	PID       int    `json:"pid"`
	BaseURL   string `json:"base_url"`
	Token     string `json:"token"`
	Version   string `json:"version"`
	BuildID   string `json:"build_id"`
	API       int    `json:"api"`
	Mode      string `json:"mode"`
	StartedAt string `json:"started_at"`
}

// RunDir returns <data>/run.
func RunDir(dataDir string) string { return filepath.Join(dataDir, "run") }

// LockPath returns <data>/run/engine.lock.
func LockPath(dataDir string) string { return filepath.Join(RunDir(dataDir), "engine.lock") }

// Path returns <data>/run/engine.json.
func Path(dataDir string) string { return filepath.Join(RunDir(dataDir), "engine.json") }

// LogPath returns <data>/logs/engine.log.
func LogPath(dataDir string) string { return filepath.Join(dataDir, "logs", "engine.log") }

// Read reads engine.json. A missing file yields an error satisfying
// errors.Is(err, os.ErrNotExist). The file may be stale: callers must still
// reach the engine (or check IsRunning) before trusting it.
func Read(dataDir string) (*Info, error) {
	b, err := os.ReadFile(Path(dataDir))
	if err != nil {
		return nil, err
	}
	var d Info
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("engine: parse %s: %w", Path(dataDir), err)
	}
	if d.BaseURL == "" || d.Token == "" {
		return nil, fmt.Errorf("engine: %s is incomplete", Path(dataDir))
	}
	return &d, nil
}

// Write writes engine.json atomically with mode 0600: the
// file holds the engine token. Windows has no permission bits; there the file
// is private because the data directory lives in the user's profile, whose
// ACL only admits that user.
func Write(dataDir string, d Info) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := RunDir(dataDir)
	tmp, err := os.CreateTemp(dir, "engine.json.*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(b); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := replaceFile(name, Path(dataDir)); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// IsRunning reports whether an engine currently holds the lock of dataDir.
// It probes with a try-lock that is released at once, so it never keeps an
// engine from starting for longer than an instant (see AcquireLock).
func IsRunning(dataDir string) bool {
	path := LockPath(dataDir)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	fl := flock.New(path)
	ok, err := fl.TryLock()
	if err != nil {
		// Unknown state (e.g. permissions): assume nothing we can talk to.
		_ = fl.Close()
		return false
	}
	if ok {
		_ = fl.Unlock()
		return false
	}
	_ = fl.Close()
	return true
}

// Lock tuning: a client probing with IsRunning holds the lock for a few
// microseconds, so a starting engine retries briefly before concluding that
// a real engine owns the directory.
const (
	lockAttempts   = 8
	lockRetryDelay = 15 * time.Millisecond
)

// AcquireLock takes the exclusive engine lock, creating <data>/run.
func AcquireLock(dataDir string) (*flock.Flock, error) {
	if err := os.MkdirAll(RunDir(dataDir), 0o700); err != nil {
		return nil, fmt.Errorf("engine: create run dir: %w", err)
	}
	fl := flock.New(LockPath(dataDir))
	for i := 0; ; i++ {
		ok, err := fl.TryLock()
		if err != nil {
			_ = fl.Close()
			return nil, fmt.Errorf("engine: lock %s: %w", LockPath(dataDir), err)
		}
		if ok {
			return fl, nil
		}
		if i+1 >= lockAttempts {
			_ = fl.Close()
			return nil, ErrAlreadyRunning
		}
		time.Sleep(lockRetryDelay)
	}
}
