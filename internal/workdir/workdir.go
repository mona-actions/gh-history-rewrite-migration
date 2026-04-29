package workdir

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// WorkDir manages the working directory structure for migration artifacts.
// It provides path helpers and idempotency checks for the migration workflow.
type WorkDir struct {
	root string
}

// New creates a new WorkDir instance and ensures the directory exists and is writable.
// Returns an error if the directory cannot be created or is not writable.
func New(root string) (*WorkDir, error) {
	// Create directory if it doesn't exist
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	// Verify directory is writable by creating a test file
	testFile := filepath.Join(root, ".write-test")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		return nil, fmt.Errorf("work directory is not writable: %w", err)
	}
	os.Remove(testFile)

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	return &WorkDir{root: absRoot}, nil
}

// Archive returns the absolute path to the combined archive downloaded from GitHub.
func (w *WorkDir) Archive() string {
	return filepath.Join(w.root, "archive.tar.gz")
}

// Extracted returns the absolute path to the extracted archive directory.
func (w *WorkDir) Extracted() string {
	return filepath.Join(w.root, "extracted")
}

// CommitMap returns the absolute path to the commit mapping file.
func (w *WorkDir) CommitMap() string {
	return filepath.Join(w.root, "commit-map")
}

// GitArchive returns the absolute path to the rewritten git archive.
func (w *WorkDir) GitArchive() string {
	return filepath.Join(w.root, "git_archive.tar.gz")
}

// MetadataArchive returns the absolute path to the metadata archive.
func (w *WorkDir) MetadataArchive() string {
	return filepath.Join(w.root, "metadata_archive.tar.gz")
}

// CleanupTxt returns the absolute path to the cleanup instructions file.
func (w *WorkDir) CleanupTxt() string {
	return filepath.Join(w.root, "cleanup.txt")
}

// BareRepoPath walks the extracted directory tree and returns the unique *.git directory.
// Returns an error if zero or more than one .git directories are found.
func (w *WorkDir) BareRepoPath() (string, error) {
	extracted := w.Extracted()

	// Check if extracted directory exists
	if _, err := os.Stat(extracted); os.IsNotExist(err) {
		return "", fmt.Errorf("extracted directory does not exist: %s", extracted)
	}

	var gitDirs []string

	err := filepath.WalkDir(extracted, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() && strings.HasSuffix(d.Name(), ".git") {
			gitDirs = append(gitDirs, path)
		}

		return nil
	})

	if err != nil {
		return "", fmt.Errorf("failed to walk extracted directory: %w", err)
	}

	if len(gitDirs) == 0 {
		return "", fmt.Errorf("no .git directory found in %s", extracted)
	}

	if len(gitDirs) > 1 {
		return "", fmt.Errorf("multiple .git directories found in %s: %v", extracted, gitDirs)
	}

	return gitDirs[0], nil
}

// HasArchive checks if the downloaded archive exists.
func (w *WorkDir) HasArchive() bool {
	_, err := os.Stat(w.Archive())
	return err == nil
}

// HasCommitMap checks if the commit map file exists.
func (w *WorkDir) HasCommitMap() bool {
	_, err := os.Stat(w.CommitMap())
	return err == nil
}

// HasGitArchive checks if the git archive exists.
func (w *WorkDir) HasGitArchive() bool {
	_, err := os.Stat(w.GitArchive())
	return err == nil
}

// HasMetadataArchive checks if the metadata archive exists.
func (w *WorkDir) HasMetadataArchive() bool {
	_, err := os.Stat(w.MetadataArchive())
	return err == nil
}

// FreeSpaceGB returns the available disk space in gigabytes for the work directory.
// Returns 0 and error on platforms that don't support statfs.
func (w *WorkDir) FreeSpaceGB() (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(w.root, &stat); err != nil {
		return 0, fmt.Errorf("failed to get disk space: %w", err)
	}

	// Available blocks * block size = available bytes
	availableBytes := stat.Bavail * uint64(stat.Bsize)
	return availableBytes / (1024 * 1024 * 1024), nil
}

// Lock creates a lock file containing this process's PID using atomic
// O_EXCL semantics, so concurrent callers (in the same process or across
// processes) race correctly: exactly one wins.
//
// If a lock file already exists but its PID is dead (or the file is
// malformed), the lock is treated as stale, removed, and creation is retried
// once.
func (w *WorkDir) Lock() error {
	lockPath := filepath.Join(w.root, ".lock")

	tryCreate := func() (bool, error) {
		f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			if os.IsExist(err) {
				return false, nil
			}
			return false, fmt.Errorf("failed to create lock file: %w", err)
		}
		defer f.Close()
		_, err = fmt.Fprintf(f, "%d\n", os.Getpid())
		if err != nil {
			// Don't leave an empty .lock behind — future callers would
			// hit the "creator still writing PID" branch indefinitely.
			_ = os.Remove(lockPath)
			return false, fmt.Errorf("failed to write lock file: %w", err)
		}
		return true, nil
	}

	created, err := tryCreate()
	if err != nil {
		return err
	}
	if created {
		return nil
	}

	// Lock file exists — check liveness of the holder.
	data, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		// Couldn't even read it; treat as held to be safe.
		return fmt.Errorf("work directory is locked (lock file present but unreadable: %v)", readErr)
	}
	pidStr := strings.TrimSpace(string(data))
	if pidStr == "" {
		// Empty lock file means a concurrent winner has called O_EXCL
		// create but hasn't flushed its PID yet (the brief window between
		// OpenFile success and Fprintf flush). Treat as held — never as
		// stale — otherwise the loser would clobber the winner's lock.
		return fmt.Errorf("work directory is locked (creator still writing PID)")
	}
	pid, parseErr := strconv.Atoi(pidStr)
	if parseErr != nil {
		// Non-empty but unparseable. We do NOT auto-remove this — it
		// could be future-format content we don't understand, or a
		// partial write. Surface it so the operator can investigate.
		return fmt.Errorf("work directory is locked (unparseable lock file content; remove %s manually if stale)", lockPath)
	}
	process, findErr := os.FindProcess(pid)
	if findErr == nil {
		// On Unix, FindProcess always succeeds; signal 0 probes liveness.
		if signalErr := process.Signal(syscall.Signal(0)); signalErr == nil {
			return fmt.Errorf("work directory is locked by process %d", pid)
		}
	}
	// Stale lock (PID parsed cleanly + process is dead) — remove and retry
	// exactly once. If the retry also loses the race, treat it as held:
	// another process beat us to the takeover.
	_ = os.Remove(lockPath)
	created, err = tryCreate()
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("work directory is locked (lost stale-lock takeover race)")
	}
	return nil
}

// Unlock removes the lock file if it exists and contains this process's PID.
func (w *WorkDir) Unlock() error {
	lockPath := filepath.Join(w.root, ".lock")

	// Read lock file to verify it's ours
	data, err := os.ReadFile(lockPath)
	if os.IsNotExist(err) {
		// Lock doesn't exist, nothing to do
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read lock file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		// Invalid lock file, remove it
		return os.Remove(lockPath)
	}

	if pid != os.Getpid() {
		return fmt.Errorf("lock file belongs to different process (PID %d, current %d)", pid, os.Getpid())
	}

	return os.Remove(lockPath)
}
