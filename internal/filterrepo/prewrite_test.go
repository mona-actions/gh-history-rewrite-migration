package filterrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- pure validation / env unit tests (no git required) ---

func writeScript(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(body), mode))
	return p
}

func TestValidatePreRewriteScripts(t *testing.T) {
	dir := t.TempDir()

	good := writeScript(t, dir, "good.pl", "#!/usr/bin/env perl\nprint while <>;\n", 0o755)
	noExec := writeScript(t, dir, "noexec.pl", "#!/usr/bin/env perl\n", 0o644)
	noShebang := writeScript(t, dir, "noshebang.pl", "print 1;\n", 0o755)

	t.Run("valid returns absolute path", func(t *testing.T) {
		abs, err := ValidatePreRewriteScripts([]string{good})
		require.NoError(t, err)
		require.Len(t, abs, 1)
		assert.True(t, filepath.IsAbs(abs[0]))
	})

	t.Run("empty is rejected", func(t *testing.T) {
		_, err := ValidatePreRewriteScripts(nil)
		require.Error(t, err)
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := ValidatePreRewriteScripts([]string{filepath.Join(dir, "nope.pl")})
		require.Error(t, err)
	})

	t.Run("not executable", func(t *testing.T) {
		_, err := ValidatePreRewriteScripts([]string{noExec})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not executable")
	})

	t.Run("missing shebang", func(t *testing.T) {
		_, err := ValidatePreRewriteScripts([]string{noShebang})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shebang")
	})

	t.Run("directory is not a regular file", func(t *testing.T) {
		_, err := ValidatePreRewriteScripts([]string{dir})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "regular file")
	})

	t.Run("windows is rejected", func(t *testing.T) {
		_, err := ValidatePreRewriteScripts([]string{good})
		if runtime.GOOS == "windows" {
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not supported on Windows")
		} else {
			require.NoError(t, err)
		}
	})
}

func TestValidatePreRewriteFlags(t *testing.T) {
	require.Error(t, ValidatePreRewriteFlags([]string{"--refs", "main"}))
	require.Error(t, ValidatePreRewriteFlags([]string{"--refs=refs/heads/main"}))
	require.NoError(t, ValidatePreRewriteFlags([]string{"--replace-message=foo.txt"}))
	require.NoError(t, ValidatePreRewriteFlags(nil))
}

func TestSanitizedScriptEnv(t *testing.T) {
	t.Setenv("GH_PAT", "secret-token")
	t.Setenv("GH_SOURCE_PAT", "secret-source")
	t.Setenv("PATH", "/usr/bin:/bin")

	env := sanitizedScriptEnv()
	joined := strings.Join(env, "\n")

	assert.Contains(t, joined, "LC_ALL=C")
	assert.Contains(t, joined, "PATH=/usr/bin:/bin")
	assert.NotContains(t, joined, "GH_PAT")
	assert.NotContains(t, joined, "secret-token")
	assert.NotContains(t, joined, "GH_SOURCE_PAT")
}

// --- integration tests against real git + git-filter-repo ---

func requireGitFilterRepo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cmd := exec.Command("git", "filter-repo", "--version")
	if err := cmd.Run(); err != nil {
		t.Skip("git filter-repo not available")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "safe.bareRepository=all"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@e.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@e.com")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// plantMalformedRepo builds a non-bare repo whose HEAD commit carries a
// malformed author/committer ident (an embedded Outlook <mailto:…> artifact)
// planted via `git hash-object --literally` so it bypasses git's ident
// validation, then clones it --bare (production always operates on a bare
// repo, where filter-repo writes <bare>/filter-repo/commit-map). Returns the
// bare repo path and the ORIGINAL planted commit SHA.
func plantMalformedRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	git(t, repo, "init", "-q")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\n"), 0o644))
	git(t, repo, "add", "f.txt")
	git(t, repo, "commit", "-qm", "init")

	tree := git(t, repo, "rev-parse", "HEAD^{tree}")
	parent := git(t, repo, "rev-parse", "HEAD")
	bad := "Pat Doe <pat.doe@example.com <mailto:pat.doe@example.com> 1452345014 +0530"
	commit := "tree " + tree + "\nparent " + parent +
		"\nauthor " + bad + "\ncommitter " + bad + "\n\nmalformed ident commit\n"
	commitFile := filepath.Join(dir, "commit.txt")
	require.NoError(t, os.WriteFile(commitFile, []byte(commit), 0o644))

	sha := git(t, repo, "hash-object", "-t", "commit", "-w", "--literally", commitFile)
	git(t, repo, "update-ref", "refs/heads/master", sha)

	bare := filepath.Join(dir, "bare.git")
	git(t, dir, "clone", "-q", "--bare", repo, bare)
	return bare, sha
}

// cleanBareRepo builds a small, well-formed bare repo for tests that exercise
// pipeline plumbing without a malformed ident.
func cleanBareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	git(t, repo, "init", "-q")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\n"), 0o644))
	git(t, repo, "add", "f.txt")
	git(t, repo, "commit", "-qm", "init")
	bare := filepath.Join(dir, "bare.git")
	git(t, dir, "clone", "-q", "--bare", repo, bare)
	return bare
}

func exampleScriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "examples", "scripts", "repair-malformed-idents.pre-rewrite.pl"))
	require.NoError(t, err)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("example script not found: %v", err)
	}
	return p
}

func TestRun_PreRewriteRepairsMalformedIdent(t *testing.T) {
	requireGitFilterRepo(t)
	repo, origSHA := plantMalformedRepo(t)
	script := exampleScriptPath(t)

	r := New(DefaultExecer{}, nil).WithStdout(os.Stderr)
	err := r.Run(context.Background(), repo, CombinedOpts{
		PreRewriteScripts: []string{script},
	})
	require.NoError(t, err)

	// The malformed ident is repaired.
	authors := git(t, repo, "log", "--all", "--format=%an <%ae>")
	assert.Contains(t, authors, "Pat Doe <pat.doe@example.com>")
	assert.NotContains(t, authors, "mailto:")

	// fsck is clean (no badEmail).
	out := git(t, repo, "fsck")
	assert.NotContains(t, strings.ToLower(out), "bademail")

	// The commit-map maps the ORIGINAL planted SHA to a new one.
	mapPath := filepath.Join(repo, "filter-repo", "commit-map")
	data, err := os.ReadFile(mapPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), origSHA, "commit-map must reference the original SHA (needs --show-original-ids)")
}

func TestRun_PreRewritePassthroughPreservesHistory(t *testing.T) {
	requireGitFilterRepo(t)
	repo := cleanBareRepo(t)

	// A pure passthrough script (cat) must leave a parseable history valid
	// and still produce a commit-map — the plumbing is transparent.
	dir := t.TempDir()
	cat := writeScript(t, dir, "cat.sh", "#!/bin/sh\nexec cat\n", 0o755)

	r := New(DefaultExecer{}, nil).WithStdout(os.Stderr)
	err := r.Run(context.Background(), repo, CombinedOpts{
		PreRewriteScripts: []string{cat},
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(repo, "filter-repo", "commit-map"))
}

func TestRun_PreRewriteScriptExitCodePropagates(t *testing.T) {
	requireGitFilterRepo(t)
	repo := cleanBareRepo(t)

	dir := t.TempDir()
	boom := writeScript(t, dir, "boom.sh", "#!/bin/sh\nexit 3\n", 0o755)

	r := New(DefaultExecer{}, nil).WithStdout(os.Stderr)
	err := r.Run(context.Background(), repo, CombinedOpts{
		PreRewriteScripts: []string{boom},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pre-rewrite script boom.sh")
}

func TestRun_PreRewriteRejectsRefsPassthrough(t *testing.T) {
	requireGitFilterRepo(t)
	repo := cleanBareRepo(t)
	script := exampleScriptPath(t)

	r := New(DefaultExecer{}, nil)
	err := r.Run(context.Background(), repo, CombinedOpts{
		PreRewriteScripts: []string{script},
		PassthroughFlags:  []string{"--refs", "master"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--refs")
}

// --- root-cause attribution unit tests (no git required) ---

// waitErr runs a tiny shell command and returns the error from Wait, used to
// produce genuine *exec.ExitError values (SIGPIPE-killed vs plain nonzero exit).
func waitErr(t *testing.T, shellCmd string) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", shellCmd)
	return cmd.Run()
}

func TestIsBrokenPipe(t *testing.T) {
	assert.False(t, isBrokenPipe(nil))
	assert.False(t, isBrokenPipe(waitErr(t, "exit 3")), "plain nonzero exit is not a broken pipe")
	assert.True(t, isBrokenPipe(waitErr(t, "kill -PIPE $$")), "SIGPIPE death is a broken pipe")
}

func TestReportPipelineFailure(t *testing.T) {
	sigpipe := waitErr(t, "kill -PIPE $$")
	exit3 := waitErr(t, "exit 3")
	require.Error(t, sigpipe)
	require.Error(t, exit3)

	stages := func() []pipelineStage {
		return []pipelineStage{
			{name: "git fast-export", err: &cappedBuffer{}},
			{name: "pre-rewrite script repair.pl", err: &cappedBuffer{}},
			{name: "git filter-repo --stdin", err: &cappedBuffer{}},
		}
	}

	t.Run("all succeed", func(t *testing.T) {
		assert.NoError(t, reportPipelineFailure(stages(), []error{nil, nil, nil}))
	})

	t.Run("script failure outranks upstream broken pipe", func(t *testing.T) {
		// fast-export dies from SIGPIPE because the script exited early — the
		// script is the real culprit and must be reported.
		err := reportPipelineFailure(stages(), []error{sigpipe, exit3, sigpipe})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pre-rewrite script repair.pl")
		assert.NotContains(t, err.Error(), "git fast-export failed")
	})

	t.Run("sigpipe-only falls back to first failure", func(t *testing.T) {
		err := reportPipelineFailure(stages(), []error{sigpipe, nil, nil})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "git fast-export")
	})

	t.Run("first non-sigpipe failure wins in pipeline order", func(t *testing.T) {
		err := reportPipelineFailure(stages(), []error{exit3, exit3, nil})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "git fast-export")
	})
}

func TestCappedBuffer(t *testing.T) {
	t.Run("under cap retains everything verbatim", func(t *testing.T) {
		var c cappedBuffer
		_, _ = c.Write([]byte("hello "))
		_, _ = c.Write([]byte("world"))
		assert.Equal(t, "hello world", c.String())
	})

	t.Run("over cap keeps the tail and marks truncation", func(t *testing.T) {
		var c cappedBuffer
		_, _ = c.Write([]byte(strings.Repeat("a", maxStageStderr)))
		_, _ = c.Write([]byte("FINAL-ERROR"))
		assert.LessOrEqual(t, len(c.buf), maxStageStderr, "retained bytes must stay bounded")
		got := c.String()
		assert.Contains(t, got, "truncated")
		assert.True(t, strings.HasSuffix(got, "FINAL-ERROR"), "must retain the most recent bytes")
	})
}
