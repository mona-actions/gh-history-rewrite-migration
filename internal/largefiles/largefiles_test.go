package largefiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/filterrepo"
)

func TestParseThreshold_Positive(t *testing.T) {
	cases := map[string]int64{
		"400M":     400 * 1024 * 1024,
		"400MB":    400 * 1024 * 1024,
		"400m":     400 * 1024 * 1024,
		"1G":       1 << 30,
		"1GB":      1 << 30,
		"1.5GB":    int64(1.5 * float64(1<<30)),
		"1024K":    1024 * 1024,
		"1024kb":   1024 * 1024,
		"1048576":  1048576,
		"1048576B": 1048576,
		"  2M  ":   2 * 1024 * 1024,
		"1Ki":      1024,
		"1MiB":     1 << 20,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got, err := ParseThreshold(in)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestParseThreshold_Negative(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"abc",
		"-1",
		"-400M",
		"0",
		"0M",
		"M",      // suffix only
		"1.2.3M", // not a number
		"1Q",     // unknown suffix → numeric parse fails on "1Q"
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := ParseThreshold(in)
			require.Error(t, err)
		})
	}
}

func TestAnalyze_Report_RuleAndReasons(t *testing.T) {
	threshold := int64(100)
	// Input must be sorted descending by Footprint per
	// filterrepo.Runner.Analyze's contract; report() preserves that order.
	res := &filterrepo.AnalyzeResult{
		Paths: []filterrepo.PathStats{
			{Path: "both", MaxDeletedUnpackedBytes: 300, CumulativeBytes: 400},
			{Path: "only-cum", MaxDeletedUnpackedBytes: 50, CumulativeBytes: 200},
			{Path: "only-max", MaxDeletedUnpackedBytes: 200, CumulativeBytes: 50},
			{Path: "neither", MaxDeletedUnpackedBytes: 50, CumulativeBytes: 50},
		},
	}
	a := &Analyzer{threshold: threshold}
	rep := a.report(res)

	require.Equal(t, threshold, rep.Threshold)
	require.Len(t, rep.Flagged, 3)

	byPath := map[string]FlaggedPath{}
	for _, p := range rep.Flagged {
		byPath[p.Path] = p
	}
	assert.Equal(t, ReasonSingleBlob, byPath["only-max"].Reason)
	assert.Equal(t, ReasonCumulative, byPath["only-cum"].Reason)
	assert.Equal(t, ReasonBoth, byPath["both"].Reason)

	_, present := byPath["neither"]
	assert.False(t, present, "neither should not be flagged")

	// Sort: descending by max(max, cum). "both" max=400, "only-max" max=200,
	// "only-cum" max=200; tie broken by path lexicographically.
	assert.Equal(t, "both", rep.Flagged[0].Path)
	// Tied at 200: "only-cum" < "only-max" lexicographically.
	assert.Equal(t, "only-cum", rep.Flagged[1].Path)
	assert.Equal(t, "only-max", rep.Flagged[2].Path)
}

func TestAnalyze_EmptyResultIsEmptyReport(t *testing.T) {
	a := &Analyzer{threshold: 100}
	rep := a.report(&filterrepo.AnalyzeResult{})
	require.NotNil(t, rep)
	assert.Empty(t, rep.Flagged)
}

func TestWriteCleanupFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "cleanup.txt")
	rep := &Report{
		Threshold: 100,
		Flagged: []FlaggedPath{
			{Path: "foo/bar.zip"},
			{Path: "dir with spaces/file.bin"},
			{Path: "deep/nested/path.bin"},
		},
	}
	require.NoError(t, rep.WriteCleanupFile(dest))

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	assert.Equal(t, []string{
		"foo/bar.zip",
		"dir with spaces/file.bin",
		"deep/nested/path.bin",
	}, lines)
}

func TestWriteCleanupFile_EmptyReport(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "cleanup.txt")
	require.NoError(t, (&Report{}).WriteCleanupFile(dest))

	info, err := os.Stat(dest)
	require.NoError(t, err)
	assert.Zero(t, info.Size())
}

func TestWriteCleanupFile_NilReport(t *testing.T) {
	err := (*Report)(nil).WriteCleanupFile("/tmp/x")
	require.Error(t, err)
}

func TestAnalyze_RejectsNonPositiveThreshold(t *testing.T) {
	a := &Analyzer{threshold: 0}
	_, err := a.Analyze(nil, "/tmp")
	require.Error(t, err)
}
