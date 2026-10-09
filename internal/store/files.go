package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"watchink/internal/app"
)

// Top-level directories of the data dir (§6.2), as relative paths.
const (
	DirLibrary = "library"
	DirExports = "exports"
	DirUploads = "uploads"
	DirTools   = "tools"
	DirModels  = "models"
	DirRun     = "run"
	DirLogs    = "logs"
)

// Suffixes of in-progress files; SweepOrphans removes leftovers.
const (
	TmpSuffix  = ".tmp"
	PartSuffix = ".part"
)

// FileInfo describes a committed file.
type FileInfo struct {
	SHA256 string // lowercase hex
	Size   int64
}

// Abs converts a data-dir-relative path (forward slashes, as stored in the
// database) to an absolute path. The result never leaves the data dir: ".."
// components are resolved against its root.
func (s *Store) Abs(rel string) string {
	clean := path.Clean("/" + filepath.ToSlash(rel))
	return filepath.Join(s.dataDir, filepath.FromSlash(clean))
}

// Rel converts an absolute path inside the data dir to the relative,
// forward-slash form stored in the database. Paths outside the data dir are
// rejected.
func (s *Store) Rel(abs string) (string, error) {
	rel, err := filepath.Rel(s.dataDir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", app.E(app.CodeInternal, "路径不在数据目录内：%s", abs)
	}
	return filepath.ToSlash(rel), nil
}

// VideoDir returns the relative directory of a video: library/<video_id>.
// Combine with path.Join for the files of §6.2 and with Abs for tools.
func (s *Store) VideoDir(videoID string) string { return path.Join(DirLibrary, videoID) }

// ExportDir returns the relative export directory of a video.
func (s *Store) ExportDir(videoID string) string { return path.Join(DirExports, videoID) }

// UploadPath returns the relative staging path of an upload: uploads/<id>.part.
func (s *Store) UploadPath(uploadID string) string {
	return path.Join(DirUploads, uploadID+PartSuffix)
}

// TempPath returns a fresh absolute temp path next to the final location of
// rel ("<abs>.<random>.tmp") and creates the parent directory. Write the
// file there, then call CommitFile. Staying in the same directory keeps the
// final rename atomic.
func (s *Store) TempPath(rel string) (string, error) {
	abs := s.Abs(rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fileErr("无法创建目录", err)
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", internalErr("无法生成临时文件名", err)
	}
	return abs + "." + hex.EncodeToString(b[:]) + TmpSuffix, nil
}

// WriteFileAtomic writes data to rel through a temp file (write, fsync,
// rename — §6.2) and returns its hash and size. Register the artifact row
// afterwards.
func (s *Store) WriteFileAtomic(rel string, data []byte) (FileInfo, error) {
	tmp, err := s.TempPath(rel)
	if err != nil {
		return FileInfo{}, err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return FileInfo{}, fileErr("写入文件失败", err)
	}
	return s.CommitFile(tmp, rel)
}

// CommitFile moves a finished temp file (any absolute path on the same
// volume, normally from TempPath) to rel: it fsyncs the file, hashes it,
// renames it over the destination and fsyncs the directory. On failure the
// temp file is removed.
func (s *Store) CommitFile(tmpAbs, rel string) (_ FileInfo, err error) {
	defer func() {
		if err != nil {
			os.Remove(tmpAbs)
		}
	}()
	f, err := os.Open(tmpAbs)
	if err != nil {
		return FileInfo{}, fileErr("无法打开临时文件", err)
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return FileInfo{}, fileErr("写入文件失败", err)
	}
	dst := s.Abs(rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return FileInfo{}, fileErr("无法创建目录", err)
	}
	if err := os.Rename(tmpAbs, dst); err != nil {
		return FileInfo{}, fileErr("无法写入目标文件", err)
	}
	syncDir(filepath.Dir(dst))
	return FileInfo{SHA256: hex.EncodeToString(h.Sum(nil)), Size: size}, nil
}

// syncDir makes a rename durable. Best effort: some file systems do not
// support fsync on directories.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// SHA256File hashes the file at an absolute path.
func SHA256File(abs string) (FileInfo, error) {
	f, err := os.Open(abs)
	if err != nil {
		return FileInfo{}, fileErr("无法打开文件", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return FileInfo{}, fileErr("读取文件失败", err)
	}
	return FileInfo{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// RemoveFile deletes the file at rel; a missing file is not an error.
func (s *Store) RemoveFile(rel string) error {
	if err := os.Remove(s.Abs(rel)); err != nil && !os.IsNotExist(err) {
		return fileErr("删除文件失败", err)
	}
	return nil
}

// VerifyArtifact reports whether a may be reused (§7.4): the row is not
// purged, and — for artifacts with a file — the file exists with the
// registered size and sha256. A nil artifact is not reusable. Hashing reads
// the whole file, so this costs time on multi-gigabyte sources; see
// ArtifactPresent for the cheap check.
func (s *Store) VerifyArtifact(a *Artifact) bool {
	if !s.ArtifactPresent(a) {
		return false
	}
	if a.Path == "" {
		return true
	}
	fi, err := SHA256File(s.Abs(a.Path))
	return err == nil && fi.SHA256 == a.SHA256
}

// ArtifactPresent is the cheap variant of VerifyArtifact: the row is not
// purged and the file (if any) exists with the registered size; the content
// is not hashed.
func (s *Store) ArtifactPresent(a *Artifact) bool {
	if a == nil || a.Purged {
		return false
	}
	if a.Path == "" {
		return true
	}
	st, err := os.Stat(s.Abs(a.Path))
	return err == nil && st.Mode().IsRegular() && st.Size() == a.SizeBytes
}

// PurgeArtifact deletes the file of the artifact and marks the row purged,
// so the stage reruns when the artifact is needed again (§7.4).
func (s *Store) PurgeArtifact(ctx context.Context, id string) error {
	a, err := s.GetArtifact(ctx, id)
	if err != nil {
		return err
	}
	// Mark first: a crash in between leaves a purged row with a stray file,
	// which is harmless, rather than a live row without its file.
	if err := s.MarkArtifactPurged(ctx, id); err != nil {
		return err
	}
	if a.Path == "" {
		return nil
	}
	return s.RemoveFile(a.Path)
}

// SweepReport says what a sweep removed (relative paths).
type SweepReport struct {
	TempFiles      []string // *.tmp / *.part leftovers
	VideoDirs      []string // library/<id> directories without a videos row
	ExpiredUploads []string // upload IDs
}

// SweepExpiredUploads deletes the staged file and the row of every upload
// that expired without being imported (§6.2). Safe to call at any time.
func (s *Store) SweepExpiredUploads(ctx context.Context, now time.Time) ([]string, error) {
	expired, err := s.ListExpiredUploads(ctx, now)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, u := range expired {
		if err := s.RemoveFile(u.Path); err != nil {
			return ids, err
		}
		if err := s.DeleteUpload(ctx, u.ID); err != nil {
			return ids, err
		}
		ids = append(ids, u.ID)
	}
	return ids, nil
}

// SweepOrphans cleans the data dir after a restart (§6.2, §7.6 step 3). Call
// it at engine start, after Recover and before the scheduler or the API
// accept work — it assumes nothing is being written:
//
//   - *.tmp and *.part files under library/ and exports/ are removed;
//   - library/<id> directories without a videos row are removed;
//   - under uploads/, *.tmp files and *.part files without an uploads row
//     are removed, and expired, never-imported uploads are deleted.
//
// tools/ and models/ are left to the package that manages them.
func (s *Store) SweepOrphans(ctx context.Context) (*SweepReport, error) {
	rep := &SweepReport{}

	entries, err := os.ReadDir(s.Abs(DirLibrary))
	if err != nil && !os.IsNotExist(err) {
		return rep, fileErr("无法读取媒体库目录", err)
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), app.PrefixVideo) {
			continue
		}
		if _, err := s.GetVideo(ctx, e.Name()); app.IsCode(err, app.CodeNotFound) {
			rel := path.Join(DirLibrary, e.Name())
			if err := os.RemoveAll(s.Abs(rel)); err != nil {
				return rep, fileErr("无法清理孤立目录", err)
			}
			rep.VideoDirs = append(rep.VideoDirs, rel)
		} else if err != nil {
			return rep, err
		}
	}

	for _, root := range []string{DirLibrary, DirExports} {
		err := filepath.WalkDir(s.Abs(root), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if d.Type().IsRegular() && (strings.HasSuffix(p, TmpSuffix) || strings.HasSuffix(p, PartSuffix)) {
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					return err
				}
				if rel, err := s.Rel(p); err == nil {
					rep.TempFiles = append(rep.TempFiles, rel)
				}
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return rep, err
			}
			return rep, fileErr("清理临时文件失败", err)
		}
	}

	if rep.ExpiredUploads, err = s.SweepExpiredUploads(ctx, time.Now()); err != nil {
		return rep, err
	}
	uploads, err := s.ListUploads(ctx)
	if err != nil {
		return rep, err
	}
	known := make(map[string]bool, len(uploads))
	for _, u := range uploads {
		known[path.Base(u.Path)] = true
	}
	entries, err = os.ReadDir(s.Abs(DirUploads))
	if err != nil && !os.IsNotExist(err) {
		return rep, fileErr("无法读取上传目录", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		if strings.HasSuffix(name, TmpSuffix) || (strings.HasSuffix(name, PartSuffix) && !known[name]) {
			rel := path.Join(DirUploads, name)
			if err := s.RemoveFile(rel); err != nil {
				return rep, err
			}
			rep.TempFiles = append(rep.TempFiles, rel)
		}
	}
	return rep, nil
}

// fileErr maps a file system error to an *app.Error; a full disk becomes
// DISK_FULL so the job error tells the user what to do.
func fileErr(msg string, err error) *app.Error {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return app.E(app.CodeDiskFull, "磁盘空间不足").WithHint("请清理磁盘空间后重试").WithCause(err)
	}
	return app.E(app.CodeInternal, "%s：%v", msg, err).WithCause(err)
}
