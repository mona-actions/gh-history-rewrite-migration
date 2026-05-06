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

func TestWorkDirPaths(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	paths := map[string]string{
		"git_archive_raw.tar.gz":      wd.RawGitArchive(),
		"metadata_archive_raw.tar.gz": wd.RawMetadataArchive(),
		"git_extracted":               wd.GitExtractedDir(),
		"metadata_extracted":          wd.MetadataExtractedDir(),
		"git_archive.tar.gz":          wd.GitArchive(),
		"metadata_archive.tar.gz":     wd.MetadataArchive(),
		"commit-map":                  wd.CommitMap(),
		".export-mode":                wd.ExportModeFile(),
		"_combined_raw.tar.gz":        wd.CombinedRawArchive(),
		"_combined_split":             wd.CombinedSplitDir(),
		"_git_stage":                  wd.GitStageDir(),
		"_meta_stage":                 wd.MetaStageDir(),
		"cleanup.txt":                 wd.CleanupTxt(),
	}

	for suffix, path := range paths {
		t.Run(suffix, func(t *testing.T) {
			assert.True(t, filepath.IsAbs(path))
			assert.Equal(t, filepath.Join(tmpDir, suffix), path)
		})
	}
}

func TestIdempotencyHelpers(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	// Initially, no files exist
	assert.False(t, wd.HasCommitMap())
	assert.False(t, wd.HasGitArchive())
	assert.False(t, wd.HasMetadataArchive())

	// Create files
	require.NoError(t, os.WriteFile(wd.CommitMap(), []byte("test"), 0644))
	require.NoError(t, os.WriteFile(wd.GitArchive(), []byte("test"), 0644))
	require.NoError(t, os.WriteFile(wd.MetadataArchive(), []byte("test"), 0644))

	// Now they should exist
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

func TestLock_SweepsPartials(t *testing.T) {
	tmpDir := t.TempDir()
	wd, err := New(tmpDir)
	require.NoError(t, err)

	partialPath := filepath.Join(tmpDir, "git_archive_raw.tar.gz.partial")
	require.NoError(t, os.WriteFile(partialPath, []byte("interrupted"), 0644))

	require.NoError(t, wd.Lock())
	defer wd.Unlock()

	_, err = os.Stat(partialPath)
	assert.True(t, os.IsNotExist(err))
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
