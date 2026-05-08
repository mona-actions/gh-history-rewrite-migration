package remap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mona-actions/gh-commit-remap/pkg/archive"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/atomicfs"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

const (
	oldSHA1 = "1111111111111111111111111111111111111111"
	newSHA1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oldSHA2 = "2222222222222222222222222222222222222222"
	newSHA2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type remapFixture struct {
	root string
	wd   *workdir.WorkDir
	src  string
	in   Input
}

func newFixture(t *testing.T, files map[string]string, commitMap string) remapFixture {
	t.Helper()
	root := t.TempDir()
	wd, err := workdir.New(root)
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}
	src := filepath.Join(root, "metadata-src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	for name, content := range files {
		path := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := archive.ReTarDir(src, wd.RawMetadataArchive()); err != nil {
		t.Fatalf("retar raw metadata: %v", err)
	}
	if err := os.WriteFile(wd.CommitMap(), []byte(commitMap), 0o644); err != nil {
		t.Fatalf("write commit-map: %v", err)
	}
	return remapFixture{
		root: root,
		wd:   wd,
		src:  src,
		in: Input{
			WorkDir:              wd,
			RawMetadataArchive:   wd.RawMetadataArchive(),
			CommitMapPath:        wd.CommitMap(),
			MetadataExtractedDir: wd.MetadataExtractedDir(),
		},
	}
}

func newArchiveFixture(t *testing.T, rawArchive, commitMap string) remapFixture {
	t.Helper()
	root := t.TempDir()
	wd, err := workdir.New(root)
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}
	if err := atomicfs.CopyFile(rawArchive, wd.RawMetadataArchive()); err != nil {
		t.Fatalf("copy raw metadata: %v", err)
	}
	if err := os.WriteFile(wd.CommitMap(), []byte(commitMap), 0o644); err != nil {
		t.Fatalf("write commit-map: %v", err)
	}
	return remapFixture{
		root: root,
		wd:   wd,
		in: Input{
			WorkDir:              wd,
			RawMetadataArchive:   wd.RawMetadataArchive(),
			CommitMapPath:        wd.CommitMap(),
			MetadataExtractedDir: wd.MetadataExtractedDir(),
		},
	}
}

func runRemap(t *testing.T, fx remapFixture) Result {
	t.Helper()
	res, err := NewReal(nil).Run(context.Background(), fx.in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func extractFinal(t *testing.T, fx remapFixture) string {
	t.Helper()
	out := filepath.Join(fx.root, "final-extracted")
	if _, err := archive.UnTar(fx.wd.MetadataArchive(), out); err != nil {
		t.Fatalf("untar final: %v", err)
	}
	return out
}

func readExtracted(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read extracted %s: %v", name, err)
	}
	return string(data)
}

func commitMap(entries ...string) string {
	return strings.Join(entries, "\n") + "\n"
}

func TestValidate_Success(t *testing.T) {
	fx := newFixture(t, map[string]string{"issues_000001.json": `[{"sha":"` + oldSHA1 + `"}]`}, commitMap(oldSHA1+" "+newSHA1))
	if err := Validate(fx.in); err != nil {
		t.Fatalf("expected valid input, got %v", err)
	}
}

func TestValidate_NilWorkDir(t *testing.T) {
	fx := newFixture(t, map[string]string{"issues_000001.json": `[]`}, "")
	fx.in.WorkDir = nil
	if err := Validate(fx.in); err == nil {
		t.Fatal("expected error for nil WorkDir")
	}
}

func TestValidate_MissingRawMetadataArchive(t *testing.T) {
	fx := newFixture(t, map[string]string{"issues_000001.json": `[]`}, "")
	fx.in.RawMetadataArchive = filepath.Join(fx.root, "missing.tar.gz")
	if err := Validate(fx.in); err == nil {
		t.Fatal("expected error for missing raw metadata archive")
	}
}

func TestValidate_MissingCommitMap(t *testing.T) {
	fx := newFixture(t, map[string]string{"issues_000001.json": `[]`}, "")
	fx.in.CommitMapPath = filepath.Join(fx.root, "missing-commit-map")
	if err := Validate(fx.in); err == nil {
		t.Fatal("expected error for missing commit-map")
	}
}

func TestEmptyCommitMapNoOpCopiesRawArchive(t *testing.T) {
	fx := newFixture(t, map[string]string{"users_000001.json": `[{"login":"octo"}]`}, "")
	res := runRemap(t, fx)

	if res.CommitsRemapped != 0 {
		t.Fatalf("expected 0 commits remapped, got %d", res.CommitsRemapped)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "commit-map is empty") {
		t.Fatalf("expected empty commit-map warning, got %#v", res.Warnings)
	}
	raw, err := os.ReadFile(fx.wd.RawMetadataArchive())
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	final, err := os.ReadFile(fx.wd.MetadataArchive())
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(raw) != string(final) {
		t.Fatal("expected final archive to be copied byte-for-byte from raw archive")
	}
}

func TestHappyPathRewritesSHAs(t *testing.T) {
	fx := newFixture(t, map[string]string{
		"issues_000001.json": `[{"sha":"` + oldSHA1 + `","nested":{"other":"` + oldSHA2 + `"}}]`,
	}, commitMap(oldSHA1+" "+newSHA1, oldSHA2+" "+newSHA2))
	res := runRemap(t, fx)

	if res.CommitsRemapped != 2 || res.FilesScanned != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	got := readExtracted(t, extractFinal(t, fx), "issues_000001.json")
	if !strings.Contains(got, newSHA1) || !strings.Contains(got, newSHA2) {
		t.Fatalf("expected rewritten SHAs in %s", got)
	}
	if strings.Contains(got, oldSHA1) || strings.Contains(got, oldSHA2) {
		t.Fatalf("old SHAs remained in %s", got)
	}
}

func TestLayoutWarningWhenNoFilesChanged(t *testing.T) {
	fx := newFixture(t, map[string]string{"issues_000001.json": `[{"sha":"3333333333333333333333333333333333333333"}]`}, commitMap(oldSHA1+" "+newSHA1))
	res := runRemap(t, fx)
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "0 files were rewritten") {
		t.Fatalf("expected layout warning, got %#v", res.Warnings)
	}
}

func TestLayoutErrorWhenNoSHABearingFilesMatched(t *testing.T) {
	fx := newFixture(t, map[string]string{"users_000001.json": `[{"login":"octo"}]`}, commitMap(oldSHA1+" "+newSHA1))
	_, err := NewReal(nil).Run(context.Background(), fx.in)
	if err == nil || !strings.Contains(err.Error(), "no metadata files matching SHA-bearing prefixes") {
		t.Fatalf("expected no-matched-files layout error, got %v", err)
	}
}

func TestNoLayoutWarningWhenFilesChanged(t *testing.T) {
	fx := newFixture(t, map[string]string{"issues_000001.json": `[{"sha":"` + oldSHA1 + `"}]`}, commitMap(oldSHA1+" "+newSHA1))
	res := runRemap(t, fx)
	for _, warning := range res.Warnings {
		if strings.Contains(warning, "0 files were rewritten") {
			t.Fatalf("did not expect layout warning, got %#v", res.Warnings)
		}
	}
}

func TestUnknownPrefixWarning(t *testing.T) {
	fx := newFixture(t, map[string]string{
		"issues_000001.json":              `[{"sha":"` + oldSHA1 + `"}]`,
		"discussion_comments_000001.json": `[{"sha":"` + oldSHA1 + `"}]`,
	}, commitMap(oldSHA1+" "+newSHA1))
	res := runRemap(t, fx)
	if !hasWarningContaining(res.Warnings, `unrecognized prefix file "discussion_comments"`) || !hasWarningContaining(res.Warnings, "unrecognized prefix") {
		t.Fatalf("expected unknown-prefix warning, got %#v", res.Warnings)
	}

	fx = newFixture(t, map[string]string{"issues_000001.json": `[{"sha":"` + oldSHA1 + `"}]`}, commitMap(oldSHA1+" "+newSHA1))
	res = runRemap(t, fx)
	if hasWarningContaining(res.Warnings, "unrecognized prefix file") {
		t.Fatalf("did not expect unknown-prefix warning, got %#v", res.Warnings)
	}
}

func TestFindUnknownPrefixes(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"users_000001.json",
		"weirdname.json",
		"discussion_comments_000001.json",
		"discussion_comments_000002.json",
		"pull_requests_000001.json",
		"future_reviews_abc.json",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`[]`), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got := findUnknownPrefixes(dir)
	if len(got) != 1 || got[0] != "discussion_comments" {
		t.Fatalf("expected only discussion_comments, got %#v", got)
	}
}

func TestMultiRepoGuard(t *testing.T) {
	fx := newFixture(t, map[string]string{
		"repo-one/issues_000001.json": `[{"sha":"` + oldSHA1 + `"}]`,
		"repo-two/issues_000001.json": `[{"sha":"` + oldSHA1 + `"}]`,
	}, commitMap(oldSHA1+" "+newSHA1))
	_, err := NewReal(nil).Run(context.Background(), fx.in)
	if err == nil || !strings.Contains(err.Error(), "multiple metadata directories") || !strings.Contains(err.Error(), "repo-one") || !strings.Contains(err.Error(), "repo-two") {
		t.Fatalf("expected multi-repo error naming discovered dirs, got %v", err)
	}
}

func TestIdempotencyShortCircuitsExistingArchive(t *testing.T) {
	fx := newFixture(t, map[string]string{"issues_000001.json": `[{"sha":"` + oldSHA1 + `"}]`}, commitMap(oldSHA1+" "+newSHA1))
	if err := os.WriteFile(fx.wd.MetadataArchive(), []byte("already-done"), 0o644); err != nil {
		t.Fatalf("write existing final: %v", err)
	}
	res := runRemap(t, fx)
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "already exists") {
		t.Fatalf("expected cached warning, got %#v", res.Warnings)
	}
	data, err := os.ReadFile(fx.wd.MetadataArchive())
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(data) != "already-done" {
		t.Fatalf("expected existing archive to be untouched, got %q", data)
	}
}

func TestNonSHAPrefixUsersIsNotRewritten(t *testing.T) {
	fx := newFixture(t, map[string]string{
		"issues_000001.json": `[{"sha":"3333333333333333333333333333333333333333"}]`,
		"users_000001.json":  `[{"bio":"` + oldSHA1 + `"}]`,
	}, commitMap(oldSHA1+" "+newSHA1))
	runRemap(t, fx)
	got := readExtracted(t, extractFinal(t, fx), "users_000001.json")
	if !strings.Contains(got, oldSHA1) || strings.Contains(got, newSHA1) {
		t.Fatalf("users prefix should not be rewritten, got %s", got)
	}
}

func TestSHABearingPrefixesSmoke(t *testing.T) {
	for _, prefix := range SHABearingPrefixes {
		t.Run(prefix, func(t *testing.T) {
			fx := newFixture(t, map[string]string{prefix + "_000001.json": `[{"some_field":"` + oldSHA1 + `"}]`}, commitMap(oldSHA1+" "+newSHA1))
			runRemap(t, fx)
			got := readExtracted(t, extractFinal(t, fx), prefix+"_000001.json")
			if !strings.Contains(got, newSHA1) || strings.Contains(got, oldSHA1) {
				t.Fatalf("expected %s to be rewritten, got %s", prefix, got)
			}
		})
	}
}

func TestAttachmentsBinaryFidelity(t *testing.T) {
	roundTrip := func(t *testing.T, rawArchive string) {
		t.Helper()
		sourceExtract := filepath.Join(t.TempDir(), "source")
		if _, err := archive.UnTar(rawArchive, sourceExtract); err != nil {
			t.Fatalf("untar source fixture: %v", err)
		}
		want := collectAttachmentFingerprints(t, sourceExtract)
		wantAttachmentJSON := readUniqueFileNamed(t, sourceExtract, "attachments_000001.json")

		fx := newArchiveFixture(t, rawArchive, commitMap(oldSHA1+" "+newSHA1))
		runRemap(t, fx)
		finalExtract := extractFinal(t, fx)

		assertFingerprintsEqual(t, want, collectAttachmentFingerprints(t, finalExtract))
		gotAttachmentJSON := readUniqueFileNamed(t, finalExtract, "attachments_000001.json")
		if !bytes.Equal(gotAttachmentJSON, wantAttachmentJSON) {
			t.Fatal("attachments_000001.json changed during remap")
		}
	}

	t.Run("synthetic binary edge cases", func(t *testing.T) {
		entries := []tarFixtureEntry{
			{name: "issues_000001.json", mode: 0o644, body: []byte(`[{"some_field":"` + oldSHA1 + `"}]`)},
			{name: "attachments_000001.json", mode: 0o644, body: []byte(`[{"path":"attachments/empty.bin"}]`)},
			{name: "attachments", mode: 0o755, dir: true},
			{name: "attachments/empty.bin", mode: 0o644},
			{name: "attachments/nuls.bin", mode: 0o644, body: []byte{0x00, 0x01, 0x02, 0x00, 0xff}},
		}
		if runtime.GOOS != "windows" {
			entries = append(entries, tarFixtureEntry{name: "attachments/executable.bin", mode: 0o755, body: []byte("#!/bin/sh\nexit 0\n")})
		}

		rawArchive := filepath.Join(t.TempDir(), "metadata.tar.gz")
		writeTarGzFixture(t, rawArchive, entries)
		roundTrip(t, rawArchive)
	})
}

func TestWarningsAreLogged(t *testing.T) {
	fx := newFixture(t, map[string]string{
		"issues_000001.json":              `[{"sha":"3333333333333333333333333333333333333333"}]`,
		"discussion_comments_000001.json": `[{"sha":"` + oldSHA1 + `"}]`,
	}, commitMap(oldSHA1+" "+newSHA1))
	logger := &recordingLogger{}
	res, err := NewReal(logger).Run(context.Background(), fx.in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected warnings")
	}
	for _, warning := range res.Warnings {
		if !hasWarningContaining(logger.warns, warning) {
			t.Fatalf("expected logger warning %q in %#v", warning, logger.warns)
		}
	}
}

type tarFixtureEntry struct {
	name string
	mode int64
	body []byte
	dir  bool
}

type attachmentFingerprint struct {
	sha256 [32]byte
	mode   fs.FileMode
}

type recordingLogger struct {
	infos []string
	warns []string
}

func (r *recordingLogger) Info(msg string, args ...any)  { r.infos = append(r.infos, msg) }
func (r *recordingLogger) Warn(msg string, args ...any)  { r.warns = append(r.warns, msg) }
func (r *recordingLogger) Error(msg string, args ...any) {}

func writeTarGzFixture(t *testing.T, path string, entries []tarFixtureEntry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir archive parent: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		name := filepath.ToSlash(entry.name)
		if entry.dir && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		hdr := &tar.Header{Name: name, Mode: entry.mode}
		if entry.dir {
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(entry.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %s: %v", entry.name, err)
		}
		if !entry.dir {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatalf("write tar body %s: %v", entry.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
}

func collectAttachmentFingerprints(t *testing.T, root string) map[string]attachmentFingerprint {
	t.Helper()
	attachmentDir := findUniqueDirNamed(t, root, "attachments")
	got := map[string]attachmentFingerprint{}
	if err := filepath.WalkDir(attachmentDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(attachmentDir, path)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = attachmentFingerprint{sha256: sha256.Sum256(data), mode: info.Mode()}
		return nil
	}); err != nil {
		t.Fatalf("walk attachments: %v", err)
	}
	return got
}

func assertFingerprintsEqual(t *testing.T, want, got map[string]attachmentFingerprint) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("attachment count mismatch: want %d, got %d (%#v)", len(want), len(got), got)
	}
	for name, wantFP := range want {
		gotFP, ok := got[name]
		if !ok {
			t.Fatalf("missing attachment %s", name)
		}
		if gotFP.sha256 != wantFP.sha256 {
			t.Fatalf("attachment %s SHA256 changed: want %x, got %x", name, wantFP.sha256, gotFP.sha256)
		}
		if gotFP.mode != wantFP.mode {
			t.Fatalf("attachment %s mode changed: want %s, got %s", name, wantFP.mode, gotFP.mode)
		}
	}
}

func findUniqueDirNamed(t *testing.T, root, name string) string {
	t.Helper()
	var matches []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == name {
			matches = append(matches, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one %q dir under %s, got %#v", name, root, matches)
	}
	return matches[0]
}

func readUniqueFileNamed(t *testing.T, root, name string) []byte {
	t.Helper()
	var matches []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == name {
			matches = append(matches, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one %q file under %s, got %#v", name, root, matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	return data
}

func hasWarningContaining(warnings []string, needle string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, needle) {
			return true
		}
	}
	return false
}

var _ Remapper = (*RealRemapper)(nil)
