package migrate_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/api"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/atomicfs"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/exporter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/rewriter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

func TestResumeLockAcquisitionSweepsTruncatedPartial(t *testing.T) {
	wd := newResumeWorkDir(t)
	partial := wd.RawGitArchive() + ".partial"
	mustWriteFile(t, partial, []byte("not a tar archive"))

	lockWorkDir(t, wd)
	defer unlockWorkDir(t, wd)

	assertNotExists(t, partial)
}

func TestResumeHalfExtractedDirWithoutCompleteTriggersReextract(t *testing.T) {
	wd := newResumeWorkDir(t)
	lockWorkDir(t, wd)
	writeSyntheticGitArchive(t, wd.RawGitArchive(), "real extracted content")
	mustWriteFile(t, filepath.Join(wd.GitExtractedDir(), "garbage.txt"), []byte("left by killed process"))
	unlockWorkDir(t, wd)

	lockWorkDir(t, wd)
	defer unlockWorkDir(t, wd)
	runResumeRewriter(t, wd, nil)

	assertNotExists(t, filepath.Join(wd.GitExtractedDir(), "garbage.txt"))
	assertFileContains(t, filepath.Join(wd.GitExtractedDir(), "repositories", "Acme", "widget.git", "HEAD"), "ref: refs/heads/main")
	assertFileContains(t, filepath.Join(wd.GitExtractedDir(), "repositories", "Acme", "widget.git", "resume-marker.txt"), "real extracted content")
	if !atomicfs.IsDirComplete(wd.GitExtractedDir()) {
		t.Fatalf("expected %s to be marked complete", wd.GitExtractedDir())
	}
}

func TestResumeCompleteMarkedExtractedDirIsSkipped(t *testing.T) {
	wd := newResumeWorkDir(t)
	lockWorkDir(t, wd)
	writeSyntheticGitArchive(t, wd.RawGitArchive(), "original archive content")
	runResumeRewriter(t, wd, nil)
	sentinel := filepath.Join(wd.GitExtractedDir(), "repositories", "Acme", "widget.git", "resume-sentinel.txt")
	mustWriteFile(t, sentinel, []byte("must survive"))
	mustRemove(t, wd.GitArchive()) // force Run past the final-archive cache so the extraction sentinel is exercised.
	unlockWorkDir(t, wd)

	lockWorkDir(t, wd)
	defer unlockWorkDir(t, wd)
	runResumeRewriter(t, wd, nil)

	assertFileContains(t, sentinel, "must survive")
}

func TestResumeFinalArchiveWithSentinelReturnsCached(t *testing.T) {
	wd := newResumeWorkDir(t)
	lockWorkDir(t, wd)
	writeSyntheticGitArchive(t, wd.RawGitArchive(), "archive content")
	runResumeRewriter(t, wd, nil)
	before := sha256File(t, wd.GitArchive())
	unlockWorkDir(t, wd)

	logger := &resumeLogger{}
	lockWorkDir(t, wd)
	defer unlockWorkDir(t, wd)
	runResumeRewriter(t, wd, logger)
	after := sha256File(t, wd.GitArchive())

	if before != after {
		t.Fatalf("expected cached final archive SHA to remain unchanged; before=%s after=%s", before, after)
	}
	if !strings.Contains(logger.joined(), "already exists") || !strings.Contains(logger.joined(), "skipping") {
		t.Fatalf("expected cached-style log message, got %q", logger.joined())
	}
}

func TestResumeFinalArchiveWithoutSentinelRebuilds(t *testing.T) {
	wd := newResumeWorkDir(t)
	lockWorkDir(t, wd)
	writeSyntheticGitArchive(t, wd.RawGitArchive(), "first archive content")
	runResumeRewriter(t, wd, nil)
	mustRemove(t, filepath.Join(wd.GitExtractedDir(), ".complete"))
	mustRemove(t, wd.GitArchive())
	mustWriteFile(t, filepath.Join(wd.GitExtractedDir(), "garbage-after-crash.txt"), []byte("stale"))
	unlockWorkDir(t, wd)

	lockWorkDir(t, wd)
	defer unlockWorkDir(t, wd)
	runResumeRewriter(t, wd, nil)

	assertExists(t, wd.GitArchive())
	if !atomicfs.IsDirComplete(wd.GitExtractedDir()) {
		t.Fatalf("expected %s to be marked complete after rebuild", wd.GitExtractedDir())
	}
	assertNotExists(t, filepath.Join(wd.GitExtractedDir(), "garbage-after-crash.txt"))
}

func TestResumeExporterModeMismatchRejectedBeforeAPICall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first exporter.Mode
		then  exporter.Mode
	}{
		{name: "combined_then_two", first: exporter.ModeCombined, then: exporter.ModeTwo},
		{name: "two_then_combined", first: exporter.ModeTwo, then: exporter.ModeCombined},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wd := newResumeWorkDir(t)
			var apiCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				apiCalls.Add(1)
				http.Error(w, "unexpected API call", http.StatusTeapot)
			}))
			defer srv.Close()
			client, err := api.NewForTesting(srv.URL)
			if err != nil {
				t.Fatalf("NewForTesting: %v", err)
			}

			lockWorkDir(t, wd)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = exporter.New(client, wd, exporter.Config{Org: "Acme", Repo: "widget", Mode: tc.first}).Run(ctx)
			assertFileContains(t, wd.ExportModeFile(), tc.first.String())
			unlockWorkDir(t, wd)

			apiCalls.Store(0)
			lockWorkDir(t, wd)
			defer unlockWorkDir(t, wd)
			err = exporter.New(client, wd, exporter.Config{Org: "Acme", Repo: "widget", Mode: tc.then}).Run(context.Background())
			if err == nil {
				t.Fatal("expected mode mismatch error")
			}
			msg := err.Error()
			for _, want := range []string{tc.first.String(), tc.then.String(), "fresh --work-dir"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("expected error %q to mention %q", msg, want)
				}
			}
			if got := apiCalls.Load(); got != 0 {
				t.Fatalf("expected mismatch to return before API calls; got %d calls", got)
			}
		})
	}
}

type resumeLogger struct{ lines []string }

func (l *resumeLogger) Info(msg string, args ...any)  { l.lines = append(l.lines, "INFO:"+msg) }
func (l *resumeLogger) Warn(msg string, args ...any)  { l.lines = append(l.lines, "WARN:"+msg) }
func (l *resumeLogger) Error(msg string, args ...any) { l.lines = append(l.lines, "ERROR:"+msg) }
func (l *resumeLogger) joined() string                { return strings.Join(l.lines, "\n") }

func newResumeWorkDir(t *testing.T) *workdir.WorkDir {
	t.Helper()
	wd, err := workdir.New(t.TempDir())
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}
	return wd
}

func lockWorkDir(t *testing.T, wd *workdir.WorkDir) {
	t.Helper()
	if err := wd.Lock(); err != nil {
		t.Fatalf("Lock: %v", err)
	}
}

func unlockWorkDir(t *testing.T, wd *workdir.WorkDir) {
	t.Helper()
	if err := wd.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

func runResumeRewriter(t *testing.T, wd *workdir.WorkDir, logger output.Logger) *rewriter.Result {
	t.Helper()
	res, err := rewriter.New(wd, nil, nil, logger, rewriter.Config{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Rewriter.Run: %v", err)
	}
	if res == nil {
		t.Fatal("Rewriter.Run returned nil result")
	}
	return res
}

func writeSyntheticGitArchive(t *testing.T, outPath, marker string) {
	t.Helper()
	files := map[string][]byte{
		"organizations_000001.json":                      []byte("{}\n"),
		"repositories_000001.json":                       []byte("[]\n"),
		"schema.json":                                    []byte("{}\n"),
		"repositories/Acme/widget.git/HEAD":              []byte("ref: refs/heads/main\n"),
		"repositories/Acme/widget.git/refs/heads/main":   []byte("0000000000000000000000000000000000000000\n"),
		"repositories/Acme/widget.git/resume-marker.txt": []byte(marker),
	}
	writeTarGz(t, files, outPath)
}

func writeTarGz(t *testing.T, files map[string][]byte, outPath string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for path, content := range files {
		hdr := &tar.Header{Name: path, Mode: 0o644, Size: int64(len(content)), ModTime: time.Unix(1, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%s): %v", path, err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("Write(%s): %v", path, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	mustWriteFile(t, outPath, buf.Bytes())
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove(%s): %v", path, err)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s not to exist, stat err=%v", path, err)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("expected %s to contain %q, got %q", path, want, string(data))
	}
}
