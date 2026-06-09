package rewriter

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/filterrepo"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/largefiles"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// stubRunner records the single combined call and emulates filter-repo
// writing one commit-map.
type stubRunner struct {
	combinedCalls []filterrepo.CombinedOpts
	writeCount    int

	combinedErr error
}

func (s *stubRunner) RunCombined(_ context.Context, bare string, opts filterrepo.CombinedOpts) error {
	s.combinedCalls = append(s.combinedCalls, opts)
	if s.combinedErr != nil {
		return s.combinedErr
	}
	return s.writeCommitMap(bare)
}
func (s *stubRunner) writeCommitMap(bare string) error {
	s.writeCount++
	dst := filepath.Join(bare, "filter-repo", "commit-map")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte("old new\nabc 123\n"), 0o644)
}

type stubAnalyzer struct {
	report *largefiles.Report
	err    error
	called bool
}

func (s *stubAnalyzer) Analyze(_ context.Context, _ string) (*largefiles.Report, error) {
	s.called = true
	return s.report, s.err
}

func makeRewriter(wd *workdir.WorkDir, runner runnerIface, analyzer analyzerIface, cfg Config, isTTY bool, confirmAnswer bool) *Rewriter {
	r := newWithDeps(wd, runner, analyzer, nil, cfg)
	r.isTTY = func() bool { return isTTY }
	r.confirm = func(string, bool) (bool, error) { return confirmAnswer, nil }
	return r
}

func TestRun_ModeUnawareProducesArchiveAndCommitMap(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoFlags: []string{"--refs", "main"}}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.FileExists(t, wd.GitArchive())
	assert.FileExists(t, wd.CommitMap())
	assert.Equal(t, 1, len(runner.combinedCalls))
	assert.Equal(t, 1, runner.writeCount)
	assert.Equal(t, 1, res.CommitsRemapped)
	assert.DirExists(t, filepath.Join(wd.GitExtractedDir(), "repositories", "Acme", "foo.git"))
}

func TestRun_IdempotentFinalArchiveSkipsExtractAndFilterRepo(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoFlags: []string{"--refs", "main"}}, false, false)

	_, err := r.Run(context.Background())
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(wd.GitExtractedDir()))

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 1, len(runner.combinedCalls))
	assert.Equal(t, 1, runner.writeCount)
	assert.NoDirExists(t, wd.GitExtractedDir())
}

func TestRun_MultipleBareReposRejected(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git", "bar.git")
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoFlags: []string{"--refs", "main"}}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-repo migrations are not supported")
	assert.Empty(t, runner.combinedCalls)
}

func TestRun_NoBareRepoRejected(t *testing.T) {
	wd := newArchiveWorkDir(t)
	r := makeRewriter(wd, &stubRunner{}, &stubAnalyzer{}, Config{}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no .git directory found")
}

func TestRun_StripHappyPath(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	analyzer := &stubAnalyzer{report: &largefiles.Report{
		Threshold: 1024,
		Flagged: []largefiles.FlaggedPath{
			{Path: "big1.bin", MaxDeletedUnpackedBytes: 5_000_000, CumulativeBytes: 5_000_000, Reason: largefiles.ReasonBoth},
			{Path: "big2.bin", MaxDeletedUnpackedBytes: 2_000_000, CumulativeBytes: 1_500_000, Reason: largefiles.ReasonSingleBlob},
		},
	}}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true, SkipConfirm: true}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.StripPerformed)
	assert.Equal(t, []string{"big1.bin", "big2.bin"}, res.PathsStripped)
	assert.EqualValues(t, 5_000_000, res.LargestStripped)
	assert.EqualValues(t, 7_000_000, res.BytesFreed)
	require.Len(t, runner.combinedCalls, 1)
	assert.Equal(t, wd.CleanupTxt(), runner.combinedCalls[0].PathsFromFile)
	assert.True(t, runner.combinedCalls[0].StripActive)

	body, err := os.ReadFile(wd.CleanupTxt())
	require.NoError(t, err)
	assert.Equal(t, "big1.bin\nbig2.bin\n", string(body))
	assert.FileExists(t, wd.GitArchive())
	assert.FileExists(t, wd.CommitMap())
	assert.Equal(t, 1, res.CommitsRemapped)
}

func TestRun_StripZeroFlagged_NoStripStillArchives(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	analyzer := &stubAnalyzer{report: &largefiles.Report{Threshold: 1024}}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true, SkipConfirm: true}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.StripPerformed)
	assert.Empty(t, runner.combinedCalls)
	assert.FileExists(t, wd.GitArchive())
	// No rewrite happened, so there is no commit-map; the run must not emit
	// the "remap will be unable to translate SHAs" warning.
	for _, w := range res.Warnings {
		assert.NotContains(t, w, "no commit-map produced")
	}
	assert.Equal(t, 0, res.CommitsRemapped)
}

func TestRun_NonTTYWithoutYes_Errors(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	analyzer := &stubAnalyzer{report: &largefiles.Report{
		Threshold: 1024,
		Flagged:   []largefiles.FlaggedPath{{Path: "big.bin", MaxDeletedUnpackedBytes: 9999, CumulativeBytes: 9999, Reason: largefiles.ReasonBoth}},
	}}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TTY")
	assert.Empty(t, runner.combinedCalls)
}

func TestRun_NonInteractiveWithoutYes_Errors(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	analyzer := &stubAnalyzer{report: &largefiles.Report{
		Threshold: 1024,
		Flagged:   []largefiles.FlaggedPath{{Path: "big.bin", MaxDeletedUnpackedBytes: 9999, CumulativeBytes: 9999, Reason: largefiles.ReasonBoth}},
	}}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true, NonInteractive: true}, true, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-interactive")
	assert.Empty(t, runner.combinedCalls)
}

func TestRun_UserScriptsOnly(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	scriptPath := filepath.Join(t.TempDir(), "rewrite.commit-callback.py")
	require.NoError(t, os.WriteFile(scriptPath, []byte("# noop"), 0o644))
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoScripts: []string{scriptPath}}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, []string{"rewrite.commit-callback.py"}, res.ScriptsRun)
	require.Len(t, runner.combinedCalls, 1)
	assert.Equal(t, []string{scriptPath}, runner.combinedCalls[0].ScriptPaths)
	assert.Empty(t, runner.combinedCalls[0].PathsFromFile)
	assert.Empty(t, runner.combinedCalls[0].PassthroughFlags)
}

func TestRun_UserFlagsOnly(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoFlags: []string{"--refs", "main"}}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, []string{"--refs", "main"}, res.UserFlagsApplied)
	require.Len(t, runner.combinedCalls, 1)
	assert.Equal(t, []string{"--refs", "main"}, runner.combinedCalls[0].PassthroughFlags)
	assert.Empty(t, runner.combinedCalls[0].PathsFromFile)
	assert.Empty(t, runner.combinedCalls[0].ScriptPaths)
}

func TestRun_StripScriptsAndFlags_SingleInvocation(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	scriptPath := filepath.Join(t.TempDir(), "x.commit-callback.py")
	require.NoError(t, os.WriteFile(scriptPath, []byte("# noop"), 0o644))
	analyzer := &stubAnalyzer{report: &largefiles.Report{
		Threshold: 1024,
		Flagged:   []largefiles.FlaggedPath{{Path: "big.bin", MaxDeletedUnpackedBytes: 9999, CumulativeBytes: 9999, Reason: largefiles.ReasonBoth}},
	}}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{
		StripLargeFiles:   true,
		SkipConfirm:       true,
		FilterRepoScripts: []string{scriptPath},
		FilterRepoFlags:   []string{"--refs", "main"},
	}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)

	// Strip + callbacks + flags must collapse into exactly ONE filter-repo
	// invocation, producing exactly one commit-map.
	require.Len(t, runner.combinedCalls, 1)
	assert.Equal(t, 1, runner.writeCount)
	opts := runner.combinedCalls[0]
	assert.True(t, opts.StripActive)
	assert.Equal(t, wd.CleanupTxt(), opts.PathsFromFile)
	assert.Equal(t, []string{scriptPath}, opts.ScriptPaths)
	assert.Equal(t, []string{"--refs", "main"}, opts.PassthroughFlags)

	assert.True(t, res.StripPerformed)
	assert.NotEmpty(t, res.ScriptsRun)
	assert.NotEmpty(t, res.UserFlagsApplied)
	assert.FileExists(t, wd.CommitMap())
	assert.Equal(t, 1, res.CommitsRemapped)
}

func TestRun_StripWithPathSelectionFlag_Rejected(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{
		StripLargeFiles: true,
		SkipConfirm:     true,
		FilterRepoFlags: []string{"--invert-paths"},
	}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--invert-paths")
	assert.Empty(t, runner.combinedCalls)
}

func TestRun_DuplicateCallbackKind_Rejected(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	dir := t.TempDir()
	a := filepath.Join(dir, "a.commit-callback.py")
	b := filepath.Join(dir, "b.commit-callback.py")
	require.NoError(t, os.WriteFile(a, []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(b, []byte("y"), 0o644))
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoScripts: []string{a, b}}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}

func TestRun_DuplicateCallbackKindAcrossScriptAndFlag_Rejected(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	script := filepath.Join(t.TempDir(), "a.commit-callback.py")
	require.NoError(t, os.WriteFile(script, []byte("x"), 0o644))
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{
		FilterRepoScripts: []string{script},
		FilterRepoFlags:   []string{"--commit-callback=return commit"},
	}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
	assert.Empty(t, runner.combinedCalls)
}

func TestRun_MissingCallbackScript_Rejected(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	missing := filepath.Join(t.TempDir(), "gone.commit-callback.py")
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoScripts: []string{missing}}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Empty(t, runner.combinedCalls)
}

func TestRun_UnknownCallbackKind_Rejected(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	bad := filepath.Join(t.TempDir(), "weird.py")
	require.NoError(t, os.WriteFile(bad, []byte("x"), 0o644))
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoScripts: []string{bad}}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown callback kind")
}

func TestHandoffCommitMap_CopiesContentAtomically(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	src := filepath.Join(wd.GitExtractedDir(), "repositories", "Acme", "foo.git", "filter-repo", "commit-map")
	require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
	want := "old new\nabc 123\ndef 456\n"
	require.NoError(t, os.WriteFile(src, []byte(want), 0o644))

	r := newWithDeps(wd, &stubRunner{}, &stubAnalyzer{}, nil, Config{})
	warn := r.handoffCommitMap(src)
	assert.Empty(t, warn)
	got, err := os.ReadFile(wd.CommitMap())
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}

func TestHandoffCommitMap_MissingSource_Warns(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	r := newWithDeps(wd, &stubRunner{}, &stubAnalyzer{}, nil, Config{})
	warn := r.handoffCommitMap(filepath.Join(wd.GitExtractedDir(), "repositories", "Acme", "foo.git", "filter-repo", "commit-map"))
	assert.Contains(t, warn, "no commit-map produced")
	_, err := os.Stat(wd.CommitMap())
	assert.True(t, os.IsNotExist(err))
}

func TestSanitizeUserFlags_RedactsCallbackBodies(t *testing.T) {
	in := []string{"--commit-callback=secret-body", "--refs", "main", "--message-callback=other-body", "--no-value-flag"}
	got := sanitizeUserFlags(in)
	assert.Equal(t, []string{"--commit-callback=<redacted>", "--refs", "main", "--message-callback=<redacted>", "--no-value-flag"}, got)
}

func TestSanitizeUserFlags_RedactsTwoTokenCallbackBody(t *testing.T) {
	in := []string{"--commit-callback", "return commit", "--refs", "main"}
	got := sanitizeUserFlags(in)
	assert.Equal(t, []string{"--commit-callback", "<redacted>", "--refs", "main"}, got)
}

func TestResultRender_PrintsKeyFields(t *testing.T) {
	res := &Result{
		StripPerformed:   true,
		PathsStripped:    []string{"big1.bin", "big2.bin"},
		LargestStripped:  5_000_000,
		BytesFreed:       7_000_000,
		ScriptsRun:       []string{"a.commit-callback.py"},
		UserFlagsApplied: []string{"--refs", "main"},
		CommitsRemapped:  42,
		Warnings:         []string{"watch out!"},
	}

	var buf bytes.Buffer
	printer := func(headers []string, rows [][]string) {
		for _, h := range headers {
			fmt.Fprintf(&buf, "[%s]", h)
		}
		buf.WriteByte('\n')
		for _, row := range rows {
			fmt.Fprintln(&buf, strings.Join(row, "|"))
		}
	}
	var warns []string
	warnFn := func(m string) { warns = append(warns, m) }

	res.Render(printer, warnFn)
	out := buf.String()
	assert.Contains(t, out, "big1.bin")
	assert.Contains(t, out, "big2.bin")
	assert.Contains(t, out, "4.8 MiB")
	assert.Contains(t, out, "6.7 MiB")
	assert.Contains(t, out, "42")
	assert.Contains(t, out, "a.commit-callback.py")
	assert.Contains(t, out, "--refs")
	assert.Equal(t, []string{"watch out!"}, warns)
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 1 << 20: "1.0 MiB", 1<<30 + 100: "1.0 GiB"}
	for n, want := range cases {
		assert.Equal(t, want, HumanBytes(n), "n=%d", n)
	}
}

func newArchiveWorkDir(t *testing.T, repoNames ...string) *workdir.WorkDir {
	t.Helper()
	wd, err := workdir.New(t.TempDir())
	require.NoError(t, err)
	srcRoot := t.TempDir()
	for _, name := range repoNames {
		createBareRepoWithCommit(t, filepath.Join(srcRoot, "repositories", "Acme", name))
	}
	writeTarGz(t, srcRoot, wd.RawGitArchive())
	return wd
}

func createBareRepoWithCommit(t *testing.T, bare string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(bare), 0o755))
	runGit(t, "", "init", "--bare", bare)
	work := filepath.Join(t.TempDir(), "work")
	runGit(t, "", "init", work)
	runGit(t, work, "config", "user.email", "t@example.test")
	runGit(t, work, "config", "user.name", "tester")
	runGit(t, work, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644))
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "seed")
	runGit(t, work, "push", bare, "HEAD:refs/heads/main")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s failed:\n%s", strings.Join(args, " "), string(out))
}

func writeTarGz(t *testing.T, srcRoot, outPath string) {
	t.Helper()
	out, err := os.Create(outPath)
	require.NoError(t, err)
	defer out.Close()
	gz := gzip.NewWriter(out)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	err = filepath.WalkDir(srcRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == srcRoot {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	require.NoError(t, err)
}
