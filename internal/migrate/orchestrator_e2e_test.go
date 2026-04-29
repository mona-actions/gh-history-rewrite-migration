package migrate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/api"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/exporter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/filterrepo"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/largefiles"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/remap"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/rewriter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// --- Integration-test helpers --------------------------------------------

// migrationsHandler returns an http.Handler that mimics enough of the
// GitHub org-migrations REST API for the real go-github client to drive
// the export phase end-to-end. It serves the supplied tarball bytes
// for the archive download.
func migrationsHandler(t *testing.T, archive []byte, calls *apiCallCounts) http.Handler {
	t.Helper()
	mux := http.NewServeMux()

	// POST /orgs/{org}/migrations -> 201 {"id":42,"state":"pending"}
	// GET /orgs/{org}/migrations/{id} -> {"id":42,"state":"exported"}
	mux.HandleFunc("/orgs/acme/migrations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		calls.start++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintln(w, `{"id":42,"state":"pending"}`)
	})

	mux.HandleFunc("/orgs/acme/migrations/42", func(w http.ResponseWriter, r *http.Request) {
		calls.poll++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"id":42,"state":"exported"}`)
	})

	// GET /orgs/{org}/migrations/{id}/archive -> 302 to /download/42.tar.gz
	// DELETE /orgs/{org}/migrations/{id}/archive -> 204
	mux.HandleFunc("/orgs/acme/migrations/42/archive", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			calls.archiveURL++
			// go-github's MigrationArchiveURL extracts the Location header
			// from the redirect response; the actual download follows next.
			http.Redirect(w, r, calls.serverURL+"/download/42.tar.gz", http.StatusFound)
		case http.MethodDelete:
			calls.delete++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/download/42.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		calls.download++
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(archive)
	})

	return mux
}

type apiCallCounts struct {
	start, poll, archiveURL, download, delete int
	serverURL                                 string
}

// buildBareRepoArchive creates a real bare git repo at <scratch>/<RepoName>.git,
// optionally seeds a single commit so filter-repo has something to rewrite,
// then tarballs `<prefix>/<RepoName>.git/` plus a fake metadata sibling JSON
// into a gzipped tarball returned as bytes.
func buildBareRepoArchive(t *testing.T, repoName, prefix string, withCommit bool) []byte {
	t.Helper()
	scratch := t.TempDir()
	bare := filepath.Join(scratch, repoName+".git")
	mustRun(t, scratch, "git", "init", "--bare", bare)

	if withCommit {
		// Seed a real commit by initing a non-bare working repo elsewhere
		// and pushing into the bare. filter-repo refuses to operate on a
		// repo with zero commits.
		work := filepath.Join(scratch, "seed")
		mustRun(t, scratch, "git", "init", work)
		mustRun(t, work, "git", "config", "user.email", "t@t.test")
		mustRun(t, work, "git", "config", "user.name", "tester")
		mustRun(t, work, "git", "config", "commit.gpgsign", "false")
		if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi"), 0644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		mustRun(t, work, "git", "add", "README.md")
		mustRun(t, work, "git", "commit", "-m", "seed")
		mustRun(t, work, "git", "push", bare, "HEAD:refs/heads/main")
	}

	// Build the tarball: prefix/<repoName>.git/<everything> + metadata sibling.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name: prefix + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("write prefix dir: %v", err)
	}

	root := bare
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		tarName := prefix + "/" + repoName + ".git"
		if rel != "." {
			tarName = tarName + "/" + filepath.ToSlash(rel)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = tarName
		if info.IsDir() {
			hdr.Name += "/"
			hdr.Typeflag = tar.TypeDir
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk bare repo: %v", err)
	}

	// Sibling metadata JSON — survives extraction even though we don't remap it here.
	meta := []byte(`{"schema_version":"1.2.0","repositories":[]}`)
	if err := tw.WriteHeader(&tar.Header{
		Name:     prefix + "/repositories_000001.json",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(meta)),
		ModTime:  time.Now(),
	}); err != nil {
		t.Fatalf("write metadata header: %v", err)
	}
	if _, err := tw.Write(meta); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return buf.Bytes()
}

func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// hasFilterRepo reports whether `git filter-repo` is available on PATH.
func hasFilterRepo() bool {
	cmd := exec.Command("git", "filter-repo", "--version")
	return cmd.Run() == nil
}

// --- Integration test ----------------------------------------------------

// TestOrchestrator_E2E_RemapPending exercises the migrate Orchestrator with
// real implementations end-to-end:
//   - real workdir.WorkDir against t.TempDir()
//   - real api.API (via api.NewForTesting) pointing at a real httptest.Server
//     that mimics enough of the GitHub migrations REST API for the real
//     go-github client to drive the export phase
//   - real exporter.Exporter, exercising the production code path including
//     start → poll → MigrationArchiveURL redirect → stream-to-disk → extract
//   - real filter-repo binary if present, else skip — the test exercises the
//     real Rewriter+Runner exec path with a no-op passthrough flag
//     (--refs HEAD), validated up-front against ValidateUserFlags
//   - StubRemapper returns ErrUpstreamPending; orchestrator MUST surface
//     that and skip the import phase entirely
//   - Importer is a fake; we assert it is NEVER invoked end-to-end
func TestOrchestrator_E2E_RemapPending(t *testing.T) {
	if !hasFilterRepo() {
		t.Skip("git filter-repo not on PATH; skipping hermetic e2e test")
	}

	const (
		repoName = "Repo"
		prefix   = "myprefix"
		org      = "acme"
	)

	// 1. Build a real tarball containing a real bare repo with one commit.
	archiveBytes := buildBareRepoArchive(t, repoName, prefix, true)

	// 2. httptest server impersonating the migrations API.
	calls := &apiCallCounts{}
	srv := httptest.NewServer(migrationsHandler(t, archiveBytes, calls))
	defer srv.Close()
	calls.serverURL = srv.URL

	// 3. Real WorkDir.
	wd, err := workdir.New(t.TempDir())
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}

	// 4. Real *api.API pointed at the httptest server. NewForTesting bypasses
	//    api.New's hard-coded https://hostname plumbing — production callers
	//    must use api.New(); tests use NewForTesting.
	a, err := api.NewForTesting(srv.URL)
	if err != nil {
		t.Fatalf("api.NewForTesting: %v", err)
	}
	exp := exporter.New(a, wd, exporter.Config{
		Org:  org,
		Repo: repoName,
	})

	// 5. Real Rewriter wrapping a real filter-repo Runner. --refs HEAD is a
	//    no-op rewrite that exercises the real exec path; verify it passes
	//    ValidateUserFlags before relying on it.
	if err := filterrepo.ValidateUserFlags([]string{"--refs", "HEAD"}, false); err != nil {
		t.Fatalf("--refs HEAD must be allowed by ValidateUserFlags: %v", err)
	}
	runner := filterrepo.New(filterrepo.DefaultExecer{}, nil)
	analyzer := largefiles.New(runner, nil, 0)
	rw := rewriter.New(wd, runner, analyzer, nil, rewriter.Config{
		FilterRepoFlags: []string{"--refs", "HEAD"},
	})

	// 6. StubRemapper — returns ErrUpstreamPending.
	rmp := remap.NewStub(nil)

	// 7. Importer must NOT be invoked.
	im := &fakeImporter{}

	// 8. Build orchestrator and run.
	rec := &recorder{}
	o := New(wd, nil, exp, rw, rmp, im, remap.Input{
		WorkDir:       wd,
		ExtractedDir:  wd.Extracted(),
		CommitMapPath: wd.CommitMap(),
	}, Config{TargetRepoURL: "https://github.com/dst/repo"}, rec.printers())

	// Bound runtime — the test should be fast in CI.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err = o.Run(ctx)
	if !errors.Is(err, remap.ErrUpstreamPending) {
		t.Fatalf("expected ErrUpstreamPending from orchestrator; got %v", err)
	}

	// 9. Assert HTTP boundary was actually hit by the real *api.API.
	if calls.start != 1 {
		t.Errorf("expected 1 StartMigration call; got %d", calls.start)
	}
	if calls.poll < 1 {
		t.Errorf("expected at least 1 MigrationStatus poll; got %d", calls.poll)
	}
	if calls.archiveURL != 1 {
		t.Errorf("expected 1 MigrationArchiveURL call; got %d", calls.archiveURL)
	}
	if calls.download != 1 {
		t.Errorf("expected exactly 1 archive download; got %d", calls.download)
	}
	// Note: the orchestrator aborts before the cleanup-archive call when
	// remap fails, so we don't assert calls.delete here.

	// 10. Importer must not have been called.
	if im.called {
		t.Errorf("importer must NOT be invoked when remap returns ErrUpstreamPending")
	}

	// 11. Work-dir layout assertions.
	if !wd.HasArchive() {
		t.Errorf("expected archive.tar.gz in workdir at %s", wd.Archive())
	}
	expectedBareRepo := filepath.Join(wd.Extracted(), prefix, repoName+".git")
	if _, err := os.Stat(expectedBareRepo); err != nil {
		t.Errorf("expected extracted bare repo at %s: %v", expectedBareRepo, err)
	}
	bare, err := wd.BareRepoPath()
	if err != nil {
		t.Errorf("BareRepoPath: %v", err)
	} else if bare != expectedBareRepo {
		t.Errorf("BareRepoPath = %s, want %s", bare, expectedBareRepo)
	}

	// 12. Real filter-repo with --refs HEAD should have produced a commit-map.
	//     The Rewriter hands it off to wd.CommitMap().
	if !wd.HasCommitMap() {
		t.Errorf("expected commit-map at %s after real filter-repo run", wd.CommitMap())
	}

	// 13. Output should include the upstream-pending warning.
	out := rec.joined()
	if !strings.Contains(out, "MANUAL-REMAP.md") {
		t.Errorf("expected MANUAL-REMAP.md pointer in output; got: %s", out)
	}
}
