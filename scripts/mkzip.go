//go:build ignore

// mkzip writes a zip archive of one directory, the directory itself being the
// single top-level entry. scripts/build-windows.sh uses it because the build
// runs both under Git Bash on Windows (no `zip`) and on macOS (cross builds).
//
//	go run scripts/mkzip.go <out.zip> <dir>
//
// Entries are stored with forward slashes, sorted by name, with the files'
// modification times; executables are not given Unix mode bits, which mean
// nothing to the Windows users of the archive.
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/mkzip.go <out.zip> <dir>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "mkzip:", err)
		os.Exit(1)
	}
}

func run(out, dir string) (err error) {
	dir = filepath.Clean(dir)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(out)
		}
	}()
	zw := zip.NewWriter(f)
	parent := filepath.Dir(dir)
	// WalkDir visits entries in lexical order, so the archive is stable.
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		rel, err := filepath.Rel(parent, p)
		if err != nil {
			return err
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: filepath.ToSlash(rel), Method: zip.Deflate, Modified: info.ModTime()})
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(w, src)
		return err
	})
	if err != nil {
		return err
	}
	return zw.Close()
}
