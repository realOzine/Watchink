package tools

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// extracted describes one file written by extractFiles.
type extracted struct {
	sha256 string
	size   int64
}

// maxExtractedSize bounds a single extracted file; the largest real one
// (the Linux ffmpeg) is under 200 MB. It only guards against a broken archive.
const maxExtractedSize = 2 << 30

// extractFiles copies the entries listed in files (archive path → installed
// name) from an already verified payload into dir, with mode 0755. Entries not
// listed are ignored, and installed names are plain file names, so an archive
// cannot write outside dir.
func extractFiles(payload, kind string, files map[string]string, dir string) (map[string]extracted, error) {
	out := make(map[string]extracted, len(files))
	put := func(dst string, r io.Reader) error {
		sum, n, err := writeNewFile(filepath.Join(dir, dst), io.LimitReader(r, maxExtractedSize), 0o755)
		if err != nil {
			return err
		}
		out[dst] = extracted{sha256: sum, size: n}
		return nil
	}
	want := make(map[string]string, len(files))
	for src, dst := range files {
		want[cleanArchivePath(src)] = dst
	}

	switch kind {
	case ArchiveNone:
		f, err := os.Open(payload)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		for _, dst := range files {
			if err := put(dst, f); err != nil {
				return nil, err
			}
		}
	case ArchiveZip:
		zr, err := zip.OpenReader(payload)
		if err != nil {
			return nil, fmt.Errorf("open zip: %w", err)
		}
		defer zr.Close()
		for _, zf := range zr.File {
			dst, ok := want[cleanArchivePath(zf.Name)]
			if !ok || zf.FileInfo().IsDir() {
				continue
			}
			if _, dup := out[dst]; dup {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				return nil, fmt.Errorf("zip entry %s: %w", zf.Name, err)
			}
			err = put(dst, rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
		}
	case ArchiveTarGz:
		f, err := os.Open(payload)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("open tar.gz: %w", err)
		}
		if err := untar(gz, kind, want, out, put); err != nil {
			return nil, err
		}
	case ArchiveTarXz:
		f, err := os.Open(payload)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if err := untar(newXZReader(f), kind, want, out, put); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown archive kind %q", kind)
	}

	for src, dst := range files {
		if _, ok := out[dst]; !ok {
			return nil, fmt.Errorf("archive has no entry %q", src)
		}
	}
	return out, nil
}

// untar copies the wanted regular files of a tar stream through put. It stops
// reading once every wanted file was seen, so the rest of a large archive is
// not decompressed for nothing.
func untar(r io.Reader, kind string, want map[string]string, out map[string]extracted, put func(dst string, r io.Reader) error) error {
	tr := tar.NewReader(r)
	for len(out) < len(want) {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", kind, err)
		}
		dst, ok := want[cleanArchivePath(hdr.Name)]
		if !ok || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if _, dup := out[dst]; dup {
			continue
		}
		if err := put(dst, tr); err != nil {
			return err
		}
	}
	return nil
}

func cleanArchivePath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(p, `\`, "/")), "/")
}

// writeNewFile creates path (which must not exist), copies r into it and
// returns the sha256 and size of what was written.
func writeNewFile(path string, r io.Reader, mode os.FileMode) (string, int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		// OpenFile's mode is filtered by the umask; the executable bit must
		// not depend on it (§11.3 step 4).
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// hashFile returns the sha256 and size of a file.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
