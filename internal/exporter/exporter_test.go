package exporter

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/api"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI implements migrationsClient for tests. It simulates the org
// migrations REST API: start → poll (state machine) → download → delete.
type fakeAPI struct {
	t            *testing.T
	pollSequence []string // states returned in order; last value sticky
	archiveBytes []byte
	startErr     error
	downloadErr  error

	startCalled    int32
	pollCalled     int32
	downloadCalled int32
	deleteCalled   int32
	lastOpts       api.MigrationOpts
}

func (f *fakeAPI) StartOrgMigration(_ context.Context, _ string, opts api.MigrationOpts) (int64, error) {
	atomic.AddInt32(&f.startCalled, 1)
	f.lastOpts = opts
	if f.startErr != nil {
		return 0, f.startErr
	}
	return 42, nil
}

func (f *fakeAPI) GetMigration(_ context.Context, _ string, _ int64) (*api.Migration, error) {
	n := atomic.AddInt32(&f.pollCalled, 1)
	idx := int(n) - 1
	if idx >= len(f.pollSequence) {
		idx = len(f.pollSequence) - 1
	}
	return &api.Migration{ID: 42, State: f.pollSequence[idx]}, nil
}

func (f *fakeAPI) DownloadArchive(_ context.Context, _ string, _ int64, dest string) error {
	atomic.AddInt32(&f.downloadCalled, 1)
	if f.downloadErr != nil {
		return f.downloadErr
	}
	return os.WriteFile(dest, f.archiveBytes, 0o644)
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
	exp := &Exporter{
		api:         fake,
		wd:          wd,
		cfg:         cfg,
		pollInitial: 1 * time.Millisecond,
		pollMax:     5 * time.Millisecond,
		now:         time.Now,
	}
	return exp, wd
}

type tarEntry struct {
	isDir     bool
	content   []byte
	symlinkTo string
}

func buildTarGz(t *testing.T, entries map[string]*tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for path, e := range entries {
		hdr := &tar.Header{Name: path, Mode: 0o644, ModTime: time.Now()}
		switch {
		case e.isDir:
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		case e.symlinkTo != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.symlinkTo
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.content))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if hdr.Typeflag == tar.TypeReg {
			_, err := tw.Write(e.content)
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func TestExporter_Run_HappyPath(t *testing.T) {
	archive := buildTarGz(t, map[string]*tarEntry{
		"prefix/":                         {isDir: true},
		"prefix/Repo.git/":                {isDir: true},
		"prefix/Repo.git/HEAD":            {content: []byte("ref: refs/heads/main\n")},
		"prefix/repositories_000001.json": {content: []byte("[]")},
	})
	fake := &fakeAPI{t: t, pollSequence: []string{"pending", "exporting", "exported"}, archiveBytes: archive}
	exp, wd := newTestExporter(t, fake, Config{LockRepositories: true, ExcludeAttachments: true})

	require.NoError(t, exp.Run(context.Background()))

	assert.Equal(t, int32(1), atomic.LoadInt32(&fake.startCalled))
	assert.GreaterOrEqual(t, atomic.LoadInt32(&fake.pollCalled), int32(3))
	assert.Equal(t, int32(1), atomic.LoadInt32(&fake.downloadCalled))
	assert.Equal(t, int32(1), atomic.LoadInt32(&fake.deleteCalled))
	assert.True(t, fake.lastOpts.LockRepositories)
	assert.True(t, fake.lastOpts.ExcludeAttachments)
	assert.Equal(t, []string{"acme/widget"}, fake.lastOpts.Repositories)

	bare, err := wd.BareRepoPath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(wd.Extracted(), "prefix", "Repo.git"), bare)
}

func TestExporter_Run_IdempotencySkip(t *testing.T) {
	archive := buildTarGz(t, map[string]*tarEntry{
		"prefix/Repo.git/":     {isDir: true},
		"prefix/Repo.git/HEAD": {content: []byte("ref: refs/heads/main\n")},
	})
	fake := &fakeAPI{t: t, pollSequence: []string{"exported"}, archiveBytes: archive}
	exp, wd := newTestExporter(t, fake, Config{})

	require.NoError(t, os.WriteFile(wd.Archive(), archive, 0o644))
	require.NoError(t, Extract(wd.Archive(), wd.Extracted()))

	require.NoError(t, exp.Run(context.Background()))
	assert.Equal(t, int32(0), atomic.LoadInt32(&fake.startCalled))
	assert.Equal(t, int32(0), atomic.LoadInt32(&fake.pollCalled))
	assert.Equal(t, int32(0), atomic.LoadInt32(&fake.downloadCalled))
}

func TestExporter_Run_ReextractsCachedArchive(t *testing.T) {
	archive := buildTarGz(t, map[string]*tarEntry{
		"prefix/Repo.git/":     {isDir: true},
		"prefix/Repo.git/HEAD": {content: []byte("ref: refs/heads/main\n")},
	})
	fake := &fakeAPI{t: t, pollSequence: []string{"exported"}, archiveBytes: archive}
	exp, wd := newTestExporter(t, fake, Config{})
	require.NoError(t, os.WriteFile(wd.Archive(), archive, 0o644))

	require.NoError(t, exp.Run(context.Background()))
	assert.Equal(t, int32(0), atomic.LoadInt32(&fake.downloadCalled))
	bare, err := wd.BareRepoPath()
	require.NoError(t, err)
	assert.Contains(t, bare, "Repo.git")
}

func TestExporter_Run_MultiRepoRejected(t *testing.T) {
	archive := buildTarGz(t, map[string]*tarEntry{
		"prefix/RepoOne.git/":     {isDir: true},
		"prefix/RepoOne.git/HEAD": {content: []byte("ref: refs/heads/main\n")},
		"prefix/RepoTwo.git/":     {isDir: true},
		"prefix/RepoTwo.git/HEAD": {content: []byte("ref: refs/heads/main\n")},
	})
	fake := &fakeAPI{t: t, pollSequence: []string{"exported"}, archiveBytes: archive}
	exp, _ := newTestExporter(t, fake, Config{})

	err := exp.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple repos")
	assert.Contains(t, err.Error(), "v1 supports single-repo migrations only")
	assert.Contains(t, err.Error(), "open an issue")
}

func TestExporter_Run_FailedState(t *testing.T) {
	fake := &fakeAPI{t: t, pollSequence: []string{"pending", "failed"}}
	exp, _ := newTestExporter(t, fake, Config{})

	err := exp.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed state")
	assert.Equal(t, int32(0), atomic.LoadInt32(&fake.downloadCalled))
}

func TestExporter_Run_StartError(t *testing.T) {
	fake := &fakeAPI{t: t, startErr: errors.New("boom"), pollSequence: []string{"exported"}}
	exp, _ := newTestExporter(t, fake, Config{})
	err := exp.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestExporter_Run_DownloadErrorCleansPartial(t *testing.T) {
	fake := &fakeAPI{t: t, pollSequence: []string{"exported"}, downloadErr: errors.New("network reset")}
	exp, wd := newTestExporter(t, fake, Config{})
	err := exp.Run(context.Background())
	require.Error(t, err)
	assert.False(t, wd.HasArchive())
}

func TestExporter_Run_ContextCancel(t *testing.T) {
	fake := &fakeAPI{t: t, pollSequence: []string{"pending"}}
	exp, _ := newTestExporter(t, fake, Config{})
	exp.pollInitial = 50 * time.Millisecond
	exp.pollMax = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := exp.Run(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestExporter_Run_RequiresOrgAndRepo(t *testing.T) {
	wd, err := workdir.New(t.TempDir())
	require.NoError(t, err)
	exp := &Exporter{api: &fakeAPI{t: t}, wd: wd}
	err = exp.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "org is required")
}

func TestExtract_ZipSlipRejected(t *testing.T) {
	archive := buildTarGz(t, map[string]*tarEntry{
		"../escape.txt": {content: []byte("malicious")},
	})
	tmp := t.TempDir()
	src := filepath.Join(tmp, "evil.tar.gz")
	require.NoError(t, os.WriteFile(src, archive, 0o644))

	err := Extract(src, filepath.Join(tmp, "extracted"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zip-slip")
	_, statErr := os.Stat(filepath.Join(tmp, "escape.txt"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestExtract_SymlinkEscapeRejected(t *testing.T) {
	archive := buildTarGz(t, map[string]*tarEntry{
		"link": {symlinkTo: "../../etc/passwd"},
	})
	tmp := t.TempDir()
	src := filepath.Join(tmp, "evil.tar.gz")
	require.NoError(t, os.WriteFile(src, archive, 0o644))

	err := Extract(src, filepath.Join(tmp, "extracted"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zip-slip")
}

func TestExtract_HappyPath(t *testing.T) {
	archive := buildTarGz(t, map[string]*tarEntry{
		"a/":      {isDir: true},
		"a/b.txt": {content: []byte("hello")},
		"a/c/":    {isDir: true},
		"a/c/d":   {content: []byte("world")},
	})
	tmp := t.TempDir()
	src := filepath.Join(tmp, "ok.tar.gz")
	require.NoError(t, os.WriteFile(src, archive, 0o644))

	dest := filepath.Join(tmp, "out")
	require.NoError(t, Extract(src, dest))

	got, err := os.ReadFile(filepath.Join(dest, "a", "b.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
	got, err = os.ReadFile(filepath.Join(dest, "a", "c", "d"))
	require.NoError(t, err)
	assert.Equal(t, "world", string(got))
}

func TestBuildTarGz_RoundTrip(t *testing.T) {
	archive := buildTarGz(t, map[string]*tarEntry{"f.txt": {content: []byte("x")}})
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "f.txt", hdr.Name)
	body, err := io.ReadAll(tr)
	require.NoError(t, err)
	assert.Equal(t, "x", string(body))
}
