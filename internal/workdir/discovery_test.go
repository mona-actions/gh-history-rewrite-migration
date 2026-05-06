package workdir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindBareRepo(t *testing.T) {
	t.Run("empty tree", func(t *testing.T) {
		root := t.TempDir()

		_, err := FindBareRepo(root)
		assert.ErrorIs(t, err, ErrNoBareRepo)
	})

	t.Run("flat", func(t *testing.T) {
		root := t.TempDir()
		gitDir := filepath.Join(root, "foo.git")
		require.NoError(t, os.MkdirAll(gitDir, 0755))

		got, err := FindBareRepo(root)
		require.NoError(t, err)
		assert.Equal(t, gitDir, got)
	})

	t.Run("real archive depth", func(t *testing.T) {
		root := t.TempDir()
		gitDir := filepath.Join(root, "repositories", "Acme", "foo.git")
		require.NoError(t, os.MkdirAll(gitDir, 0755))

		got, err := FindBareRepo(root)
		require.NoError(t, err)
		assert.Equal(t, gitDir, got)
	})

	t.Run("multi match", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "foo.git"), 0755))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "bar.git"), 0755))

		_, err := FindBareRepo(root)
		assert.ErrorIs(t, err, ErrMultipleBareRepos)
	})

	t.Run("beyond depth limit", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b", "c", "d", "e", "foo.git"), 0755))

		_, err := FindBareRepo(root)
		assert.ErrorIs(t, err, ErrNoBareRepo)
	})

	t.Run("nested git ignored", func(t *testing.T) {
		root := t.TempDir()
		gitDir := filepath.Join(root, "foo.git")
		nestedGitDir := filepath.Join(gitDir, "objects", "pack", "nested.git")
		require.NoError(t, os.MkdirAll(nestedGitDir, 0755))

		got, err := FindBareRepo(root)
		require.NoError(t, err)
		assert.Equal(t, gitDir, got)
	})
}

func TestFindMetadataDirs(t *testing.T) {
	prefixes := []string{"issues", "pull_requests"}

	t.Run("flat root", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "issues_000001.json"))
		writeFile(t, filepath.Join(root, "pull_requests_000001.json"))

		got, err := FindMetadataDirs(root, prefixes)
		require.NoError(t, err)
		assert.Equal(t, []string{root}, got)
	})

	t.Run("single nested subdir", func(t *testing.T) {
		root := t.TempDir()
		metadataDir := filepath.Join(root, "metadata")
		writeFile(t, filepath.Join(metadataDir, "issues_000001.json"))

		got, err := FindMetadataDirs(root, prefixes)
		require.NoError(t, err)
		assert.Equal(t, []string{metadataDir}, got)
	})

	t.Run("ignores sibling non prefix dirs", func(t *testing.T) {
		root := t.TempDir()
		metadataDir := filepath.Join(root, "metadata")
		writeFile(t, filepath.Join(metadataDir, "issues_000001.json"))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "git-lfs"), 0755))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "attachments"), 0755))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "releases"), 0755))

		got, err := FindMetadataDirs(root, prefixes)
		require.NoError(t, err)
		assert.Equal(t, []string{metadataDir}, got)
	})

	t.Run("multiple prefix dirs", func(t *testing.T) {
		root := t.TempDir()
		dir1 := filepath.Join(root, "repo1")
		dir2 := filepath.Join(root, "repo2")
		writeFile(t, filepath.Join(dir1, "issues_000001.json"))
		writeFile(t, filepath.Join(dir2, "pull_requests_000001.json"))

		got, err := FindMetadataDirs(root, prefixes)
		require.NoError(t, err)
		assert.Equal(t, []string{dir1, dir2}, got)
	})

	t.Run("no matches", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "organizations_000001.json"))

		got, err := FindMetadataDirs(root, prefixes)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("beyond depth 3", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "a", "b", "c", "d", "issues_000001.json"))

		got, err := FindMetadataDirs(root, prefixes)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestFindBareRepoSentinelErrors(t *testing.T) {
	assert.True(t, errors.Is(ErrNoBareRepo, ErrNoBareRepo))
	assert.True(t, errors.Is(ErrMultipleBareRepos, ErrMultipleBareRepos))
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0644))
}

func TestDescendIntoSingleSubdir(t *testing.T) {
	t.Run("descends past complete sentinel", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, ".complete"), nil, 0o644))
		require.NoError(t, os.Mkdir(filepath.Join(root, "wrap"), 0o755))

		got, err := DescendIntoSingleSubdir(root)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, "wrap"), got)
	})

	t.Run("does not descend into repositories", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(root, "repositories"), 0o755))

		got, err := DescendIntoSingleSubdir(root)
		require.NoError(t, err)
		assert.Equal(t, root, got)
	})
}
