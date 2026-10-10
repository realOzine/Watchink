package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestExtractTarXz: the Linux ffmpeg build ships as .tar.xz.
func TestExtractTarXz(t *testing.T) {
	payload := filepath.Join("testdata", "xz", "pkg.tar.xz")
	dir := t.TempDir()
	out, err := extractFiles(payload, ArchiveTarXz, map[string]string{"pkg-1.0/bin/tool": "tool"}, dir)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	const want = "#!/bin/sh\necho tool 1.0\n"
	sum := sha256.Sum256([]byte(want))
	if len(out) != 1 || out["tool"].size != int64(len(want)) || out["tool"].sha256 != hex.EncodeToString(sum[:]) {
		t.Errorf("extracted = %+v", out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "tool"))
	if err != nil || string(data) != want {
		t.Errorf("tool = %q, %v", data, err)
	}
	// Windows has no executable bit; the mode it reports is unrelated.
	if st, err := os.Stat(filepath.Join(dir, "tool")); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o755) {
		t.Errorf("tool mode = %v, %v", st, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("unlisted entries were extracted: %v", entries)
	}

	// Two files from one archive, and a file that is not there.
	out, err = extractFiles(payload, ArchiveTarXz, map[string]string{"pkg-1.0/bin/tool": "a", "pkg-1.0/doc/big.txt": "b"}, t.TempDir())
	if err != nil || len(out) != 2 || out["b"].size < 1000 {
		t.Errorf("two files: %+v, %v", out, err)
	}
	if _, err := extractFiles(payload, ArchiveTarXz, map[string]string{"pkg-1.0/bin/missing": "x"}, t.TempDir()); err == nil {
		t.Error("missing entry was accepted")
	}
	// A directory entry never satisfies a file mapping.
	if _, err := extractFiles(payload, ArchiveTarXz, map[string]string{"pkg-1.0/bin": "x"}, t.TempDir()); err == nil {
		t.Error("directory entry was extracted as a file")
	}
}
