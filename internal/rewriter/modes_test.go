package rewriter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/filterrepo"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/largefiles"
)

func sizeReport() *largefiles.BlobReport {
	return &largefiles.BlobReport{
		Threshold:  1024,
		TotalBlobs: 10,
		Blobs: []largefiles.Blob{
			{SHA: "aaaa", Size: 5_000_000},
			{SHA: "bbbb", Size: 2_000_000},
		},
		Largest: 5_000_000,
		Total:   7_000_000,
	}
}

func flaggedReport() *largefiles.Report {
	return &largefiles.Report{
		Threshold: 1024,
		Flagged:   []largefiles.FlaggedPath{{Path: "big.bin", MaxDeletedUnpackedBytes: 9999, CumulativeBytes: 9999, Reason: largefiles.ReasonBoth}},
	}
}

func TestRun_SizeMode_HappyPath(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	analyzer := &stubAnalyzer{blobs: sizeReport()}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true, SkipConfirm: true}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	assert.True(t, analyzer.scanCalled)
	assert.False(t, analyzer.called, "size mode must not run filter-repo --analyze")

	require.Len(t, runner.combinedCalls, 1)
	opts := runner.combinedCalls[0]
	assert.Equal(t, filterrepo.StripBySize, opts.StripMode)
	assert.EqualValues(t, 1024, opts.StripBlobsBiggerThan)
	assert.Empty(t, opts.PathsFromFile)

	body, err := os.ReadFile(wd.LargeBlobsTxt())
	require.NoError(t, err)
	assert.Equal(t, "aaaa\t5000000\nbbbb\t2000000\n", string(body))

	assert.True(t, res.StripPerformed)
	assert.Equal(t, filterrepo.StripBySize, res.StripMode)
	assert.Equal(t, 2, res.BlobsStripped)
	assert.EqualValues(t, 5_000_000, res.LargestStripped)
	assert.EqualValues(t, 7_000_000, res.BytesFreed)
	assert.Empty(t, res.PathsStripped)
	assert.FileExists(t, wd.GitArchive())
	assert.FileExists(t, wd.CommitMap())
}

func TestRun_SizeMode_ZeroBlobs_NoStrip(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	analyzer := &stubAnalyzer{blobs: &largefiles.BlobReport{Threshold: 1024, TotalBlobs: 3}}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err, "no Gate 1 prompt when nothing exceeds the threshold")
	assert.Empty(t, runner.combinedCalls)
	assert.False(t, res.StripPerformed)
	assert.Equal(t, filterrepo.StripBySize, res.StripMode)
	assert.Zero(t, res.BlobsStripped)
	assert.NoFileExists(t, wd.LargeBlobsTxt())
	assert.FileExists(t, wd.GitArchive())
}

func TestRun_SizeMode_ZeroBlobs_RemovesStaleBlobList(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	require.NoError(t, os.MkdirAll(filepath.Dir(wd.LargeBlobsTxt()), 0o755))
	require.NoError(t, os.WriteFile(wd.LargeBlobsTxt(), []byte("stale\t1\n"), 0o644))
	analyzer := &stubAnalyzer{blobs: &largefiles.BlobReport{Threshold: 1024, TotalBlobs: 3}}
	r := makeRewriter(wd, &stubRunner{}, analyzer, Config{StripLargeFiles: true}, false, false)

	_, err := r.Run(context.Background())
	require.NoError(t, err)
	assert.NoFileExists(t, wd.LargeBlobsTxt())
}

func TestRun_SizeMode_ZeroBlobsWithScripts_RunsWithoutStrip(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	script := filepath.Join(t.TempDir(), "x.commit-callback.py")
	require.NoError(t, os.WriteFile(script, []byte("# noop"), 0o644))
	analyzer := &stubAnalyzer{blobs: &largefiles.BlobReport{Threshold: 1024}}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true, FilterRepoScripts: []string{script}}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	require.Len(t, runner.combinedCalls, 1)
	assert.Zero(t, runner.combinedCalls[0].StripBlobsBiggerThan)
	assert.Equal(t, []string{script}, runner.combinedCalls[0].ScriptPaths)
	assert.False(t, res.StripPerformed)
}

func TestRun_SizeMode_Gate1Errors(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		isTTY   bool
		confirm bool
		want    string
	}{
		{"non-tty", Config{StripLargeFiles: true}, false, true, "not running on a TTY"},
		{"non-interactive", Config{StripLargeFiles: true, NonInteractive: true}, true, true, "--non-interactive"},
		{"declined", Config{StripLargeFiles: true}, true, false, "rewrite aborted by user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wd := newArchiveWorkDir(t, "foo.git")
			runner := &stubRunner{}
			r := makeRewriter(wd, runner, &stubAnalyzer{blobs: sizeReport()}, tc.cfg, tc.isTTY, tc.confirm)

			_, err := r.Run(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, runner.combinedCalls)
			assert.NoFileExists(t, wd.GitArchive())
		})
	}
}

func TestRun_SizeMode_PromptMentionsCountAndThreshold(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	r := makeRewriter(wd, &stubRunner{}, &stubAnalyzer{blobs: sizeReport()}, Config{StripLargeFiles: true}, true, true)
	var prompt string
	r.confirm = func(p string, _ bool) (bool, error) { prompt = p; return true, nil }

	_, err := r.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Strip up to 2 blobs larger than 1.0 KiB from history? This rewrites all commits.", prompt)
}

func TestRun_SizeMode_AllowsPathFlag(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{blobs: sizeReport()}, Config{
		StripLargeFiles: true,
		SkipConfirm:     true,
		FilterRepoFlags: []string{"--path", "foo"},
	}, false, false)

	_, err := r.Run(context.Background())
	require.NoError(t, err)
	require.Len(t, runner.combinedCalls, 1)
	assert.Equal(t, []string{"--path", "foo"}, runner.combinedCalls[0].PassthroughFlags)
}

func TestRun_SizeMode_RejectsStripBlobsFlag(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	analyzer := &stubAnalyzer{blobs: sizeReport()}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{
		StripLargeFiles: true,
		SkipConfirm:     true,
		FilterRepoFlags: []string{"--strip-blobs-bigger-than=1M"},
	}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--strip-blobs-bigger-than")
	assert.False(t, analyzer.scanCalled, "flag validation runs before the scan")
	assert.Empty(t, runner.combinedCalls)
}

func TestRun_SizeMode_ScanErrorPropagated(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, &stubAnalyzer{scanErr: errors.New("cat-file exploded")}, Config{StripLargeFiles: true, SkipConfirm: true}, false, false)

	_, err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cat-file exploded")
	assert.Empty(t, runner.combinedCalls)
}

func TestRun_AnalyzeOnly_ReportsWithoutChanges(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	analyzer := &stubAnalyzer{report: flaggedReport()}
	runner := &stubRunner{}
	r := makeRewriter(wd, runner, analyzer, Config{Analyze: true}, false, false)

	res, err := r.Run(context.Background())
	require.NoError(t, err)
	assert.True(t, res.AnalyzeOnly)
	assert.Equal(t, 1, res.AnalyzedPaths)
	assert.False(t, res.StripPerformed)
	assert.True(t, analyzer.called)
	assert.Equal(t, wd.AnalysisDir(), analyzer.reportDir)
	assert.False(t, analyzer.scanCalled)
	assert.Empty(t, runner.combinedCalls)
	assert.NoFileExists(t, wd.GitArchive())
	assert.NoFileExists(t, wd.CommitMap())
	assert.NoFileExists(t, wd.CleanupTxt())
}

func TestRun_AnalyzeOnly_RejectsRewriteInputs(t *testing.T) {
	script := filepath.Join(t.TempDir(), "x.commit-callback.py")
	require.NoError(t, os.WriteFile(script, []byte("# noop"), 0o644))
	pre := writeExecScript(t, "repair.pre-rewrite.pl")
	cases := map[string]Input{
		"scripts":        {FilterRepoScripts: []string{script}},
		"flags":          {FilterRepoFlags: []string{"--refs", "main"}},
		"pre-scripts":    {PreRewriteScripts: []string{pre}},
		"user-callbacks": {UserCallbacks: []string{"return commit"}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			wd := newArchiveWorkDir(t, "foo.git")
			analyzer := &stubAnalyzer{report: flaggedReport()}
			runner := &stubRunner{}
			r := makeRewriter(wd, runner, analyzer, Config{}, false, false)
			in.WorkDir = wd
			in.Analyze = true

			_, err := r.Run(context.Background(), in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--analyze without --strip-large-files is report-only")
			assert.False(t, analyzer.called)
			assert.Empty(t, runner.combinedCalls)
			assert.NoDirExists(t, wd.GitExtractedDir(), "rejected before extraction")
		})
	}
}

func TestRun_AnalyzeOnly_RunsEvenWhenFinalArchiveExists(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	runner := &stubRunner{}
	first := makeRewriter(wd, runner, &stubAnalyzer{}, Config{FilterRepoFlags: []string{"--refs", "main"}}, false, false)
	_, err := first.Run(context.Background())
	require.NoError(t, err)
	require.FileExists(t, wd.GitArchive())
	require.NoError(t, os.RemoveAll(wd.GitExtractedDir()))

	analyzer := &stubAnalyzer{report: &largefiles.Report{Threshold: 1024}}
	r := makeRewriter(wd, runner, analyzer, Config{Analyze: true}, false, false)
	res, err := r.Run(context.Background())
	require.NoError(t, err)
	assert.True(t, res.AnalyzeOnly)
	assert.Zero(t, res.AnalyzedPaths)
	assert.True(t, analyzer.called)
	assert.Len(t, runner.combinedCalls, 1, "analysis must not invoke the rewrite")
}

func TestRun_AnalyzeOnlyThenSizeMode_Strips(t *testing.T) {
	wd := newArchiveWorkDir(t, "foo.git")
	runner := &stubRunner{}
	analyzer := &stubAnalyzer{report: flaggedReport(), blobs: sizeReport()}

	_, err := makeRewriter(wd, runner, analyzer, Config{Analyze: true}, false, false).Run(context.Background())
	require.NoError(t, err)
	require.Empty(t, runner.combinedCalls)

	res, err := makeRewriter(wd, runner, analyzer, Config{StripLargeFiles: true, SkipConfirm: true}, false, false).Run(context.Background())
	require.NoError(t, err)
	require.Len(t, runner.combinedCalls, 1)
	assert.Equal(t, filterrepo.StripBySize, runner.combinedCalls[0].StripMode)
	assert.True(t, res.StripPerformed)
	assert.FileExists(t, wd.GitArchive())
}

func captureRender(res *Result) (string, []string) {
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
	res.Render(printer, func(m string) { warns = append(warns, m) })
	return buf.String(), warns
}

func TestResultRender_SizeMode(t *testing.T) {
	out, _ := captureRender(&Result{
		StripPerformed:  true,
		StripMode:       filterrepo.StripBySize,
		BlobsStripped:   3,
		LargestStripped: 5_000_000,
		BytesFreed:      7_000_000,
	})
	assert.Contains(t, out, "strip mode|size")
	assert.Contains(t, out, "blobs stripped (up to)|3")
	assert.Contains(t, out, "bytes freed (up to, uncompressed)|")
	assert.NotContains(t, out, "paths stripped")
	assert.NotContains(t, out, "[stripped path]")
	assert.Contains(t, out, "6.7 MiB")
}

func TestResultRender_AnalyzeOnly(t *testing.T) {
	out, warns := captureRender(&Result{AnalyzeOnly: true, AnalyzedPaths: 4, Warnings: []string{"wiki"}})
	assert.Contains(t, out, "mode|analysis only (no changes)")
	assert.Contains(t, out, "paths over threshold|4")
	assert.NotContains(t, out, "strip performed")
	assert.Equal(t, []string{"wiki"}, warns)
}
