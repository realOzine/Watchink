package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"watchink/internal/app"
)

// pattern returns n deterministic, position-dependent bytes.
func pattern(n int) []byte {
	b := make([]byte, n)
	var x uint32 = 2463534242
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSampleHash(t *testing.T) {
	const mib = 1 << 20
	sum := func(parts ...[]byte) string {
		h := sha256.New()
		for _, p := range parts {
			h.Write(p)
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	sizes := []int{0, 1, 5 * mib, 12*mib - 1, 12 * mib, 12*mib + 1, 20*mib + 3}
	for _, size := range sizes {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := pattern(size)
			path := writeFile(t, "v.bin", data)
			want := sum(data)
			if size >= 12*mib {
				mid := (size - 4*mib) / 2
				want = sum(data[:4*mib], data[mid:mid+4*mib], data[size-4*mib:])
			}
			key, n, err := SampleHash(path)
			if err != nil {
				t.Fatal(err)
			}
			if n != int64(size) || key != fmt.Sprintf("file:%d:%s", size, want) {
				t.Errorf("key = %s (size %d), want file:%d:%s", key, n, size, want)
			}
		})
	}

	// A change outside the sampled windows keeps the key (by design);
	// a change inside any window, or a different size, changes it.
	data := pattern(20 * mib)
	base, _, _ := SampleHash(writeFile(t, "a", data))
	flip := func(off int) string {
		d := bytes.Clone(data)
		d[off] ^= 0xFF
		k, _, err := SampleHash(writeFile(t, "b", d))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	if flip(5*mib) != base {
		t.Error("byte between head and middle windows changed the key")
	}
	for _, off := range []int{0, 4*mib - 1, 8 * mib, 12*mib - 1, 16 * mib, 20*mib - 1} {
		if flip(off) == base {
			t.Errorf("byte at %d inside a sampled window did not change the key", off)
		}
	}

	if _, _, err := SampleHash(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v", err)
	}
	if _, _, err := SampleHash(t.TempDir()); err == nil {
		t.Error("directory accepted")
	}
}

func TestCopyWithSHA256(t *testing.T) {
	data := pattern(3<<20 + 17)
	src := writeFile(t, "src.mp4", data)
	dstDir := t.TempDir()
	dst := filepath.Join(dstDir, "source.mp4")

	sha, n, err := CopyWithSHA256(context.Background(), src, dst)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	if sha != hex.EncodeToString(want[:]) || n != int64(len(data)) {
		t.Errorf("sha = %s, n = %d", sha, n)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, data) {
		t.Error("copied content differs")
	}
	if entries, _ := os.ReadDir(dstDir); len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}

	// Overwrites an existing destination atomically.
	src2 := writeFile(t, "src2", []byte("hello"))
	if sha, n, err = CopyWithSHA256(context.Background(), src2, dst); err != nil || n != 5 {
		t.Fatalf("second copy: %v, n=%d", err, n)
	}
	if sha != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("sha = %s", sha)
	}

	// Cancelled: nothing is written and no temp file remains.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir2 := t.TempDir()
	if _, _, err := CopyWithSHA256(ctx, src, filepath.Join(dir2, "x.mp4")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if entries, _ := os.ReadDir(dir2); len(entries) != 0 {
		t.Errorf("files left after cancel: %v", entries)
	}

	if _, _, err := CopyWithSHA256(context.Background(), filepath.Join(dir2, "missing"), dst); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing source: err = %v", err)
	}
}

func TestDiskFull(t *testing.T) {
	enospc := &os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}
	tests := []struct {
		name string
		err  error
		out  string
		want bool
	}{
		{"ENOSPC", enospc, "", true},
		{"wrapped ENOSPC", fmt.Errorf("copy: %w", enospc), "", true},
		{"EDQUOT", syscall.EDQUOT, "", true},
		{"ffmpeg stderr", nil, "av_interleaved_write_frame(): No space left on device\n", true},
		{"python stderr", nil, "OSError: [Errno 28] No space left on device", true},
		{"error text", errors.New("write x: no space left on device"), "", true},
		{"already DISK_FULL", app.E(app.CodeDiskFull, "x"), "", true},
		{"other error", os.ErrPermission, "Permission denied", false},
		{"nothing", nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDiskFull(tt.err, tt.out); got != tt.want {
				t.Errorf("IsDiskFull = %v, want %v", got, tt.want)
			}
		})
	}

	wrapped := WrapDiskFull(enospc, "")
	if !app.IsCode(wrapped, app.CodeDiskFull) || !errors.Is(wrapped, syscall.ENOSPC) {
		t.Errorf("WrapDiskFull = %v", wrapped)
	}
	if WrapDiskFull(os.ErrPermission, "") != os.ErrPermission {
		t.Error("unrelated error was changed")
	}
	if WrapDiskFull(nil, "No space left on device") != nil {
		t.Error("nil error must stay nil")
	}
}
