package workdir

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	tmpDir := t.TempDir()

	wd, err := New(tmpDir)
	require.NoError(t, err)
	require.NotNil(t, wd)

	// Verify directory exists
	info, err := os.Stat(tmpDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestNew_CreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	nonExistent := filepath.Join(tmpDir, "nonexistent", "nested")

	wd, err := New(nonExistent)
	require.NoError(t, err)
	require.NotNil(t, wd)

	// Verify directory was created
	info, err := os.Stat(nonExistent)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestPathHelpers(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	// All paths should be absolute and under the root
	assert.True(t, filepath.IsAbs(wd.Archive()))
	assert.True(t, filepath.IsAbs(wd.Extracted()))
	assert.True(t, filepath.IsAbs(wd.CommitMap()))
	assert.True(t, filepath.IsAbs(wd.GitArchive()))
	assert.True(t, filepath.IsAbs(wd.MetadataArchive()))
	assert.True(t, filepath.IsAbs(wd.CleanupTxt()))

	// Check expected filenames
	assert.Equal(t, "archive.tar.gz", filepath.Base(wd.Archive()))
	assert.Equal(t, "extracted", filepath.Base(wd.Extracted()))
	assert.Equal(t, "commit-map", filepath.Base(wd.CommitMap()))
	assert.Equal(t, "git_archive.tar.gz", filepath.Base(wd.GitArchive()))
	assert.Equal(t, "metadata_archive.tar.gz", filepath.Base(wd.MetadataArchive()))
	assert.Equal(t, "cleanup.txt", filepath.Base(wd.CleanupTxt()))
}

func TestBareRepoPath_NoGitDir(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	// Create extracted directory without .git
	extractedDir := wd.Extracted()
	err = os.MkdirAll(extractedDir, 0755)
	require.NoError(t, err)

	_, err = wd.BareRepoPath()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no .git directory found")
}

func TestBareRepoPath_OneGitDir(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	// Create extracted directory with one .git
	extractedDir := wd.Extracted()
	gitDir := filepath.Join(extractedDir, "repo.git")
	err = os.MkdirAll(gitDir, 0755)
	require.NoError(t, err)

	path, err := wd.BareRepoPath()
	require.NoError(t, err)
	assert.Equal(t, gitDir, path)
}

func TestBareRepoPath_MultipleGitDirs(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	// Create extracted directory with multiple .git directories
	extractedDir := wd.Extracted()
	gitDir1 := filepath.Join(extractedDir, "repo1.git")
	gitDir2 := filepath.Join(extractedDir, "repo2.git")
	err = os.MkdirAll(gitDir1, 0755)
	require.NoError(t, err)
	err = os.MkdirAll(gitDir2, 0755)
	require.NoError(t, err)

	_, err = wd.BareRepoPath()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "multiple .git directories found")
}

func TestBareRepoPath_ExtractedNotExists(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	// Don't create extracted directory
	_, err = wd.BareRepoPath()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "extracted directory does not exist")
}

func TestIdempotencyHelpers(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	// Initially, no files exist
	assert.False(t, wd.HasArchive())
	assert.False(t, wd.HasCommitMap())
	assert.False(t, wd.HasGitArchive())
	assert.False(t, wd.HasMetadataArchive())

	// Create files
	os.WriteFile(wd.Archive(), []byte("test"), 0644)
	os.WriteFile(wd.CommitMap(), []byte("test"), 0644)
	os.WriteFile(wd.GitArchive(), []byte("test"), 0644)
	os.WriteFile(wd.MetadataArchive(), []byte("test"), 0644)

	// Now they should exist
	assert.True(t, wd.HasArchive())
	assert.True(t, wd.HasCommitMap())
	assert.True(t, wd.HasGitArchive())
	assert.True(t, wd.HasMetadataArchive())
}

func TestLock_Success(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	err = wd.Lock()
	require.NoError(t, err)

	// Verify lock file exists
	lockPath := filepath.Join(tmpDir, ".lock")
	_, err = os.Stat(lockPath)
	assert.NoError(t, err)

	// Clean up
	wd.Unlock()
}

func TestLock_AlreadyLocked(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	// First lock should succeed
	err = wd.Lock()
	require.NoError(t, err)

	// Second lock should fail (same process is alive)
	err = wd.Lock()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "locked by process")

	// Clean up
	wd.Unlock()
}

func TestLock_StaleLock(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	// Create a stale lock file with a PID that doesn't exist.
	// strconv.Itoa here, not string(rune(...)): the latter produces a UTF-8
	// codepoint, not a decimal-PID string, and Lock() now correctly refuses
	// to auto-remove unparseable lock content.
	lockPath := filepath.Join(tmpDir, ".lock")
	stalePID := 99999 // Very unlikely to exist
	os.WriteFile(lockPath, []byte(strconv.Itoa(stalePID)+"\n"), 0644)

	// Lock should succeed by removing stale lock
	err = wd.Lock()
	require.NoError(t, err)

	// Clean up
	wd.Unlock()
}

func TestUnlock_Success(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	// Lock first
	err = wd.Lock()
	require.NoError(t, err)

	// Unlock should succeed
	err = wd.Unlock()
	require.NoError(t, err)

	// Lock file should be gone
	lockPath := filepath.Join(tmpDir, ".lock")
	_, err = os.Stat(lockPath)
	assert.True(t, os.IsNotExist(err))
}

func TestUnlock_NoLock(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	// Unlock without lock should not error
	err = wd.Unlock()
	assert.NoError(t, err)
}
