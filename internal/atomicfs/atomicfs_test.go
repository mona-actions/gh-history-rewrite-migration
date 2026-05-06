package atomicfs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWriteFileAtomicHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.tar.gz")

	err := WriteFileAtomic(path, func(w io.Writer) error {
		_, err := w.Write([]byte("complete"))
		return err
	})
	if err != nil {
		t.Fatalf("WriteFileAtomic returned error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "complete" {
		t.Fatalf("target content = %q, want complete", got)
	}
	assertNoPartials(t, dir)
}

func TestWriteFileAtomicPathHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.tar.gz")

	err := WriteFileAtomicPath(path, func(partialPath string) error {
		return os.WriteFile(partialPath, []byte("complete"), 0o644)
	})
	if err != nil {
		t.Fatalf("WriteFileAtomicPath returned error: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "complete" {
		t.Fatalf("target content = %q, want complete", got)
	}
	assertNoPartials(t, dir)
}

func TestWriteFileAtomicCallbackErrorRemovesPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.tar.gz")
	wantErr := errors.New("write failed")

	err := WriteFileAtomic(path, func(w io.Writer) error {
		_, writeErr := w.Write([]byte("partial"))
		if writeErr != nil {
			return writeErr
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WriteFileAtomic error = %v, want %v", err, wantErr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("target stat error = %v, want not exist", err)
	}
	assertNoPartials(t, dir)
}

func TestWriteFileAtomicCallbackPanicRemovesPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.tar.gz")

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("WriteFileAtomic did not repanic")
			}
		}()
		_ = WriteFileAtomic(path, func(w io.Writer) error {
			_, err := w.Write([]byte("partial"))
			if err != nil {
				return err
			}
			panic("boom")
		})
	}()

	assertNoPartials(t, dir)
}

func TestMarkDirCompleteAndIsDirComplete(t *testing.T) {
	dir := t.TempDir()
	if IsDirComplete(dir) {
		t.Fatal("IsDirComplete = true before sentinel exists")
	}
	if err := MarkDirComplete(dir); err != nil {
		t.Fatalf("MarkDirComplete returned error: %v", err)
	}
	if !IsDirComplete(dir) {
		t.Fatal("IsDirComplete = false after sentinel exists")
	}
}

func TestIsFileComplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.tar.gz")
	if IsFileComplete(path) {
		t.Fatal("IsFileComplete = true for missing target")
	}
	if err := os.WriteFile(path, []byte("complete"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if !IsFileComplete(path) {
		t.Fatal("IsFileComplete = false when only target exists")
	}
	if err := os.WriteFile(path+".partial", []byte("partial"), 0o644); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if IsFileComplete(path) {
		t.Fatal("IsFileComplete = true when exact partial sibling exists")
	}
	if err := os.Remove(path + ".partial"); err != nil {
		t.Fatalf("remove exact partial: %v", err)
	}
	if err := os.WriteFile(path+".123.partial", []byte("partial"), 0o644); err != nil {
		t.Fatalf("write unique partial: %v", err)
	}
	if IsFileComplete(path) {
		t.Fatal("IsFileComplete = true when unique partial sibling exists")
	}
}

func TestRemoveIfPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.tar.gz")
	partial := path + ".partial"

	if err := os.WriteFile(partial, []byte("partial"), 0o644); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if err := RemoveIfPartial(path); err != nil {
		t.Fatalf("RemoveIfPartial existing returned error: %v", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial stat error = %v, want not exist", err)
	}
	if err := RemoveIfPartial(path); err != nil {
		t.Fatalf("RemoveIfPartial absent returned error: %v", err)
	}
}

func TestSweepPartials(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.partial", "two.partial", "keep.txt", "also.keep"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if err := SweepPartials(dir); err != nil {
		t.Fatalf("SweepPartials returned error: %v", err)
	}
	for _, name := range []string{"one.partial", "two.partial"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s stat error = %v, want not exist", name, err)
		}
	}
	for _, name := range []string{"keep.txt", "also.keep"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s should remain: %v", name, err)
		}
	}
}

func TestValidateTarHeader(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.tar.gz")
	if err := os.WriteFile(valid, validTarGzip(t), 0o644); err != nil {
		t.Fatalf("write valid tar.gz: %v", err)
	}
	if err := ValidateTarHeader(valid); err != nil {
		t.Fatalf("ValidateTarHeader(valid) returned error: %v", err)
	}

	truncated := filepath.Join(dir, "truncated.tar.gz")
	if err := os.WriteFile(truncated, []byte{0x1f, 0x8b, 0x08}, 0o644); err != nil {
		t.Fatalf("write truncated: %v", err)
	}
	if err := ValidateTarHeader(truncated); err == nil {
		t.Fatal("ValidateTarHeader(truncated) returned nil, want error")
	}

	nonTar := filepath.Join(dir, "not.tar")
	if err := os.WriteFile(nonTar, []byte("not a tar archive"), 0o644); err != nil {
		t.Fatalf("write non-tar: %v", err)
	}
	if err := ValidateTarHeader(nonTar); err == nil {
		t.Fatal("ValidateTarHeader(non-tar) returned nil, want error")
	}
}

func TestWriteFileAtomicConcurrentWritersLastRenameWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.txt")
	contents := []string{"first", "second"}

	var wg sync.WaitGroup
	errs := make(chan error, len(contents))
	for _, content := range contents {
		content := content
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- WriteFileAtomic(path, func(w io.Writer) error {
				_, err := w.Write([]byte(content))
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("WriteFileAtomic concurrent error: %v", err)
		}
	}
	gotBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	got := string(gotBytes)
	if got != "first" && got != "second" {
		t.Fatalf("target content = %q, want one complete writer payload", got)
	}
	assertNoPartials(t, dir)
}

func validTarGzip(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	payload := []byte("hello")
	if err := tw.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0o644, Size: int64(len(payload))}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("write tar payload: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

func assertNoPartials(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".partial") {
			t.Fatalf("unexpected partial file left behind: %s", entry.Name())
		}
	}
}

func TestCopyFileCopiesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "nested", "dst.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o640); err != nil {
		t.Fatalf("write src: %v", err)
	}

	if err := CopyFile(src, dst); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("dst content = %q, want hello", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("dst mode = %v, want 0640", info.Mode().Perm())
	}
}

func TestCopyTreeCopiesRecursiveFilesAndModes(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "root.txt"), []byte("root"), 0o644); err != nil {
		t.Fatalf("write root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "exec.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write exec: %v", err)
	}

	if err := CopyTree(src, dst); err != nil {
		t.Fatalf("CopyTree: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "root.txt")); err != nil || string(got) != "root" {
		t.Fatalf("root copy = %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(dst, "sub", "exec.sh"))
	if err != nil {
		t.Fatalf("stat exec: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("exec mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestCopyTreeRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.Symlink("missing", filepath.Join(src, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := CopyTree(src, filepath.Join(dir, "dst")); err == nil {
		t.Fatal("CopyTree accepted symlink")
	}
}
