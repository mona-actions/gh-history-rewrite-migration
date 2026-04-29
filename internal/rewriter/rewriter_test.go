package rewriter

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/largefiles"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// stubRunner records calls and the order they happened in.
type stubRunner struct {
	stripCalls    [][2]string // [bareRepo, pathsFromFile]
	callbackCalls [][]string  // scripts per call
	runCalls      [][]string  // args per call
	order         []string

	stripErr    error
	callbackErr error
	runErr      error
}

func (s *stubRunner) StripPaths(_ context.Context, bare, paths string) error {
	s.order = append(s.order, "strip")
	s.stripCalls = append(s.stripCalls, [2]string{bare, paths})
	return s.stripErr
}
func (s *stubRunner) RunCallbackScripts(_ context.Context, _ string, scripts []string) error {
	s.order = append(s.order, "callbacks")
	s.callbackCalls = append(s.callbackCalls, append([]string(nil), scripts...))
	return s.callbackErr
}
func (s *stubRunner) Run(_ context.Context, _ string, args []string) error {
	s.order = append(s.order, "run")
	s.runCalls = append(s.runCalls, append([]string(nil), args...))
	return s.runErr
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

// newRewriterTestEnv builds a workdir with an extracted/<name>.git
// directory so wd.BareRepoPath() resolves. It returns the workdir and the
// bare repo path.
func newRewriterTestEnv(t *testing.T) (*workdir.WorkDir, string) {
	t.Helper()
	root := t.TempDir()
	wd, err := workdir.New(root)
	require.NoError(t, err)
	bare := filepath.Join(wd.Extracted(), "repo.git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	return wd, bare
}

// makeRewriter wires a Rewriter with stubbed deps and forces the TTY
// + confirm functions to deterministic values for tests.
func makeRewriter(wd *workdir.WorkDir, runner runnerIface, analyzer analyzerIface, cfg Config, isTTY bool, confirmAnswer bool) *Rewriter {
	r := newWithDeps(wd, runner, analyzer, nil, cfg)
	r.isTTY = func() bool { return isTTY }
	r.confirm = func(string, bool) (bool, error) { return confirmAnswer, nil }
	return r
}

func TestRun_Idempotent_Skips(t *testing.T) {
	wd, bare := newRewriterTestEnv(t)
	// Pre-populate both commit-map locations to trigger the skip.
	require.NoError(t, os.WriteFile(wd.CommitMap(), []byte("old new\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(bare, "filter-repo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bare, "filter-repo", "commit-map"), []byte("old new\n"), 0o644))

	runner := &stubRunner{}
	analyzer := &stubAnalyzer{}
	r := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true, SkipConfirm: true}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	assert.Nil(t, res)
	assert.False(t, analyzer.called)
	assert.Empty(t, runner.order)
}

func TestRun_StripHappyPath(t *testing.T) {
	wd, bare := newRewriterTestEnv(t)
	// filter-repo would normally produce this — pre-create so handoff succeeds.
	require.NoError(t, os.MkdirAll(filepath.Join(bare, "filter-repo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bare, "filter-repo", "commit-map"), []byte("old new\nabc 123\n"), 0o644))

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
	require.Len(t, runner.stripCalls, 1)
	assert.Equal(t, wd.CleanupTxt(), runner.stripCalls[0][1])

	// cleanup.txt should have been written with the two paths.
	body, err := os.ReadFile(wd.CleanupTxt())
	require.NoError(t, err)
	assert.Equal(t, "big1.bin\nbig2.bin\n", string(body))

	// commit-map handoff happened.
	assert.FileExists(t, wd.CommitMap())
	assert.Equal(t, 1, res.CommitsRemapped)
}

func TestRun_StripZeroFlagged_NoStrip(t *testing.T) {
	wd, _ := newRewriterTestEnv(t)
	analyzer := &stubAnalyzer{report: &largefiles.Report{Threshold: 1024}}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true, SkipConfirm: true}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.StripPerformed)
	assert.Empty(t, runner.stripCalls)
}

func TestRun_NonTTYWithoutYes_Errors(t *testing.T) {
	wd, _ := newRewriterTestEnv(t)
	analyzer := &stubAnalyzer{report: &largefiles.Report{
		Threshold: 1024,
		Flagged:   []largefiles.FlaggedPath{{Path: "big.bin", MaxDeletedUnpackedBytes: 9999, CumulativeBytes: 9999, Reason: largefiles.ReasonBoth}},
	}}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TTY")
	assert.Empty(t, runner.stripCalls)
}

func TestRun_NonInteractiveWithoutYes_Errors(t *testing.T) {
	wd, _ := newRewriterTestEnv(t)
	analyzer := &stubAnalyzer{report: &largefiles.Report{
		Threshold: 1024,
		Flagged:   []largefiles.FlaggedPath{{Path: "big.bin", MaxDeletedUnpackedBytes: 9999, CumulativeBytes: 9999, Reason: largefiles.ReasonBoth}},
	}}
	runner := &stubRunner{}
	// Even with isTTY=true, --non-interactive should hard-error.
	r := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true, NonInteractive: true}, true, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-interactive")
	assert.Empty(t, runner.stripCalls)
}

func TestRun_UserScriptsOnly(t *testing.T) {
	wd, bare := newRewriterTestEnv(t)
	scriptPath := filepath.Join(t.TempDir(), "rewrite.commit-callback.py")
	require.NoError(t, os.WriteFile(scriptPath, []byte("# noop"), 0o644))
	// filter-repo would write commit-map; emulate.
	require.NoError(t, os.MkdirAll(filepath.Join(bare, "filter-repo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bare, "filter-repo", "commit-map"), []byte("old new\n"), 0o644))

	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoScripts: []string{scriptPath}}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, []string{"rewrite.commit-callback.py"}, res.ScriptsRun)
	require.Len(t, runner.callbackCalls, 1)
	assert.Empty(t, runner.stripCalls)
	assert.Empty(t, runner.runCalls)
}

func TestRun_UserFlagsOnly(t *testing.T) {
	wd, bare := newRewriterTestEnv(t)
	require.NoError(t, os.MkdirAll(filepath.Join(bare, "filter-repo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bare, "filter-repo", "commit-map"), []byte("old new\n"), 0o644))

	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoFlags: []string{"--refs", "main"}}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, []string{"--refs", "main"}, res.UserFlagsApplied)
	require.Len(t, runner.runCalls, 1)
	assert.Empty(t, runner.stripCalls)
	assert.Empty(t, runner.callbackCalls)
}

func TestRun_StripScriptsAndFlags_OrderedAndAllCalled(t *testing.T) {
	wd, bare := newRewriterTestEnv(t)
	scriptPath := filepath.Join(t.TempDir(), "x.commit-callback.py")
	require.NoError(t, os.WriteFile(scriptPath, []byte("# noop"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(bare, "filter-repo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bare, "filter-repo", "commit-map"), []byte("old new\n"), 0o644))

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
	assert.Equal(t, []string{"strip", "callbacks", "run"}, runner.order)
	assert.True(t, res.StripPerformed)
	assert.NotEmpty(t, res.ScriptsRun)
	assert.NotEmpty(t, res.UserFlagsApplied)
}

func TestRun_StripWithPathSelectionFlag_Rejected(t *testing.T) {
	wd, _ := newRewriterTestEnv(t)
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{
		StripLargeFiles: true,
		SkipConfirm:     true,
		FilterRepoFlags: []string{"--invert-paths"},
	}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--invert-paths")
	assert.Empty(t, runner.stripCalls)
}

func TestRun_DuplicateCallbackKind_Rejected(t *testing.T) {
	wd, _ := newRewriterTestEnv(t)
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

func TestRun_UnknownCallbackKind_Rejected(t *testing.T) {
	wd, _ := newRewriterTestEnv(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "weird.py")
	require.NoError(t, os.WriteFile(bad, []byte("x"), 0o644))

	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoScripts: []string{bad}}, false, false)
	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown callback kind")
}

func TestHandoffCommitMap_CopiesContent(t *testing.T) {
	wd, bare := newRewriterTestEnv(t)
	src := filepath.Join(bare, "filter-repo", "commit-map")
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
	wd, bare := newRewriterTestEnv(t)
	r := newWithDeps(wd, &stubRunner{}, &stubAnalyzer{}, nil, Config{})
	warn := r.handoffCommitMap(filepath.Join(bare, "filter-repo", "commit-map"))
	assert.Contains(t, warn, "no commit-map produced")
	_, err := os.Stat(wd.CommitMap())
	assert.True(t, os.IsNotExist(err))
}

func TestSanitizeUserFlags_RedactsCallbackBodies(t *testing.T) {
	in := []string{
		"--commit-callback=secret-body",
		"--refs",
		"main",
		"--message-callback=other-body",
		"--no-value-flag",
	}
	got := sanitizeUserFlags(in)
	assert.Equal(t, []string{
		"--commit-callback=<redacted>",
		"--refs",
		"main",
		"--message-callback=<redacted>",
		"--no-value-flag",
	}, got)
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
	assert.Contains(t, out, "4.8 MiB") // 5_000_000 bytes
	assert.Contains(t, out, "6.7 MiB") // 7_000_000 bytes
	assert.Contains(t, out, "42")
	assert.Contains(t, out, "a.commit-callback.py")
	assert.Contains(t, out, "--refs")
	assert.Equal(t, []string{"watch out!"}, warns)
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:           "0 B",
		512:         "512 B",
		1024:        "1.0 KiB",
		1536:        "1.5 KiB",
		1 << 20:     "1.0 MiB",
		1<<30 + 100: "1.0 GiB",
	}
	for n, want := range cases {
		assert.Equal(t, want, HumanBytes(n), "n=%d", n)
	}
}
