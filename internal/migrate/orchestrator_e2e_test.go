package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	commitarchive "github.com/mona-actions/gh-commit-remap/pkg/archive"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/api"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/exporter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/filterrepo"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/importer"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/largefiles"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/remap"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/rewriter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	e2eSourceOrg      = "acme-org"
	e2eSourceRepo     = "a-repo-with-lfs"
	e2eTargetOrg      = "target-org"
	e2eTargetRepo     = "target-repo"
	e2eSourceHostname = "ghes.example.com"
)

type e2eArchives struct {
	git      []byte
	metadata []byte
	combined []byte
}

type e2ePipelineResult struct {
	wd       *workdir.WorkDir
	root     string
	server   *e2eMigrationServer
	importer *recordingImporterExecer
	output   *recordingPrinters
}

func TestOrchestratorE2E_TwoMode_RealFixture(t *testing.T) {
	requireFilterRepo(t)
	archives := realFixtureArchives(t)

	res := runOrchestratorE2E(t, exporter.ModeTwo, archives, rewriter.Config{
		StripLargeFiles:    true,
		LargeFileThreshold: 1,
		SkipConfirm:        true,
	})

	assert.True(t, res.server.started, "expected lock/run wrapper to start the pipeline")
	assert.NoFileExists(t, filepath.Join(res.root, ".lock"), "lock should be released")
	assert.Equal(t, archives.git, mustReadFile(t, res.wd.RawGitArchive()), "raw git archive should be downloaded unchanged")
	assert.Equal(t, archives.metadata, mustReadFile(t, res.wd.RawMetadataArchive()), "raw metadata archive should be downloaded unchanged")
	assert.FileExists(t, res.wd.CommitMap(), "rewriter should hand off a commit-map")
	assert.NotZero(t, fileSize(t, res.wd.CommitMap()), "real rewrite should produce a non-empty commit-map")
	assert.FileExists(t, res.wd.GitArchive())
	assert.FileExists(t, res.wd.MetadataArchive())
	assert.FileExists(t, filepath.Join(res.wd.GitExtractedDir(), ".complete"))
	assert.FileExists(t, filepath.Join(res.wd.MetadataExtractedDir(), ".complete"))
	assertNoPartials(t, res.root)
	assertImportReceivedArchives(t, res.importer, res.wd)

	require.Len(t, res.server.payloads, 2)
	assert.True(t, res.server.payloads[0].ExcludeMetadata, "first two-mode export should be git-only")
	assert.True(t, res.server.payloads[1].ExcludeGitData, "second two-mode export should be metadata-only")
	assert.Equal(t, 2, res.server.downloads)
	assert.Equal(t, 2, res.server.deletes)
}

func TestOrchestratorE2E_EmptyRepo(t *testing.T) {
	gitArchive, metadataArchive := emptyRepoArchives(t)
	res := runOrchestratorE2E(t, exporter.ModeTwo, e2eArchives{git: gitArchive, metadata: metadataArchive}, rewriter.Config{})

	assert.NoFileExists(t, filepath.Join(res.root, ".lock"), "lock should be released")
	assert.FileExists(t, res.wd.CommitMap())
	assert.Zero(t, fileSize(t, res.wd.CommitMap()), "empty fixture carries an empty commit-map")
	assert.Equal(t, metadataArchive, mustReadFile(t, res.wd.MetadataArchive()), "empty commit-map should copy metadata unchanged")
	assert.FileExists(t, res.wd.GitArchive())
	assertNoPartials(t, res.root)
	assertImportReceivedArchives(t, res.importer, res.wd)
	assert.Contains(t, strings.Join(res.output.warns, "\n"), "commit-map is empty")
}

func TestOrchestratorE2E_CombinedMode(t *testing.T) {
	requireFilterRepo(t)
	archives := realFixtureArchives(t)
	archives.combined = combinedFixtureArchive(t, archives.git, archives.metadata)
	rewriteCfg := rewriter.Config{StripLargeFiles: true, LargeFileThreshold: 1, SkipConfirm: true}

	twoMode := runOrchestratorE2E(t, exporter.ModeTwo, archives, rewriteCfg)
	combinedMode := runOrchestratorE2E(t, exporter.ModeCombined, archives, rewriteCfg)

	require.Len(t, combinedMode.server.payloads, 1)
	assert.False(t, combinedMode.server.payloads[0].ExcludeMetadata)
	assert.False(t, combinedMode.server.payloads[0].ExcludeGitData)
	assert.Equal(t, 1, combinedMode.server.downloads)
	assertBareRepoBytesEqual(t, twoMode.wd.GitArchive(), combinedMode.wd.GitArchive())
	assert.Equal(t,
		metadataFileSet(t, twoMode.wd.MetadataArchive()),
		metadataFileSet(t, combinedMode.wd.MetadataArchive()),
		"combined split output should preserve the same metadata structure as default two-mode",
	)
	assertNoPartials(t, combinedMode.root)
	assertImportReceivedArchives(t, combinedMode.importer, combinedMode.wd)
}

func runOrchestratorE2E(t *testing.T, mode exporter.Mode, archives e2eArchives, rwCfg rewriter.Config) e2ePipelineResult {
	t.Helper()
	withPATEnv(t, "source-token", "target-token")

	root := t.TempDir()
	wd, err := workdir.New(root)
	require.NoError(t, err)

	server := newE2EMigrationServer(t, archives)
	defer server.close()
	apiClient, err := api.NewForTesting(server.URL())
	require.NoError(t, err)

	exp := exporter.New(apiClient, wd, exporter.Config{
		Org:  e2eSourceOrg,
		Repo: e2eSourceRepo,
		Mode: mode,
	})
	frRunner := filterrepo.New(filterrepo.DefaultExecer{}, nil).WithStdout(io.Discard)
	analyzer := largefiles.New(frRunner, nil, rwCfg.LargeFileThreshold)
	rw := rewriter.New(wd, frRunner, analyzer, nil, rwCfg)
	remapper := remap.NewReal(nil)
	impExec := &recordingImporterExecer{}
	imp := importer.New(wd, importer.Config{
		SourceOrg:      e2eSourceOrg,
		SourceRepo:     e2eSourceRepo,
		TargetOrg:      e2eTargetOrg,
		TargetRepo:     e2eTargetRepo,
		SourceHostname: e2eSourceHostname,
		Confirm:        true,
	}, impExec)
	out := &recordingPrinters{}

	orch := New(wd, nil, exp, rw, remapper, imp, remap.Input{
		WorkDir:              wd,
		RawMetadataArchive:   wd.RawMetadataArchive(),
		CommitMapPath:        wd.CommitMap(),
		MetadataExtractedDir: wd.MetadataExtractedDir(),
	}, Config{TargetRepoURL: fmt.Sprintf("https://github.com/%s/%s", e2eTargetOrg, e2eTargetRepo)}, out.printers())

	require.NoError(t, wd.Lock())
	server.started = true
	runErr := orch.Run(context.Background())
	unlockErr := wd.Unlock()
	require.NoError(t, unlockErr)
	require.NoError(t, runErr)

	return e2ePipelineResult{wd: wd, root: root, server: server, importer: impExec, output: out}
}

type e2eMigrationServer struct {
	t         *testing.T
	server    *httptest.Server
	archives  e2eArchives
	nextID    int64
	payloads  []api.MigrationOpts
	downloads int
	deletes   int
	started   bool
	byID      map[int64][]byte
}

func newE2EMigrationServer(t *testing.T, archives e2eArchives) *e2eMigrationServer {
	t.Helper()
	s := &e2eMigrationServer{t: t, archives: archives, byID: map[int64][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.server = httptest.NewServer(mux)
	return s
}

func (s *e2eMigrationServer) URL() string { return s.server.URL }
func (s *e2eMigrationServer) close()      { s.server.Close() }

func (s *e2eMigrationServer) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "orgs" && parts[2] == "migrations" {
		s.handleMigration(w, r, parts)
		return
	}
	if len(parts) == 2 && parts[0] == "download" {
		id, err := strconv.ParseInt(strings.TrimSuffix(parts[1], ".tar.gz"), 10, 64)
		require.NoError(s.t, err)
		archiveBytes := s.byID[id]
		require.NotEmpty(s.t, archiveBytes)
		s.downloads++
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(archiveBytes)
		return
	}
	http.NotFound(w, r)
}

func (s *e2eMigrationServer) handleMigration(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 3 && r.Method == http.MethodPost:
		var opts api.MigrationOpts
		require.NoError(s.t, json.NewDecoder(r.Body).Decode(&opts))
		s.payloads = append(s.payloads, opts)
		s.nextID++
		s.byID[s.nextID] = s.archiveFor(opts)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%d,"state":"pending"}`, s.nextID)
	case len(parts) == 4 && r.Method == http.MethodGet:
		id, err := strconv.ParseInt(parts[3], 10, 64)
		require.NoError(s.t, err)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%d,"state":"exported"}`, id)
	case len(parts) == 5 && parts[4] == "archive" && r.Method == http.MethodGet:
		http.Redirect(w, r, s.server.URL+"/download/"+parts[3]+".tar.gz", http.StatusFound)
	case len(parts) == 5 && parts[4] == "archive" && r.Method == http.MethodDelete:
		s.deletes++
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (s *e2eMigrationServer) archiveFor(opts api.MigrationOpts) []byte {
	switch {
	case opts.ExcludeMetadata:
		return s.archives.git
	case opts.ExcludeGitData:
		return s.archives.metadata
	default:
		return s.archives.combined
	}
}

type recordingImporterExecer struct {
	gotName         string
	gotArgs         []string
	gotEnv          []string
	gitArchive      []byte
	metadataArchive []byte
	runCalled       bool
}

func (r *recordingImporterExecer) LookPath(name string) (string, error) {
	return "/usr/bin/" + name, nil
}

func (r *recordingImporterExecer) Run(_ context.Context, name string, args []string, env []string) (string, error) {
	r.runCalled = true
	r.gotName = name
	r.gotArgs = append([]string(nil), args...)
	r.gotEnv = append([]string(nil), env...)
	gitPath, ok := argValue(args, "--git-archive-path")
	if !ok {
		return "", fmt.Errorf("missing --git-archive-path in %v", args)
	}
	metadataPath, ok := argValue(args, "--metadata-archive-path")
	if !ok {
		return "", fmt.Errorf("missing --metadata-archive-path in %v", args)
	}
	var err error
	r.gitArchive, err = os.ReadFile(gitPath)
	if err != nil {
		return "", err
	}
	r.metadataArchive, err = os.ReadFile(metadataPath)
	return "", err
}

type recordingPrinters struct {
	infos, warns, errors, successes []string
	tables                          [][][]string
}

func (r *recordingPrinters) printers() Printers {
	return Printers{
		Info:    func(msg string) { r.infos = append(r.infos, msg) },
		Warn:    func(msg string) { r.warns = append(r.warns, msg) },
		Error:   func(msg string) { r.errors = append(r.errors, msg) },
		Success: func(msg string) { r.successes = append(r.successes, msg) },
		Table: func(_ []string, rows [][]string) {
			r.tables = append(r.tables, rows)
		},
	}
}

func realFixtureArchives(t *testing.T) e2eArchives {
	t.Helper()
	root := findRepoRoot(t)
	fixtureDir := filepath.Join(root, "internal", "testdata", "gei-real")
	return e2eArchives{
		git:      mustReadFile(t, filepath.Join(fixtureDir, "git-archive.tar.gz")),
		metadata: mustReadFile(t, filepath.Join(fixtureDir, "metadata-archive.tar.gz")),
	}
}

// combinedFixtureArchive repacks the sanitized two real GEI fixture archives
// into one tarball so combined-mode exporter splitting exercises the same
// repository and metadata bytes as default two-mode.
func combinedFixtureArchive(t *testing.T, gitArchive, metadataArchive []byte) []byte {
	t.Helper()
	root := t.TempDir()
	gitPath := filepath.Join(t.TempDir(), "git-archive.tar.gz")
	metadataPath := filepath.Join(t.TempDir(), "metadata-archive.tar.gz")
	require.NoError(t, os.WriteFile(gitPath, gitArchive, 0o644))
	require.NoError(t, os.WriteFile(metadataPath, metadataArchive, 0o644))
	_, err := commitarchive.UnTar(gitPath, root)
	require.NoError(t, err)
	_, err = commitarchive.UnTar(metadataPath, root)
	require.NoError(t, err)
	out := filepath.Join(t.TempDir(), "combined.tar.gz")
	require.NoError(t, commitarchive.ReTarDir(root, out))
	return mustReadFile(t, out)
}

func emptyRepoArchives(t *testing.T) ([]byte, []byte) {
	t.Helper()
	gitRoot := t.TempDir()
	bare := filepath.Join(gitRoot, "repositories", e2eSourceOrg, "empty-repo.git")
	require.NoError(t, os.MkdirAll(filepath.Dir(bare), 0o755))
	mustRun(t, gitRoot, "git", "init", "--bare", bare)
	require.NoError(t, os.MkdirAll(filepath.Join(bare, "filter-repo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bare, "filter-repo", "commit-map"), nil, 0o644))
	writeFile(t, gitRoot, "organizations_000001.json", `[{"login":"acme-org"}]`)
	writeFile(t, gitRoot, "repositories_000001.json", `[{"name":"empty-repo"}]`)
	writeFile(t, gitRoot, "schema.json", `{"version":"1.0.1"}`)

	metadataRoot := t.TempDir()
	writeFile(t, metadataRoot, "issues_000001.json", `[]`)
	writeFile(t, metadataRoot, "organizations_000001.json", `[{"login":"acme-org"}]`)
	writeFile(t, metadataRoot, "repositories_000001.json", `[{"name":"empty-repo"}]`)
	writeFile(t, metadataRoot, "schema.json", `{"version":"1.0.1"}`)

	gitOut := filepath.Join(t.TempDir(), "empty-git.tar.gz")
	metadataOut := filepath.Join(t.TempDir(), "empty-metadata.tar.gz")
	require.NoError(t, commitarchive.ReTarDir(gitRoot, gitOut))
	require.NoError(t, commitarchive.ReTarDir(metadataRoot, metadataOut))
	return mustReadFile(t, gitOut), mustReadFile(t, metadataOut)
}

func assertImportReceivedArchives(t *testing.T, execer *recordingImporterExecer, wd *workdir.WorkDir) {
	t.Helper()
	require.True(t, execer.runCalled, "importer should call gh gei")
	assert.True(t, strings.HasSuffix(execer.gotName, "/gh"))
	assert.True(t, argsContain(execer.gotArgs, "--git-archive-path", wd.GitArchive()))
	assert.True(t, argsContain(execer.gotArgs, "--metadata-archive-path", wd.MetadataArchive()))
	assert.NotEmpty(t, execer.gitArchive)
	assert.NotEmpty(t, execer.metadataArchive)
}

func assertNoPartials(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".partial") {
			t.Fatalf("leftover partial file: %s", path)
		}
		return nil
	}))
}

func assertBareRepoBytesEqual(t *testing.T, leftArchive, rightArchive string) {
	t.Helper()
	leftRoot := extractArchive(t, leftArchive)
	rightRoot := extractArchive(t, rightArchive)
	leftBare, err := workdir.FindBareRepo(leftRoot)
	require.NoError(t, err)
	rightBare, err := workdir.FindBareRepo(rightRoot)
	require.NoError(t, err)
	assert.Equal(t, regularFileBytes(t, leftBare), regularFileBytes(t, rightBare))
}

func metadataFileSet(t *testing.T, archivePath string) []string {
	t.Helper()
	root := extractArchive(t, archivePath)
	var files []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() == ".complete" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	}))
	sort.Strings(files)
	return files
}

func regularFileBytes(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// Compare the actual bare Git repository state, not GEI/filter-repo
		// scratch files whose audit/cache contents can vary between runs.
		if isVolatileBareRepoSidecar(rel) {
			return nil
		}
		files[rel] = mustReadFile(t, path)
		return nil
	}))
	return files
}

func isVolatileBareRepoSidecar(rel string) bool {
	if strings.HasPrefix(rel, "filter-repo/") {
		return true
	}
	switch rel {
	case "audit_log", "dgit-state", "dgit-state.flock", "nw-sync.lock", "language-stats.cache", "stacks-stats.cache":
		return true
	default:
		return false
	}
}

func extractArchive(t *testing.T, archivePath string) string {
	t.Helper()
	dest := t.TempDir()
	_, err := commitarchive.UnTar(archivePath, dest)
	require.NoError(t, err)
	return dest
}

func requireFilterRepo(t *testing.T) {
	t.Helper()
	if !hasFilterRepo() {
		t.Skip("git filter-repo is not available on PATH; skipping e2e rewrite integration")
	}
}

func hasFilterRepo() bool {
	cmd := exec.Command("git", "filter-repo", "--version")
	return cmd.Run() == nil
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

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "could not find repo root from %s", dir)
		dir = parent
	}
}

func withPATEnv(t *testing.T, source, target string) {
	t.Helper()
	t.Setenv("GH_SOURCE_PAT", source)
	t.Setenv("GH_PAT", target)
}

func argValue(args []string, flag string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1], true
		}
	}
	return "", false
}

func argsContain(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		if equalStrings(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
