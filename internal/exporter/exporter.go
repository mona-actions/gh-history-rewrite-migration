// Package exporter orchestrates the export phase of the migration: it starts
// an org migration via the GitHub REST migrations API, polls until the
// archive is ready, downloads the combined tarball, extracts it into the
// work directory, and locates the bare repository for downstream rewriting.
package exporter

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/api"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// Config carries user-tunable export parameters wired from cobra/viper flags.
type Config struct {
	// Org is the source organization that owns the repository.
	Org string
	// Repo is the source repository name (without the owner prefix).
	Repo string
	// LockRepositories locks the source repos for the duration of the migration.
	LockRepositories bool
	// ExcludeAttachments omits issue/PR attachments from the archive.
	ExcludeAttachments bool
}

// migrationsClient is the subset of api.API methods Exporter depends on.
// It exists so tests can substitute a fake without spinning up an HTTPS
// listener with a custom transport. *api.API satisfies it directly.
type migrationsClient interface {
	StartOrgMigration(ctx context.Context, org string, opts api.MigrationOpts) (int64, error)
	GetMigration(ctx context.Context, org string, id int64) (*api.Migration, error)
	DownloadArchive(ctx context.Context, org string, id int64, dest string) error
	DeleteMigrationArchive(ctx context.Context, org string, id int64) error
}

// Exporter drives the full one-archive export flow.
type Exporter struct {
	api migrationsClient
	wd  *workdir.WorkDir
	cfg Config

	// Test hooks — set by tests to control timing.
	pollInitial time.Duration
	pollMax     time.Duration
	now         func() time.Time
}

// New constructs an Exporter. The caller owns the lifecycle of api and wd.
func New(a *api.API, wd *workdir.WorkDir, cfg Config) *Exporter {
	return &Exporter{
		api:         a,
		wd:          wd,
		cfg:         cfg,
		pollInitial: 5 * time.Second,
		pollMax:     60 * time.Second,
		now:         time.Now,
	}
}

// Run executes the full export pipeline:
//  1. idempotency check (skip if archive + extracted bare repo already present)
//  2. start org migration
//  3. poll with exponential backoff + jitter until exported / failed
//  4. download archive
//  5. extract tarball (zip-slip safe)
//  6. detect bare repo (errors if multi-repo)
//  7. best-effort delete remote archive
func (e *Exporter) Run(ctx context.Context) error {
	if err := e.validateConfig(); err != nil {
		return err
	}

	// 1. idempotency
	if e.wd.HasArchive() {
		if _, err := e.wd.BareRepoPath(); err == nil {
			output.Info("archive already present, skipping export")
			return nil
		}
		// Archive exists but extraction is missing/broken — re-extract from
		// the cached archive instead of re-downloading.
		output.Info("archive present but not extracted; re-extracting")
		if err := os.RemoveAll(e.wd.Extracted()); err != nil {
			return fmt.Errorf("failed to clean stale extracted dir: %w", err)
		}
		if err := Extract(e.wd.Archive(), e.wd.Extracted()); err != nil {
			return fmt.Errorf("failed to extract cached archive: %w", err)
		}
		return e.assertSingleRepo()
	}

	// 2. start migration
	repoSlug := fmt.Sprintf("%s/%s", e.cfg.Org, e.cfg.Repo)
	output.Info(fmt.Sprintf("starting org migration for %s", repoSlug))

	migrationID, err := e.api.StartOrgMigration(ctx, e.cfg.Org, api.MigrationOpts{
		Repositories:       []string{repoSlug},
		LockRepositories:   e.cfg.LockRepositories,
		ExcludeAttachments: e.cfg.ExcludeAttachments,
	})
	if err != nil {
		return fmt.Errorf("failed to start migration: %w", err)
	}

	// 3. poll
	if err := e.pollUntilExported(ctx, migrationID); err != nil {
		return err
	}

	// 4. download
	if err := e.downloadWithProgress(ctx, migrationID); err != nil {
		return fmt.Errorf("failed to download archive: %w", err)
	}

	// 5. extract
	output.Info("extracting archive")
	if err := Extract(e.wd.Archive(), e.wd.Extracted()); err != nil {
		return fmt.Errorf("failed to extract archive: %w", err)
	}

	// 6. multi-repo detect
	if err := e.assertSingleRepo(); err != nil {
		return err
	}

	// 7. best-effort cleanup
	if err := e.api.DeleteMigrationArchive(ctx, e.cfg.Org, migrationID); err != nil {
		output.Warn(fmt.Sprintf("failed to delete remote migration archive (best-effort): %v", err))
	}

	output.Success("export complete")
	return nil
}

func (e *Exporter) validateConfig() error {
	if e.cfg.Org == "" {
		return errors.New("org is required")
	}
	if e.cfg.Repo == "" {
		return errors.New("repo is required")
	}
	return nil
}

// assertSingleRepo wraps wd.BareRepoPath errors with a v1 user-friendly
// message when multiple .git directories are detected.
//
// TODO: replace the substring match with a typed sentinel error exported
// from internal/workdir (e.g., workdir.ErrMultipleRepos) once that package
// is in scope to modify. The current coupling to wd's error wording will
// silently regress to a raw error if BareRepoPath's phrasing ever changes.
// See architecture review F1 (intentionally deferred — workdir is outside
// the modify scope of the export task).
func (e *Exporter) assertSingleRepo() error {
	_, err := e.wd.BareRepoPath()
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "multiple .git directories") {
		return fmt.Errorf("this archive contains multiple repos; v1 supports single-repo migrations only — open an issue if you need multi-repo support: %w", err)
	}
	return err
}

// pollUntilExported polls migration state with exponential backoff (capped)
// and jitter until state == "exported", an error occurs, or context is done.
func (e *Exporter) pollUntilExported(ctx context.Context, id int64) error {
	spinner := output.Spinner("waiting for migration to be exported (state: pending)")
	defer func() {
		// Belt-and-suspenders: if the caller didn't transition the spinner
		// (e.g., we returned via error), stop it cleanly.
		if spinner.IsActive {
			_ = spinner.Stop()
		}
	}()

	delay := e.pollInitial
	rng := rand.New(rand.NewSource(e.now().UnixNano()))

	for {
		m, err := e.api.GetMigration(ctx, e.cfg.Org, id)
		if err != nil {
			spinner.Fail("migration poll failed")
			return fmt.Errorf("failed to poll migration: %w", err)
		}

		switch m.State {
		case "exported":
			spinner.Success("archive ready for download")
			return nil
		case "failed":
			spinner.Fail("migration failed")
			return fmt.Errorf("migration %d entered failed state", id)
		default:
			// pending, exporting, or any unrecognized intermediate state —
			// keep polling and surface the state in the spinner.
			spinner.UpdateText(fmt.Sprintf("waiting for migration to be exported (state: %s)", m.State))
		}

		// Sleep with jitter, respecting context cancellation.
		jitter := time.Duration(rng.Int63n(int64(delay) / 4))
		sleep := delay + jitter

		select {
		case <-ctx.Done():
			spinner.Fail("migration poll cancelled")
			return ctx.Err()
		case <-time.After(sleep):
		}

		// Exponential backoff up to cap.
		delay *= 2
		if delay > e.pollMax {
			delay = e.pollMax
		}
	}
}

// downloadWithProgress downloads the migration archive and reports progress
// via a spinner. It uses a tee pipeline so we never load the archive in
// memory; bytes are streamed to disk by the underlying api client and the
// progress count is computed from the file size on tick.
func (e *Exporter) downloadWithProgress(ctx context.Context, id int64) error {
	spinner := output.Spinner("downloading archive")

	// Tick goroutine: poll the file size while the download is in flight.
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if fi, err := os.Stat(e.wd.Archive()); err == nil {
					mb := fi.Size() / (1024 * 1024)
					spinner.UpdateText(fmt.Sprintf("downloading archive (%d MB)", mb))
				}
			}
		}
	}()

	err := e.api.DownloadArchive(ctx, e.cfg.Org, id, e.wd.Archive())
	close(done)
	if err != nil {
		spinner.Fail("download failed")
		// Clean up partial file so idempotency check on retry doesn't trip.
		_ = os.Remove(e.wd.Archive())
		return err
	}

	if fi, err := os.Stat(e.wd.Archive()); err == nil {
		mb := fi.Size() / (1024 * 1024)
		spinner.Success(fmt.Sprintf("downloaded archive (%d MB)", mb))
	} else {
		spinner.Success("downloaded archive")
	}
	return nil
}

// Extract extracts a gzipped tarball at archivePath into destDir. It creates
// destDir if needed and protects against zip-slip by rejecting any entry
// whose resolved path falls outside destDir. Symlinks pointing outside
// destDir are also rejected.
func Extract(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("failed to open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to open gzip stream: %w", err)
	}
	defer gz.Close()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("failed to create dest dir: %w", err)
	}

	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("failed to resolve dest dir: %w", err)
	}

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Resolve target path and validate it stays within destDir.
		target := filepath.Join(absDest, hdr.Name)
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return fmt.Errorf("failed to resolve entry path %q: %w", hdr.Name, err)
		}
		if !isWithin(absDest, absTarget) {
			return fmt.Errorf("zip-slip: tar entry %q resolves outside destination", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(absTarget, fileModeFromHeader(hdr.Mode, 0o755)); err != nil {
				return fmt.Errorf("failed to create dir %q: %w", hdr.Name, err)
			}
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA needed for old archives
			if err := os.MkdirAll(filepath.Dir(absTarget), 0o755); err != nil {
				return fmt.Errorf("failed to create parent dir for %q: %w", hdr.Name, err)
			}
			out, err := os.OpenFile(absTarget, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileModeFromHeader(hdr.Mode, 0o644))
			if err != nil {
				return fmt.Errorf("failed to create file %q: %w", hdr.Name, err)
			}
			// Cap copy to declared size to avoid decompression bombs.
			// hdr.Size is authoritative for regular files; LimitReader
			// guards against a malicious tar that lies in its header.
			if hdr.Size < 0 {
				out.Close()
				return fmt.Errorf("invalid negative size in tar entry %q", hdr.Name)
			}
			if _, err := io.Copy(out, io.LimitReader(tr, hdr.Size)); err != nil {
				out.Close()
				return fmt.Errorf("failed to write file %q: %w", hdr.Name, err)
			}
			out.Close()
		case tar.TypeSymlink:
			// Resolve the symlink target relative to the link's directory and
			// reject if it escapes destDir.
			linkTarget := hdr.Linkname
			resolved := linkTarget
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(absTarget), linkTarget)
			}
			absResolved, err := filepath.Abs(resolved)
			if err != nil {
				return fmt.Errorf("failed to resolve symlink %q: %w", hdr.Name, err)
			}
			if !isWithin(absDest, absResolved) {
				return fmt.Errorf("zip-slip: symlink %q -> %q resolves outside destination", hdr.Name, linkTarget)
			}
			if err := os.MkdirAll(filepath.Dir(absTarget), 0o755); err != nil {
				return fmt.Errorf("failed to create parent dir for symlink %q: %w", hdr.Name, err)
			}
			// Best-effort: remove an existing entry so Symlink doesn't fail.
			_ = os.Remove(absTarget)
			if err := os.Symlink(linkTarget, absTarget); err != nil {
				return fmt.Errorf("failed to create symlink %q: %w", hdr.Name, err)
			}
		default:
			// Skip other entry types (hardlinks, devices, fifos) — not
			// expected in GitHub migration archives.
		}
	}
}

// isWithin reports whether target is equal to or a descendant of root.
// Both paths must be absolute and cleaned.
func isWithin(root, target string) bool {
	rootClean := filepath.Clean(root) + string(filepath.Separator)
	targetClean := filepath.Clean(target)
	if targetClean == filepath.Clean(root) {
		return true
	}
	return strings.HasPrefix(targetClean, rootClean)
}

func fileModeFromHeader(mode int64, fallback os.FileMode) os.FileMode {
	if mode == 0 {
		return fallback
	}
	return os.FileMode(mode) & os.ModePerm
}
