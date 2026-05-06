package atomicfs

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

var partialCounter atomic.Uint64

// WriteFileAtomic writes data to a unique *.partial file, fsyncs, then renames to path.
// Mode 0644 by default. Concurrent writers are last-rename-wins; each writer uses
// its own partial file so the final file is never interleaved or partially written.
// The writer callback is given an io.Writer; on error, the .partial file is removed.
func WriteFileAtomic(path string, write func(io.Writer) error) (err error) {
	return WriteFileAtomicPath(path, func(partialPath string) error {
		f, err := os.OpenFile(partialPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := write(f); err != nil {
			return err
		}
		return f.Sync()
	})
}

// WriteFileAtomicPath creates a unique *.partial path, lets write populate it,
// fsyncs the resulting regular file when possible, then renames it to path.
// The partial file is removed if write returns an error or panics.
func WriteFileAtomicPath(path string, write func(partialPath string) error) (err error) {
	partial := uniquePartialPath(path)
	cleanup := true
	defer func() {
		if r := recover(); r != nil {
			_ = os.Remove(partial)
			panic(r)
		}
		if cleanup {
			_ = os.Remove(partial)
		}
	}()

	if err := write(partial); err != nil {
		return err
	}
	if info, statErr := os.Stat(partial); statErr != nil {
		return statErr
	} else if !info.IsDir() && info.Mode().IsRegular() {
		f, err := os.OpenFile(partial, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	if err := os.Rename(partial, path); err != nil {
		return err
	}

	cleanup = false
	return nil
}

// CopyFile copies a regular file from src to dst, preserving source file mode
// bits and fsyncing dst before close. Symlinks are rejected.
func CopyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("copy file %s: symlinks are not supported", src)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("copy file %s: not a regular file", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// CopyTree recursively copies src into dst. Directory creation uses source
// directory permissions, file permissions are preserved, and symlinks are
// rejected because archive staging only needs regular files and directories.
func CopyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("copy tree %s: symlinks are not supported", src)
	}
	if !info.IsDir() {
		return CopyFile(src, dst)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childSrc := filepath.Join(src, entry.Name())
		childDst := filepath.Join(dst, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("copy tree %s: symlinks are not supported", childSrc)
		}
		if entry.IsDir() {
			if err := CopyTree(childSrc, childDst); err != nil {
				return err
			}
			continue
		}
		if err := CopyFile(childSrc, childDst); err != nil {
			return err
		}
	}
	return nil
}

// MarkDirComplete writes <dir>/.complete sentinel file. dir must exist.
func MarkDirComplete(dir string) error {
	return os.WriteFile(filepath.Join(dir, ".complete"), nil, 0o644)
}

// IsDirComplete reports whether <dir>/.complete exists.
func IsDirComplete(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".complete"))
	return err == nil
}

// IsFileComplete reports whether path exists and has no .partial sibling
// (a .partial sibling means a previous write was interrupted).
func IsFileComplete(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}

	partials, err := matchingPartials(path)
	if err != nil {
		return false
	}
	return len(partials) == 0
}

// RemoveIfPartial removes <path>.partial if it exists. Best-effort; returns
// nil if absent. Used by Lock acquisition to sweep orphans.
func RemoveIfPartial(path string) error {
	err := os.Remove(path + ".partial")
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

// SweepPartials walks dir non-recursively, removing any *.partial files.
// Used at session start to clean up after kill -9.
func SweepPartials(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".partial") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// ValidateTarHeader opens path, reads first tar header, returns nil if it
// parses (handles .tar.gz transparently). Cheap; catches truncated downloads.
func ValidateTarHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader = f
	var gz *gzip.Reader
	if strings.HasSuffix(path, ".gz") || strings.HasSuffix(path, ".tgz") {
		gz, err = gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}

	_, err = tar.NewReader(r).Next()
	if err != nil {
		return fmt.Errorf("validate tar header %q: %w", path, err)
	}
	return nil
}

func uniquePartialPath(path string) string {
	return fmt.Sprintf("%s.%d-%d-%d.partial", path, os.Getpid(), time.Now().UnixNano(), partialCounter.Add(1))
}

func matchingPartials(path string) ([]string, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	partials := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".partial") {
			continue
		}
		if name == base+".partial" || strings.HasPrefix(name, base+".") {
			partials = append(partials, filepath.Join(dir, name))
		}
	}
	return partials, nil
}
