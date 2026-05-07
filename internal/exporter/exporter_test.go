package exporter

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	commitarchive "github.com/mona-actions/gh-commit-remap/pkg/archive"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/api"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAPI struct {
	pollSequence []string
	archiveBytes [][]byte
	startErr     error
	downloadErr  error

	startCalled    int32
	pollCalled     int32
	downloadCalled int32
	deleteCalled   int32
	opts           []api.MigrationOpts
}

func (f *fakeAPI) StartOrgMigration(_ context.Context, _ string, opts api.MigrationOpts) (int64, error) {
	n := atomic.AddInt32(&f.startCalled, 1)
	f.opts = append(f.opts, opts)
	if f.startErr != nil {
		return 0, f.startErr
	}
	return int64(n), nil
}

func (f *fakeAPI) GetMigration(_ context.Context, _ string, _ int64) (*api.Migration, error) {
	n := atomic.AddInt32(&f.pollCalled, 1)
	idx := int(n) - 1
	if idx >= len(f.pollSequence) {
		idx = len(f.pollSequence) - 1
	}
	return &api.Migration{ID: 42, State: f.pollSequence[idx]}, nil
}

func (f *fakeAPI) DownloadArchive(_ context.Context, _ string, id int64, dest string) error {
	atomic.AddInt32(&f.downloadCalled, 1)
	if f.downloadErr != nil {
		return f.downloadErr
	}
	idx := int(id) - 1
	if idx >= len(f.archiveBytes) {
		idx = len(f.archiveBytes) - 1
	}
	return os.WriteFile(dest, f.archiveBytes[idx], 0o644)
}

func (f *fakeAPI) DeleteMigrationArchive(_ context.Context, _ string, _ int64) error {
	atomic.AddInt32(&f.deleteCalled, 1)
	return nil
}

func newTestExporter(t *testing.T, fake *fakeAPI, cfg Config) (*Exporter, *workdir.WorkDir) {
	t.Helper()
	wd, err := workdir.New(t.TempDir())
	require.NoError(t, err)
	if cfg.Org == "" {
		cfg.Org = "acme"
	}
	if cfg.Repo == "" {
		cfg.Repo = "widget"
	}
	return &Exporter{
		api:         fake,
		wd:          wd,
		cfg:         cfg,
		pollInitial: time.Millisecond,
		pollMax:     2 * time.Millisecond,
		now:         time.Now,
	}, wd
}

func TestParseMode(t *testing.T) {
	for input, want := range map[string]Mode{"": ModeTwo, "two": ModeTwo, " combined ": ModeCombined, "COMBINED": ModeCombined} {
		got, err := ParseMode(input)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	_, err := ParseMode("legacy")
	require.Error(t, err)
}

func TestExporterLoadOrInitMode(t *testing.T) {
	exp, wd := newTestExporter(t, &fakeAPI{}, Config{})
	mode, err := exp.loadOrInitMode(ModeCombined)
	require.NoError(t, err)
	assert.Equal(t, ModeCombined, mode)
	data, err := os.ReadFile(wd.ExportModeFile())
	require.NoError(t, err)
	assert.Equal(t, "combined\n", string(data))

	mode, err = exp.loadOrInitMode(ModeCombined)
	require.NoError(t, err)
	assert.Equal(t, ModeCombined, mode)

	_, err = exp.loadOrInitMode(ModeTwo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "started in combined mode")
	assert.Contains(t, err.Error(), "--export-mode combined")
}

func TestExporterRunTwoModeHappyPath(t *testing.T) {
	gitArchive := buildTarGz(t, map[string][]byte{"repositories/Acme/widget.git/HEAD": []byte("ref: refs/heads/main\n")})
	metaArchive := buildTarGz(t, map[string][]byte{"issues_000001.json": []byte("[]"), "schema.json": []byte("{}")})
	fake := &fakeAPI{pollSequence: []string{"exported"}, archiveBytes: [][]byte{gitArchive, metaArchive}}
	exp, wd := newTestExporter(t, fake, Config{Mode: ModeTwo, LockRepositories: true, ExcludeReleases: true, ExcludeAttachments: true})

	require.NoError(t, exp.Run(context.Background()))

	assert.Equal(t, int32(2), atomic.LoadInt32(&fake.startCalled))
	assert.Equal(t, int32(2), atomic.LoadInt32(&fake.downloadCalled))
	assert.Equal(t, int32(2), atomic.LoadInt32(&fake.deleteCalled))
	require.Len(t, fake.opts, 2)
	assert.True(t, fake.opts[0].ExcludeMetadata)
	assert.False(t, fake.opts[0].ExcludeGitData)
	assert.True(t, fake.opts[1].ExcludeGitData)
	assert.True(t, fake.opts[1].ExcludeOwnerProjects)
	assert.True(t, fake.opts[1].ExcludeReleases)
	assert.True(t, fake.opts[1].ExcludeAttachments)
	assert.True(t, fake.opts[1].LockRepositories)
	assert.FileExists(t, wd.RawGitArchive())
	assert.FileExists(t, wd.RawMetadataArchive())
}

func TestExporterRunTwoModeHTTPPayloadsAndCleanup(t *testing.T) {
	archiveBytes := buildTarGz(t, map[string][]byte{"schema.json": []byte("{}")})
	var posts []map[string]any
	deleted := map[string]bool{}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/orgs/acme/migrations":
			require.Equal(t, http.MethodPost, r.Method)
			var payload map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			posts = append(posts, payload)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d,"state":"pending"}`, len(posts))
		case strings.HasPrefix(r.URL.Path, "/orgs/acme/migrations/"):
			rel := strings.TrimPrefix(r.URL.Path, "/orgs/acme/migrations/")
			parts := strings.Split(strings.Trim(rel, "/"), "/")
			require.NotEmpty(t, parts)
			id := parts[0]
			switch {
			case r.Method == http.MethodGet && len(parts) == 1:
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":%s,"state":"exported"}`, id)
			case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "archive":
				http.Redirect(w, r, srv.URL+"/download/"+id+".tar.gz", http.StatusFound)
			case r.Method == http.MethodDelete && len(parts) == 2 && parts[1] == "archive":
				deleted[id] = true
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		case strings.HasPrefix(r.URL.Path, "/download/"):
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archiveBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a, err := api.NewForTesting(srv.URL)
	require.NoError(t, err)
	wd, err := workdir.New(t.TempDir())
	require.NoError(t, err)
	exp := &Exporter{api: a, wd: wd, cfg: Config{Org: "acme", Repo: "widget", Mode: ModeTwo}, pollInitial: time.Millisecond, pollMax: 2 * time.Millisecond, now: time.Now}

	require.NoError(t, exp.Run(context.Background()))

	require.Len(t, posts, 2)
	assert.Equal(t, true, posts[0]["exclude_metadata"])
	assert.NotContains(t, posts[0], "exclude_git_data")
	assert.Equal(t, true, posts[1]["exclude_git_data"])
	assert.NotContains(t, posts[1], "exclude_metadata")
	assert.FileExists(t, wd.RawGitArchive())
	assert.FileExists(t, wd.RawMetadataArchive())
	assert.True(t, deleted["1"], "expected first migration archive to be deleted")
	assert.True(t, deleted["2"], "expected second migration archive to be deleted")
}

func TestExporterRunSkipsCompleteArchives(t *testing.T) {
	archive := buildTarGz(t, map[string][]byte{"schema.json": []byte("{}")})
	fake := &fakeAPI{pollSequence: []string{"exported"}, archiveBytes: [][]byte{archive}}
	exp, wd := newTestExporter(t, fake, Config{Mode: ModeTwo})
	require.NoError(t, os.WriteFile(wd.RawGitArchive(), archive, 0o644))
	require.NoError(t, os.WriteFile(wd.RawMetadataArchive(), archive, 0o644))

	require.NoError(t, exp.Run(context.Background()))
	assert.Equal(t, int32(0), atomic.LoadInt32(&fake.startCalled))
}

func TestExporterRunCombinedModeSplitsFixtureArchive(t *testing.T) {
	combinedArchive := buildCombinedFixtureArchive(t)
	fake := &fakeAPI{pollSequence: []string{"exported"}, archiveBytes: [][]byte{combinedArchive}}
	exp, wd := newTestExporter(t, fake, Config{Mode: ModeCombined})

	require.NoError(t, exp.Run(context.Background()))

	assert.Equal(t, int32(1), atomic.LoadInt32(&fake.startCalled))
	require.Len(t, fake.opts, 1)
	assert.False(t, fake.opts[0].ExcludeGitData)
	assert.False(t, fake.opts[0].ExcludeMetadata)
	assert.NoFileExists(t, wd.RawGitArchive())
	assert.NoFileExists(t, wd.RawMetadataArchive())
	require.FileExists(t, filepath.Join(wd.GitExtractedDir(), ".complete"))
	require.FileExists(t, filepath.Join(wd.MetadataExtractedDir(), ".complete"))

	gitRoot := wd.GitExtractedDir()
	metaRoot := wd.MetadataExtractedDir()
	_, err := workdir.FindBareRepo(gitRoot)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(metaRoot, "issues_000001.json"))
	assert.NoDirExists(t, filepath.Join(metaRoot, "repositories"))
	for _, rel := range []string{"organizations_000001.json", "repositories_000001.json", "schema.json"} {
		assert.FileExists(t, filepath.Join(gitRoot, rel))
		assert.FileExists(t, filepath.Join(metaRoot, rel))
	}
}

func TestExporterRunPersistsAndResumesMode(t *testing.T) {
	gitArchive := buildTarGz(t, map[string][]byte{"repositories/Acme/widget.git/HEAD": []byte("ref: refs/heads/main\n")})
	metaArchive := buildTarGz(t, map[string][]byte{"issues_000001.json": []byte("[]"), "schema.json": []byte("{}")})
	wd, err := workdir.New(t.TempDir())
	require.NoError(t, err)

	firstFake := &fakeAPI{pollSequence: []string{"exported"}, archiveBytes: [][]byte{gitArchive, metaArchive}}
	first := newTestExporterWithWorkDir(wd, firstFake, Config{Org: "acme", Repo: "widget", Mode: ModeTwo})
	require.NoError(t, first.Run(context.Background()))
	modeBytes, err := os.ReadFile(wd.ExportModeFile())
	require.NoError(t, err)
	assert.Equal(t, "two\n", string(modeBytes))

	secondFake := &fakeAPI{pollSequence: []string{"exported"}, archiveBytes: [][]byte{gitArchive, metaArchive}}
	second := newTestExporterWithWorkDir(wd, secondFake, Config{Org: "acme", Repo: "widget", Mode: ModeTwo})
	require.NoError(t, second.Run(context.Background()))
	assert.Equal(t, int32(0), atomic.LoadInt32(&secondFake.startCalled))
}

func TestExporterRunModeMismatchErrorsBeforeHTTP(t *testing.T) {
	wd, err := workdir.New(t.TempDir())
	require.NoError(t, err)
	combined := newTestExporterWithWorkDir(wd, &fakeAPI{}, Config{Org: "acme", Repo: "widget", Mode: ModeCombined})
	_, err = combined.loadOrInitMode(ModeCombined)
	require.NoError(t, err)

	fake := &fakeAPI{}
	two := newTestExporterWithWorkDir(wd, fake, Config{Org: "acme", Repo: "widget", Mode: ModeTwo})
	err = two.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "combined")
	assert.Contains(t, err.Error(), "two")
	assert.Contains(t, err.Error(), "fresh --work-dir")
	assert.Equal(t, int32(0), atomic.LoadInt32(&fake.startCalled))
}

func TestExporterRunFailedStateKeepsRemote(t *testing.T) {
	fake := &fakeAPI{pollSequence: []string{"failed"}}
	exp, _ := newTestExporter(t, fake, Config{})
	err := exp.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed state")
	assert.Equal(t, int32(0), atomic.LoadInt32(&fake.deleteCalled))
}

func TestExporterRunMigrationDownloadErrorKeepsRemote(t *testing.T) {
	fake := &fakeAPI{pollSequence: []string{"exported"}, downloadErr: errors.New("boom")}
	exp, wd := newTestExporter(t, fake, Config{})
	err := exp.runMigration(context.Background(), api.MigrationOpts{Repositories: []string{"widget"}}, wd.RawGitArchive(), "git archive")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to download git archive")
	assert.Equal(t, int32(0), atomic.LoadInt32(&fake.deleteCalled))
}

func TestExporterRunMigrationValidationErrorKeepsRemote(t *testing.T) {
	fake := &fakeAPI{pollSequence: []string{"exported"}, archiveBytes: [][]byte{[]byte("not a tarball")}}
	exp, wd := newTestExporter(t, fake, Config{})
	err := exp.runMigration(context.Background(), api.MigrationOpts{Repositories: []string{"widget"}}, wd.RawGitArchive(), "git archive")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "downloaded archive failed validation")
	assert.Equal(t, int32(0), atomic.LoadInt32(&fake.deleteCalled))
	assert.NoFileExists(t, wd.RawGitArchive())
}

func TestExporterRunContextCancelDoesNotCleanRemote(t *testing.T) {
	fake := &fakeAPI{pollSequence: []string{"pending"}}
	exp, _ := newTestExporter(t, fake, Config{})
	exp.pollInitial = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	err := exp.Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(0), atomic.LoadInt32(&fake.deleteCalled))
}

func TestExporterRunStartError(t *testing.T) {
	fake := &fakeAPI{startErr: errors.New("boom"), pollSequence: []string{"exported"}}
	exp, _ := newTestExporter(t, fake, Config{})
	err := exp.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestSplitTreeStandardLayout(t *testing.T) {
	extractRoot := t.TempDir()
	writeFile(t, extractRoot, "organizations_000001.json", "org")
	writeFile(t, extractRoot, "repositories_000001.json", "repo-meta")
	writeFile(t, extractRoot, "schema.json", "schema")
	writeFile(t, extractRoot, "issues_000001.json", "issues")
	writeFile(t, extractRoot, "users_000001.json", "users")
	writeFile(t, extractRoot, "repositories/Acme/foo.git/HEAD", "ref: refs/heads/main\n")

	gitStage := filepath.Join(t.TempDir(), "git")
	metaStage := filepath.Join(t.TempDir(), "meta")
	require.NoError(t, splitTree(extractRoot, filepath.Join(extractRoot, "repositories/Acme/foo.git"), gitStage, metaStage))

	assert.FileExists(t, filepath.Join(gitStage, "repositories/Acme/foo.git/HEAD"))
	assert.NoDirExists(t, filepath.Join(metaStage, "repositories"))
	assert.FileExists(t, filepath.Join(metaStage, "issues_000001.json"))
	assert.FileExists(t, filepath.Join(metaStage, "users_000001.json"))
	for _, rel := range []string{"organizations_000001.json", "repositories_000001.json", "schema.json"} {
		assert.FileExists(t, filepath.Join(gitStage, rel))
		assert.FileExists(t, filepath.Join(metaStage, rel))
		assertSharedEqual(t, gitStage, metaStage, rel)
	}
}

func TestSplitTreeMovesAttachmentLFSAndReleaseSiblingsToMetadata(t *testing.T) {
	extractRoot := t.TempDir()
	writeFile(t, extractRoot, "schema.json", "schema")
	writeFile(t, extractRoot, "attachments/uuid/file.txt", "attachment")
	writeFile(t, extractRoot, "git-lfs/objects/x", "lfs")
	writeFile(t, extractRoot, "releases/asset.bin", "release")
	writeFile(t, extractRoot, "repositories/Acme/foo.git/HEAD", "ref: refs/heads/main\n")

	gitStage := filepath.Join(t.TempDir(), "git")
	metaStage := filepath.Join(t.TempDir(), "meta")
	require.NoError(t, splitTree(extractRoot, filepath.Join(extractRoot, "repositories/Acme/foo.git"), gitStage, metaStage))

	assert.FileExists(t, filepath.Join(metaStage, "attachments/uuid/file.txt"))
	assert.FileExists(t, filepath.Join(metaStage, "git-lfs/objects/x"))
	assert.FileExists(t, filepath.Join(metaStage, "releases/asset.bin"))
	assert.NoDirExists(t, filepath.Join(gitStage, "attachments"))
	assert.NoDirExists(t, filepath.Join(gitStage, "git-lfs"))
	assert.NoDirExists(t, filepath.Join(gitStage, "releases"))
}

func TestSplitTreeSharedFilesDuplicationByteEquality(t *testing.T) {
	extractRoot := t.TempDir()
	writeFile(t, extractRoot, "organizations_000001.json", `{"login":"acme","id":1}`)
	writeFile(t, extractRoot, "repositories_000001.json", "repo-meta")
	writeFile(t, extractRoot, "schema.json", "schema")
	writeFile(t, extractRoot, "repositories/Acme/foo.git/HEAD", "ref: refs/heads/main\n")
	origHash := sha256.Sum256(mustReadFile(t, filepath.Join(extractRoot, "organizations_000001.json")))

	gitStage := filepath.Join(t.TempDir(), "git")
	metaStage := filepath.Join(t.TempDir(), "meta")
	require.NoError(t, splitTree(extractRoot, filepath.Join(extractRoot, "repositories/Acme/foo.git"), gitStage, metaStage))

	gitHash := sha256.Sum256(mustReadFile(t, filepath.Join(gitStage, "organizations_000001.json")))
	metaHash := sha256.Sum256(mustReadFile(t, filepath.Join(metaStage, "organizations_000001.json")))
	assert.Equal(t, origHash, gitHash)
	assert.Equal(t, origHash, metaHash)
}

func TestSplitTreeErrorsWhenRepositoriesHasNoBareRepo(t *testing.T) {
	extractRoot := t.TempDir()
	writeFile(t, extractRoot, "repositories/Acme/not-bare/HEAD", "ref: refs/heads/main\n")
	_, err := workdir.FindBareRepo(extractRoot)
	require.ErrorIs(t, err, workdir.ErrNoBareRepo)
}

func TestSplitTreeErrorsWhenRepositoriesHasMultipleBareRepos(t *testing.T) {
	extractRoot := t.TempDir()
	writeFile(t, extractRoot, "repositories/Acme/one.git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, extractRoot, "repositories/Acme/two.git/HEAD", "ref: refs/heads/main\n")
	_, err := workdir.FindBareRepo(extractRoot)
	require.ErrorIs(t, err, workdir.ErrMultipleBareRepos)
}

func TestSplitterDuplicatesSharedRootJSONs(t *testing.T) {
	extractRoot := t.TempDir()
	shared := map[string]string{
		"organizations_000001.json": `{"org":"acme"}`,
		"repositories_000001.json":  `[{"name":"foo"}]`,
		"schema.json":               `{"version":1}`,
	}
	for rel, content := range shared {
		writeFile(t, extractRoot, rel, content)
	}
	writeFile(t, extractRoot, "issues_000001.json", "[]")
	writeFile(t, extractRoot, "repositories/Acme/foo.git/HEAD", "ref: refs/heads/main\n")

	gitStage := filepath.Join(t.TempDir(), "git")
	metaStage := filepath.Join(t.TempDir(), "meta")
	require.NoError(t, splitTree(extractRoot, filepath.Join(extractRoot, "repositories/Acme/foo.git"), gitStage, metaStage))

	gitArchive := filepath.Join(t.TempDir(), "git.tar.gz")
	metaArchive := filepath.Join(t.TempDir(), "meta.tar.gz")
	require.NoError(t, commitarchive.ReTarDir(gitStage, gitArchive))
	require.NoError(t, commitarchive.ReTarDir(metaStage, metaArchive))
	gitRoot := extractArchive(t, gitArchive)
	metaRoot := extractArchive(t, metaArchive)

	for rel := range shared {
		gitBytes := mustReadFile(t, filepath.Join(gitRoot, rel))
		metaBytes := mustReadFile(t, filepath.Join(metaRoot, rel))
		assert.Equal(t, gitBytes, metaBytes, rel)
	}
}

func TestSplitTreeBareRepoMustBeUnderRepositories(t *testing.T) {
	extractRoot := t.TempDir()
	writeFile(t, extractRoot, "foo.git/HEAD", "ref: refs/heads/main\n")
	err := splitTree(extractRoot, filepath.Join(extractRoot, "foo.git"), filepath.Join(t.TempDir(), "git"), filepath.Join(t.TempDir(), "meta"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not under repositories")
}

func TestIsCrossDeviceDetectsEXDEV(t *testing.T) {
	assert.True(t, isCrossDevice(syscall.EXDEV))
	assert.True(t, isCrossDevice(&os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EXDEV}))
	assert.False(t, isCrossDevice(errors.New("other")))
}

func newTestExporterWithWorkDir(wd *workdir.WorkDir, fake *fakeAPI, cfg Config) *Exporter {
	return &Exporter{
		api:         fake,
		wd:          wd,
		cfg:         cfg,
		pollInitial: time.Millisecond,
		pollMax:     2 * time.Millisecond,
		now:         time.Now,
	}
}

func buildCombinedFixtureArchive(t *testing.T) []byte {
	t.Helper()
	root := t.TempDir()
	repoRoot := findRepoRoot(t)
	_, err := commitarchive.UnTar(filepath.Join(repoRoot, "internal/testdata/gei-real/git-archive.tar.gz"), root)
	require.NoError(t, err)
	_, err = commitarchive.UnTar(filepath.Join(repoRoot, "internal/testdata/gei-real/metadata-archive.tar.gz"), root)
	require.NoError(t, err)
	out := filepath.Join(t.TempDir(), "combined.tar.gz")
	require.NoError(t, commitarchive.ReTarDir(root, out))
	return mustReadFile(t, out)
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

func extractArchive(t *testing.T, archivePath string) string {
	t.Helper()
	dest := t.TempDir()
	_, err := commitarchive.UnTar(archivePath, dest)
	require.NoError(t, err)
	return dest
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func buildTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for path, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(content)), ModTime: time.Now()}))
		_, err := tw.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func assertSharedEqual(t *testing.T, gitStage, metaStage, rel string) {
	t.Helper()
	gitBytes, err := os.ReadFile(filepath.Join(gitStage, rel))
	require.NoError(t, err)
	metaBytes, err := os.ReadFile(filepath.Join(metaStage, rel))
	require.NoError(t, err)
	assert.Equal(t, gitBytes, metaBytes)
}
