package filterrepo

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeExecer records calls and optionally writes fixture files when the
// command being run is `git filter-repo --analyze`.
type fakeExecer struct {
	calls        [][]string
	analyzeFiles map[string]string // filename in <bare>/filter-repo/analysis -> content
	runErr       error
	lookPathErr  error
}

func (f *fakeExecer) LookPath(name string) (string, error) {
	if f.lookPathErr != nil {
		return "", f.lookPathErr
	}
	return "/usr/bin/" + name, nil
}

func (f *fakeExecer) Run(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(args) >= 2 && args[0] == "filter-repo" && args[1] == "--analyze" && f.analyzeFiles != nil {
		analysisDir := filepath.Join(dir, "filter-repo", "analysis")
		if err := os.MkdirAll(analysisDir, 0o755); err != nil {
			return err
		}
		for fname, content := range f.analyzeFiles {
			if err := os.WriteFile(filepath.Join(analysisDir, fname), []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	return f.runErr
}

func TestValidateUserFlags_AlwaysBlocked(t *testing.T) {
	cases := []string{"--force", "--analyze", "--dry-run", "--debug"}
	for _, flag := range cases {
		t.Run(flag, func(t *testing.T) {
			err := ValidateUserFlags([]string{flag}, false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), flag)
			err = ValidateUserFlags([]string{flag}, true)
			require.Error(t, err)
		})
	}
}

func TestValidateUserFlags_PathFamily(t *testing.T) {
	pathFlags := []string{"--invert-paths", "--paths-from-file", "--path", "--paths", "--path-glob", "--path-regex"}
	for _, flag := range pathFlags {
		t.Run(flag+"_strip_active", func(t *testing.T) {
			err := ValidateUserFlags([]string{flag}, true)
			require.Error(t, err, "should reject when stripActive=true")
			assert.Contains(t, err.Error(), flag)
		})
		t.Run(flag+"_strip_inactive", func(t *testing.T) {
			err := ValidateUserFlags([]string{flag}, false)
			require.NoError(t, err, "should allow when stripActive=false")
		})
	}
}

func TestValidateUserFlags_AcceptsFlagWithEquals(t *testing.T) {
	err := ValidateUserFlags([]string{"--force=true"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--force")
}

func TestValidateUserFlags_AllowsArbitraryFlags(t *testing.T) {
	err := ValidateUserFlags([]string{"--refs", "main", "--partial"}, false)
	require.NoError(t, err)
}

func TestValidateUserFlags_DuplicateCallback(t *testing.T) {
	err := ValidateUserFlags([]string{"--commit-callback", "body1", "--commit-callback", "body2"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}

func TestValidateUserFlags_NonFlagTokensIgnored(t *testing.T) {
	// e.g. callback bodies passed as the value to --commit-callback are
	// non-flag tokens and must not be misclassified.
	err := ValidateUserFlags([]string{"--commit-callback", "return commit", "--refs", "main"}, false)
	require.NoError(t, err)
}

func TestCallbackKindFor_AllSuffixes(t *testing.T) {
	cases := map[string]string{
		"x.commit-callback.py":   "--commit-callback",
		"x.email-callback.py":    "--email-callback",
		"x.blob-callback.py":     "--blob-callback",
		"x.filename-callback.py": "--filename-callback",
		"x.message-callback.py":  "--message-callback",
		"x.refname-callback.py":  "--refname-callback",
		"x.tag-callback.py":      "--tag-callback",
		"x.reset-callback.py":    "--reset-callback",
	}
	for filename, want := range cases {
		t.Run(filename, func(t *testing.T) {
			got, err := CallbackKindFor(filename)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestCallbackKindFor_UnknownSuffix(t *testing.T) {
	_, err := CallbackKindFor("notacallback.py")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown callback kind")
}

func TestCallbackKindFor_PathPrefix(t *testing.T) {
	got, err := CallbackKindFor("/tmp/scripts/fixmail.email-callback.py")
	require.NoError(t, err)
	assert.Equal(t, "--email-callback", got)
}

func TestRunCombined_AssemblesSingleInvocation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fixmail.email-callback.py")
	const body = "# secret-looking content"
	require.NoError(t, os.WriteFile(script, []byte(body), 0o644))

	exec := &fakeExecer{}
	r := New(exec, nil)
	err := r.RunCombined(context.Background(), dir, CombinedOpts{
		StripActive:      true,
		PathsFromFile:    "/tmp/cleanup.txt",
		ScriptPaths:      []string{script},
		PassthroughFlags: []string{"--refs", "main"},
	})
	require.NoError(t, err)
	require.Len(t, exec.calls, 1, "exactly one filter-repo invocation")
	args := exec.calls[0]
	assert.Equal(t, []string{
		"git", "filter-repo", "--force",
		"--invert-paths", "--paths-from-file", "/tmp/cleanup.txt",
		"--email-callback", body,
		"--refs", "main",
	}, args)
}

func TestRunCombined_StripOnly(t *testing.T) {
	exec := &fakeExecer{}
	r := New(exec, nil)
	err := r.RunCombined(context.Background(), "/tmp/repo.git", CombinedOpts{
		StripActive:   true,
		PathsFromFile: "/tmp/cleanup.txt",
	})
	require.NoError(t, err)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{
		"git", "filter-repo", "--force", "--invert-paths", "--paths-from-file", "/tmp/cleanup.txt",
	}, exec.calls[0])
}

func TestRunCombined_FlagsOnly(t *testing.T) {
	exec := &fakeExecer{}
	r := New(exec, nil)
	err := r.RunCombined(context.Background(), "/tmp", CombinedOpts{
		PassthroughFlags: []string{"--refs", "main"},
	})
	require.NoError(t, err)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"git", "filter-repo", "--force", "--refs", "main"}, exec.calls[0])
}

func TestRunCombined_RejectsReservedFlag(t *testing.T) {
	exec := &fakeExecer{}
	r := New(exec, nil)
	err := r.RunCombined(context.Background(), "/tmp", CombinedOpts{
		PassthroughFlags: []string{"--force"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--force")
	assert.Empty(t, exec.calls)
}

func TestRunCombined_RejectsStripBlockedPathFamily(t *testing.T) {
	exec := &fakeExecer{}
	r := New(exec, nil)
	err := r.RunCombined(context.Background(), "/tmp", CombinedOpts{
		StripActive:      true,
		PathsFromFile:    "/tmp/cleanup.txt",
		PassthroughFlags: []string{"--invert-paths"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--invert-paths")
	assert.Empty(t, exec.calls)
}

func TestRunCombined_UnknownSuffix(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "noop.py")
	require.NoError(t, os.WriteFile(bad, []byte(""), 0o644))

	exec := &fakeExecer{}
	r := New(exec, nil)
	err := r.RunCombined(context.Background(), dir, CombinedOpts{ScriptPaths: []string{bad}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown callback kind")
	assert.Empty(t, exec.calls)
}

func TestRunCombined_DuplicateKindAcrossScripts(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.commit-callback.py")
	b := filepath.Join(dir, "b.commit-callback.py")
	require.NoError(t, os.WriteFile(a, []byte("# a"), 0o644))
	require.NoError(t, os.WriteFile(b, []byte("# b"), 0o644))

	exec := &fakeExecer{}
	r := New(exec, nil)
	err := r.RunCombined(context.Background(), dir, CombinedOpts{ScriptPaths: []string{a, b}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
	assert.Empty(t, exec.calls)
}

func TestRunCombined_DuplicateKindAcrossScriptAndFlag(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "a.commit-callback.py")
	require.NoError(t, os.WriteFile(script, []byte("# a"), 0o644))

	exec := &fakeExecer{}
	r := New(exec, nil)
	err := r.RunCombined(context.Background(), dir, CombinedOpts{
		ScriptPaths:      []string{script},
		PassthroughFlags: []string{"--commit-callback=return commit"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
	assert.Contains(t, err.Error(), "--commit-callback")
	assert.Empty(t, exec.calls)
}

func TestRunCombined_NeverLogsBody(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "x.commit-callback.py")
	const secretBody = "TOTALLY-SECRET-CALLBACK-BODY"
	require.NoError(t, os.WriteFile(script, []byte(secretBody), 0o644))

	rec := &recordingLogger{}
	r := New(&fakeExecer{}, rec)
	err := r.RunCombined(context.Background(), dir, CombinedOpts{ScriptPaths: []string{script}})
	require.NoError(t, err)

	for _, msg := range rec.msgs {
		assert.False(t, strings.Contains(msg, secretBody),
			"logger should never see callback script body, got: %q", msg)
	}
}

func TestRunCombined_PropagatesExecError(t *testing.T) {
	r := New(&fakeExecer{runErr: errors.New("boom")}, nil)
	err := r.RunCombined(context.Background(), "/tmp", CombinedOpts{
		PassthroughFlags: []string{"--refs", "main"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestRunCombined_MissingScriptErrors(t *testing.T) {
	exec := &fakeExecer{}
	r := New(exec, nil)
	err := r.RunCombined(context.Background(), "/tmp", CombinedOpts{
		ScriptPaths: []string{filepath.Join(t.TempDir(), "missing.commit-callback.py")},
	})
	require.Error(t, err)
	assert.Empty(t, exec.calls)
}

func TestRedactForLog(t *testing.T) {
	got := redactForLog([]string{"filter-repo", "--commit-callback", "some code", "--refs", "main"})
	assert.Equal(t, "filter-repo --commit-callback <redacted> --refs main", got)

	got = redactForLog([]string{"filter-repo", "--blob-callback=some code", "--refs", "main"})
	assert.Equal(t, "filter-repo --blob-callback=<redacted> --refs main", got)

	got = redactForLog([]string{"filter-repo", "--partial", "--refs", "main"})
	assert.Equal(t, "filter-repo --partial --refs main", got)
}

// --- Analyze parsing -------------------------------------------------------

const pathAllSizesFixture = `=== All paths by reverse accumulated size ===
Format: unpacked size, packed size, date deleted, path name
       1048576         524288 <none>     small/file.txt
     524288000      262144000 <none>     data/backup_1.bak
        10240           5120 <none>     dir with spaces/file.bin
`

const pathDeletedSizesFixture = `=== Deleted paths by reverse accumulated size ===
Format: unpacked size, packed size, date deleted, path name
     524288000      262144000 2024-01-15 data/backup_1.bak
     500000000      250000000 2024-02-20 data/backup_1.bak
`

func TestAnalyze_ParsesFiles(t *testing.T) {
	dir := t.TempDir()
	exec := &fakeExecer{
		analyzeFiles: map[string]string{
			"path-all-sizes.txt":     pathAllSizesFixture,
			"path-deleted-sizes.txt": pathDeletedSizesFixture,
		},
	}
	r := New(exec, nil)

	res, err := r.Analyze(context.Background(), dir)
	require.NoError(t, err)
	require.NotNil(t, res)

	byPath := map[string]PathStats{}
	for _, p := range res.Paths {
		byPath[p.Path] = p
	}

	// data/backup_1.bak appears twice in deleted-sizes; max should be the
	// larger of the two (524288000), and cumulative comes from
	// path-all-sizes.
	bk, ok := byPath["data/backup_1.bak"]
	require.True(t, ok)
	assert.EqualValues(t, 524288000, bk.MaxDeletedUnpackedBytes)
	assert.EqualValues(t, 524288000, bk.CumulativeBytes)

	// small/file.txt only in path-all-sizes; max defaults to cumulative.
	sm, ok := byPath["small/file.txt"]
	require.True(t, ok)
	assert.EqualValues(t, 1048576, sm.MaxDeletedUnpackedBytes)
	assert.EqualValues(t, 1048576, sm.CumulativeBytes)

	// Path with spaces preserved verbatim.
	sp, ok := byPath["dir with spaces/file.bin"]
	require.True(t, ok)
	assert.EqualValues(t, 10240, sp.CumulativeBytes)

	// Sort order: largest max(max,cum) first.
	require.NotEmpty(t, res.Paths)
	assert.Equal(t, "data/backup_1.bak", res.Paths[0].Path)
}

func TestAnalyze_MissingDeletedFileTolerated(t *testing.T) {
	dir := t.TempDir()
	exec := &fakeExecer{
		analyzeFiles: map[string]string{
			"path-all-sizes.txt": pathAllSizesFixture,
		},
	}
	r := New(exec, nil)
	res, err := r.Analyze(context.Background(), dir)
	require.NoError(t, err)
	assert.NotEmpty(t, res.Paths)
}

func TestAnalyze_MissingAllSizesIsError(t *testing.T) {
	dir := t.TempDir()
	exec := &fakeExecer{}
	r := New(exec, nil)
	_, err := r.Analyze(context.Background(), dir)
	require.Error(t, err)
}

func TestParseSizesFile_SkipsHeaderLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.txt")
	require.NoError(t, os.WriteFile(p, []byte(pathAllSizesFixture), 0o644))

	out, err := parseSizesFile(p, false)
	require.NoError(t, err)
	assert.Len(t, out, 3)
}

func TestExtractPathColumn_HandlesSpaces(t *testing.T) {
	got := extractPathColumn("       10240           5120 <none>     dir with spaces/file.bin")
	assert.Equal(t, "dir with spaces/file.bin", got)
}

type recordingLogger struct{ msgs []string }

func (r *recordingLogger) Info(m string, args ...any)  { r.msgs = append(r.msgs, m) }
func (r *recordingLogger) Warn(m string, args ...any)  { r.msgs = append(r.msgs, m) }
func (r *recordingLogger) Error(m string, args ...any) { r.msgs = append(r.msgs, m) }

func TestCountCommitsRemapped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "commit-map")
	content := "old                                      new\n" +
		"\n" +
		"# this is a comment\n" +
		"aaa111 bbb222\n" +
		"   \n" +
		"ccc333 ddd444\n" +
		"# trailing comment\n" +
		"eee555 fff666\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := CountCommitsRemapped(path)
	if err != nil {
		t.Fatalf("CountCommitsRemapped: %v", err)
	}
	if got != 3 {
		t.Fatalf("expected 3 mappings, got %d", got)
	}
}

func TestCountCommitsRemappedMissing(t *testing.T) {
	_, err := CountCommitsRemapped(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected error for missing commit-map")
	}
}
