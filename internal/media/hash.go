package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"watchink/internal/app"
)

const (
	// sampleBytes is the size of each of the three sampled windows (§6.6).
	sampleBytes = 4 << 20
	// sampleWholeBelow: smaller files are hashed completely (§6.6).
	sampleWholeBelow = 3 * sampleBytes
	copyBufBytes     = 1 << 20
)

// SampleHash computes the dedupe key of a local file or upload (§6.6, D-13):
//
//	file:<size>:<sha256(head 4 MiB ‖ middle 4 MiB ‖ tail 4 MiB)>
//
// The middle window starts at offset (size − 4 MiB) / 2. Files smaller than
// 12 MiB are hashed in full. Reading at most 12 MiB keeps an import request
// in the millisecond range regardless of the file size.
func SampleHash(path string) (key string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, app.E(app.CodeInvalidArgument, "不是普通文件：%s", filepath.Base(path))
	}
	size = st.Size()
	h := sha256.New()
	if size < sampleWholeBelow {
		if _, err := io.Copy(h, f); err != nil {
			return "", 0, err
		}
	} else {
		for _, off := range []int64{0, (size - sampleBytes) / 2, size - sampleBytes} {
			if _, err := io.Copy(h, io.NewSectionReader(f, off, sampleBytes)); err != nil {
				return "", 0, err
			}
		}
	}
	return fmt.Sprintf("file:%d:%s", size, hex.EncodeToString(h.Sum(nil))), size, nil
}

// CopyWithSHA256 copies src to dst and returns the full sha256 of the content
// computed on the way (D-13). The data goes to a temporary file in dst's
// directory, is fsynced, and only then renamed over dst, so a crash never
// leaves a partial dst. A full disk is reported as DISK_FULL.
func CopyWithSHA256(ctx context.Context, src, dst string) (sha string, n int64, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return "", 0, WrapDiskFull(err, "")
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
			err = WrapDiskFull(err, "")
		}
	}()

	h := sha256.New()
	buf := make([]byte, copyBufBytes)
	for {
		if err = ctx.Err(); err != nil {
			return "", 0, err
		}
		nr, rerr := in.Read(buf)
		if nr > 0 {
			h.Write(buf[:nr])
			if _, err = tmp.Write(buf[:nr]); err != nil {
				return "", 0, err
			}
			n += int64(nr)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			err = rerr
			return "", 0, err
		}
	}
	if err = tmp.Chmod(0o644); err != nil {
		return "", 0, err
	}
	if err = tmp.Sync(); err != nil {
		return "", 0, err
	}
	if err = tmp.Close(); err != nil {
		return "", 0, err
	}
	if err = os.Rename(tmpName, dst); err != nil {
		return "", 0, err
	}
	syncDir(dir)
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// syncDir makes a rename durable; failures are not actionable and ignored.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// IsDiskFull reports whether err, or the output of an external tool, says the
// disk is full (ENOSPC / EDQUOT, "No space left on device").
func IsDiskFull(err error, toolOutput string) bool {
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) || app.IsCode(err, app.CodeDiskFull) {
			return true
		}
		if containsDiskFull(err.Error()) {
			return true
		}
	}
	return containsDiskFull(toolOutput)
}

func containsDiskFull(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "no space left on device") ||
		strings.Contains(s, "disk quota exceeded") ||
		strings.Contains(s, "errno 28") ||
		strings.Contains(s, "磁盘空间不足")
}

// WrapDiskFull converts err into a DISK_FULL *app.Error when err or the tool
// output indicates a full disk; any other error is returned unchanged.
func WrapDiskFull(err error, toolOutput string) error {
	if err == nil || app.IsCode(err, app.CodeDiskFull) || !IsDiskFull(err, toolOutput) {
		return err
	}
	return app.E(app.CodeDiskFull, "磁盘空间不足").
		WithHint("请清理磁盘空间后重试。").WithCause(err)
}
