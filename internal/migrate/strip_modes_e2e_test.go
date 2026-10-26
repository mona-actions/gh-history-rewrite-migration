package migrate

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	commitarchive "github.com/mona-actions/gh-commit-remap/pkg/archive"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/exporter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/rewriter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	stripFixtureRepo      = "strip-fixture"
	stripFixtureLargePath = "assets/data.bin"
	stripFixtureKeepPath  = "README.md"
	stripFixtureThreshold = 1024
)

// stripFixture is a synthetic GEI export whose stripFixtureLargePath has a
// small revision followed by a revision larger than stripFixtureThreshold.
type stripFixture struct {
	archives  e2eArchives
	smallBlob string
	largeBlob string
	keepBlob  string
}

func TestOrchestratorE2E_StripLargeFiles_SizeMode(t *testing.T) {
	requireFilterRepo(t)
	fx := buildStripFixture(t)

	res := runOrchestratorE2E(t, exporter.ModeTwo, fx.archives, rewriter.Config{
		StripLargeFiles:    true,
		LargeFileThreshold: stripFixtureThreshold,
		SkipConfirm:        true,
	})

	assert.Equal(t, "size", tableValue(res.output.tables, "strip mode"))
	assert.Equal(t, "yes", tableValue(res.output.tables, "strip performed"))
	assert.Equal(t, "1", tableValue(res.output.tables, "blobs stripped (up to)"))

	blobList := string(mustReadFile(t, res.wd.LargeBlobsTxt()))
	assert.Equal(t, fx.largeBlob+"\t4096\n", blobList, "large-blobs.txt should list exactly the oversized blob")
	assert.NoFileExists(t, res.wd.CleanupTxt(), "size mode must not write cleanup.txt")
	assert.NoDirExists(t, res.wd.AnalysisDir(), "size mode must not run filter-repo --analyze")

	assert.FileExists(t, res.wd.CommitMap())
	assert.NotZero(t, fileSize(t, res.wd.CommitMap()), "size-mode rewrite should produce a non-empty commit-map")

	bare := rewrittenBareRepo(t, res.wd)
	objects := revListObjects(t, bare)
	assert.NotContains(t, objects, fx.largeBlob, "oversized blob should be unreachable after the strip")
	assert.False(t, gitObjectExists(t, bare, fx.largeBlob), "oversized blob should not ship in the rewritten archive")
	assert.Contains(t, objects, fx.smallBlob, "small revision of the same path should survive a size-mode strip")
	assert.True(t, gitObjectExists(t, bare, fx.smallBlob))
	assert.Contains(t, objects, fx.keepBlob)
	assert.NotEmpty(t, gitOutput(t, bare, "log", "--all", "--format=%H", "--", stripFixtureLargePath),
		"size mode strips blobs, not paths, so the path should remain in history")

	assertNoPartials(t, res.root)
	assertImportReceivedArchives(t, res.importer, res.wd)
}

func TestOrchestratorE2E_StripLargeFiles_PathMode(t *testing.T) {
	requireFilterRepo(t)
	fx := buildStripFixture(t)

	res := runOrchestratorE2E(t, exporter.ModeTwo, fx.archives, rewriter.Config{
		StripLargeFiles:    true,
		Analyze:            true,
		LargeFileThreshold: stripFixtureThreshold,
		SkipConfirm:        true,
	})

	assert.Equal(t, "path", tableValue(res.output.tables, "strip mode"))
	assert.Equal(t, "yes", tableValue(res.output.tables, "strip performed"))
	assert.Equal(t, "1", tableValue(res.output.tables, "paths stripped"))

	assert.Equal(t, stripFixtureLargePath+"\n", string(mustReadFile(t, res.wd.CleanupTxt())))
	assert.NoFileExists(t, res.wd.LargeBlobsTxt(), "path mode must not write large-blobs.txt")
	assertAnalysisReportOutsideArchive(t, res.wd)

	assert.FileExists(t, res.wd.CommitMap())
	assert.NotZero(t, fileSize(t, res.wd.CommitMap()))

	bare := rewrittenBareRepo(t, res.wd)
	objects := revListObjects(t, bare)
	assert.NotContains(t, objects, fx.largeBlob)
	assert.NotContains(t, objects, fx.smallBlob, "path mode removes every revision of the flagged path")
	assert.False(t, gitObjectExists(t, bare, fx.largeBlob))
	assert.NotContains(t, objects, stripFixtureLargePath)
	assert.Empty(t, gitOutput(t, bare, "log", "--all", "--format=%H", "--", stripFixtureLargePath),
		"flagged path should be gone from all of history")
	assert.Contains(t, objects, fx.keepBlob, "unflagged paths should be preserved")
	assertNoAnalysisInArchive(t, res.wd.GitArchive())

	assertNoPartials(t, res.root)
	assertImportReceivedArchives(t, res.importer, res.wd)
}

func TestOrchestratorE2E_AnalyzeOnly(t *testing.T) {
	requireFilterRepo(t)
	fx := buildStripFixture(t)

	res := runOrchestratorE2E(t, exporter.ModeTwo, fx.archives, rewriter.Config{
		Analyze:            true,
		LargeFileThreshold: stripFixtureThreshold,
	})

	assert.Equal(t, "analysis only (no changes)", tableValue(res.output.tables, "mode"))
	assert.Equal(t, "1", tableValue(res.output.tables, "paths over threshold"))
	assert.Contains(t, strings.Join(res.output.infos, "\n"), "Analysis only")

	assert.False(t, res.importer.runCalled, "report-only run must not import")
	assert.Empty(t, res.output.successes)
	assert.NoFileExists(t, res.wd.GitArchive(), "report-only run must not write a rewritten archive")
	assert.NoFileExists(t, res.wd.MetadataArchive())
	assert.NoFileExists(t, res.wd.CommitMap(), "report-only run must not hand off a commit-map")
	assert.NoFileExists(t, res.wd.CleanupTxt())
	assert.NoFileExists(t, res.wd.LargeBlobsTxt())
	assertAnalysisReportOutsideArchive(t, res.wd)

	repos, err := workdir.FindBareRepo(res.wd.GitExtractedDir())
	require.NoError(t, err)
	assert.True(t, gitObjectExists(t, repos.Main, fx.largeBlob), "report-only run must leave history untouched")
	assert.Contains(t, revListObjects(t, repos.Main), fx.largeBlob)
	assertNoPartials(t, res.root)
}

// buildStripFixture builds a GEI-style git + metadata archive pair around a
// bare repo whose history contains a small and a large revision of one path.
func buildStripFixture(t *testing.T) stripFixture {
	t.Helper()
	work := t.TempDir()
	gitRun(t, work, "init", "-q")
	writeFile(t, work, stripFixtureKeepPath, "# strip fixture\n")
	writeFile(t, work, stripFixtureLargePath, "small version\n")
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "initial")

	writeFile(t, work, stripFixtureLargePath, strings.Repeat("0123456789abcdef", 256))
	gitRun(t, work, "commit", "-q", "-am", "grow data.bin past the threshold")

	writeFile(t, work, stripFixtureKeepPath, "# strip fixture\n\nupdated\n")
	gitRun(t, work, "commit", "-q", "-am", "update readme")

	fx := stripFixture{
		smallBlob: gitOutput(t, work, "rev-parse", "HEAD~2:"+stripFixtureLargePath),
		largeBlob: gitOutput(t, work, "rev-parse", "HEAD:"+stripFixtureLargePath),
		keepBlob:  gitOutput(t, work, "rev-parse", "HEAD:"+stripFixtureKeepPath),
	}

	gitRoot := t.TempDir()
	bare := filepath.Join(gitRoot, "repositories", e2eSourceOrg, stripFixtureRepo+".git")
	require.NoError(t, os.MkdirAll(filepath.Dir(bare), 0o755))
	gitRun(t, gitRoot, "clone", "-q", "--bare", "--no-local", work, bare)
	gitRun(t, bare, "remote", "remove", "origin")
	writeFile(t, gitRoot, "organizations_000001.json", `[{"login":"acme-org"}]`)
	writeFile(t, gitRoot, "repositories_000001.json", `[{"name":"`+stripFixtureRepo+`"}]`)
	writeFile(t, gitRoot, "schema.json", `{"version":"1.0.1"}`)

	metadataRoot := t.TempDir()
	writeFile(t, metadataRoot, "issues_000001.json", `[]`)
	writeFile(t, metadataRoot, "organizations_000001.json", `[{"login":"acme-org"}]`)
	writeFile(t, metadataRoot, "repositories_000001.json", `[{"name":"`+stripFixtureRepo+`"}]`)
	writeFile(t, metadataRoot, "schema.json", `{"version":"1.0.1"}`)

	gitOut := filepath.Join(t.TempDir(), "strip-git.tar.gz")
	metadataOut := filepath.Join(t.TempDir(), "strip-metadata.tar.gz")
	require.NoError(t, commitarchive.ReTarDir(gitRoot, gitOut))
	require.NoError(t, commitarchive.ReTarDir(metadataRoot, metadataOut))
	fx.archives = e2eArchives{git: mustReadFile(t, gitOut), metadata: mustReadFile(t, metadataOut)}
	return fx
}

// assertAnalysisReportOutsideArchive checks the --analyze report landed in
// wd.AnalysisDir(), outside the extracted tree that gets re-archived.
func assertAnalysisReportOutsideArchive(t *testing.T, wd *workdir.WorkDir) {
	t.Helper()
	assert.FileExists(t, filepath.Join(wd.AnalysisDir(), "path-all-sizes.txt"))
	rel, err := filepath.Rel(wd.GitExtractedDir(), wd.AnalysisDir())
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(rel, ".."), "analysis dir %s must not be inside the git extraction dir", wd.AnalysisDir())

	repos, err := workdir.FindBareRepo(wd.GitExtractedDir())
	require.NoError(t, err)
	assert.NoDirExists(t, filepath.Join(repos.Main, "filter-repo", "analysis"),
		"analysis report must not be written inside the bare repo")
}

func assertNoAnalysisInArchive(t *testing.T, archivePath string) {
	t.Helper()
	root := extractArchive(t, archivePath)
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "analysis" {
			t.Errorf("analysis directory shipped in archive: %s", path)
		}
		if d.Name() == "path-all-sizes.txt" {
			t.Errorf("analysis report shipped in archive: %s", path)
		}
		return nil
	}))
}

func rewrittenBareRepo(t *testing.T, wd *workdir.WorkDir) string {
	t.Helper()
	repos, err := workdir.FindBareRepo(extractArchive(t, wd.GitArchive()))
	require.NoError(t, err)
	return repos.Main
}

func revListObjects(t *testing.T, repo string) string {
	t.Helper()
	return gitOutput(t, repo, "rev-list", "--objects", "--all")
}

func gitObjectExists(t *testing.T, repo, sha string) bool {
	t.Helper()
	cmd := exec.Command("git", "-c", "safe.bareRepository=all", "cat-file", "-e", sha)
	cmd.Dir = repo
	return cmd.Run() == nil
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = gitOutput(t, dir, args...)
}

// gitOutput runs git with a hermetic identity and no signing so fixtures
// build the same way regardless of the host's git config.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{
		"-c", "safe.bareRepository=all",
		"-c", "commit.gpgsign=false",
		"-c", "init.defaultBranch=main",
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@example.com",
	}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2024-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2024-01-01T00:00:00Z",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// tableValue returns the value for metric from the last recorded
// metric/value table containing it.
func tableValue(tables [][][]string, metric string) string {
	for i := len(tables) - 1; i >= 0; i-- {
		for _, row := range tables[i] {
			if len(row) == 2 && row[0] == metric {
				return row[1]
			}
		}
	}
	return ""
}
