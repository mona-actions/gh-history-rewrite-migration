// Package exporter orchestrates the export phase of the migration. It produces
// the raw git and metadata archives consumed by downstream v2 phases.
package exporter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	commitarchive "github.com/mona-actions/gh-commit-remap/pkg/archive"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/api"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/atomicfs"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// Mode selects how exporter obtains the two raw archives.
type Mode int

const (
	ModeTwo Mode = iota
	ModeCombined
)

// SharedRootFiles are duplicated into both split archives in combined mode.
var SharedRootFiles = []string{"organizations_", "repositories_", "schema.json"}

// ParseMode parses an export mode string.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "two":
		return ModeTwo, nil
	case "combined":
		return ModeCombined, nil
	default:
		return ModeTwo, fmt.Errorf("invalid export mode %q (want two or combined)", s)
	}
}

func (m Mode) String() string {
	switch m {
	case ModeTwo:
		return "two"
	case ModeCombined:
		return "combined"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// Config carries user-tunable export parameters wired from cobra/viper flags.
type Config struct {
	Org                string
	Repo               string
	Mode               Mode
	LockRepositories   bool
	ExcludeReleases    bool
	ExcludeAttachments bool
}

// migrationsClient is the subset of api.API methods Exporter depends on.
type migrationsClient interface {
	StartOrgMigration(ctx context.Context, org string, opts api.MigrationOpts) (int64, error)
	GetMigration(ctx context.Context, org string, id int64) (*api.Migration, error)
	DownloadArchive(ctx context.Context, org string, id int64, dest string) error
	DeleteMigrationArchive(ctx context.Context, org string, id int64) error
}

// Exporter drives the v2 export flow.
type Exporter struct {
	api migrationsClient
	wd  *workdir.WorkDir
	cfg Config

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

func (e *Exporter) Run(ctx context.Context) error {
	if err := e.validateConfig(); err != nil {
		return err
	}
	mode, err := e.loadOrInitMode(e.cfg.Mode)
	if err != nil {
		return err
	}

	switch mode {
	case ModeTwo:
		return e.runTwoMode(ctx)
	case ModeCombined:
		return e.runCombinedMode(ctx)
	default:
		return fmt.Errorf("unsupported export mode %s", mode.String())
	}
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

func (e *Exporter) loadOrInitMode(wantMode Mode) (Mode, error) {
	path := e.wd.ExportModeFile()
	if data, err := os.ReadFile(path); err == nil {
		got, parseErr := ParseMode(string(data))
		if parseErr != nil {
			return ModeTwo, fmt.Errorf("invalid export mode marker %s: %w", path, parseErr)
		}
		if got != wantMode {
			return got, fmt.Errorf("this work-dir was started in %s mode but current export mode is %s; pass --export-mode %s or use a fresh --work-dir", got.String(), wantMode.String(), got.String())
		}
		return got, nil
	} else if !os.IsNotExist(err) {
		return ModeTwo, fmt.Errorf("read export mode marker: %w", err)
	}

	if err := atomicfs.WriteFileAtomic(path, func(w io.Writer) error {
		_, err := io.WriteString(w, wantMode.String()+"\n")
		return err
	}); err != nil {
		return ModeTwo, fmt.Errorf("write export mode marker: %w", err)
	}
	return wantMode, nil
}

func (e *Exporter) runTwoMode(ctx context.Context) error {
	base := e.baseToggles()
	repo := e.repoSlug()
	if !archiveComplete(e.wd.RawGitArchive()) {
		if err := e.runMigration(ctx, api.GitOnlyMigrationOpts(repo), e.wd.RawGitArchive()); err != nil {
			return err
		}
	}
	if !archiveComplete(e.wd.RawMetadataArchive()) {
		if err := e.runMigration(ctx, api.MetadataOnlyMigrationOpts(repo, base), e.wd.RawMetadataArchive()); err != nil {
			return err
		}
	}
	output.Success("export complete")
	return nil
}

func (e *Exporter) runCombinedMode(ctx context.Context) error {
	rawCombined := e.wd.CombinedRawArchive()
	if atomicfs.IsDirComplete(e.wd.GitExtractedDir()) && atomicfs.IsDirComplete(e.wd.MetadataExtractedDir()) {
		_ = os.Remove(rawCombined)
		output.Success("export complete")
		return nil
	}

	if !archiveComplete(rawCombined) {
		if err := e.runMigration(ctx, api.CombinedMigrationOpts(e.repoSlug(), e.baseToggles()), rawCombined); err != nil {
			return err
		}
	}

	splitDir := e.wd.CombinedSplitDir()
	if err := os.RemoveAll(splitDir); err != nil {
		return err
	}
	if _, err := commitarchive.UnTar(rawCombined, splitDir); err != nil {
		return fmt.Errorf("extract combined archive: %w", err)
	}
	if err := atomicfs.MarkDirComplete(splitDir); err != nil {
		return err
	}

	extractRoot, err := workdir.DescendIntoSingleSubdir(splitDir)
	if err != nil {
		return err
	}
	bareRepo, err := workdir.FindBareRepo(extractRoot)
	if err != nil {
		return err
	}

	gitStage := e.wd.GitStageDir()
	metaStage := e.wd.MetaStageDir()
	if err := os.RemoveAll(gitStage); err != nil {
		return err
	}
	if err := os.RemoveAll(metaStage); err != nil {
		return err
	}
	if err := splitTree(extractRoot, bareRepo, gitStage, metaStage); err != nil {
		return err
	}

	if err := replaceCompleteDir(gitStage, e.wd.GitExtractedDir()); err != nil {
		return err
	}
	if err := replaceCompleteDir(metaStage, e.wd.MetadataExtractedDir()); err != nil {
		return err
	}

	_ = os.RemoveAll(splitDir)
	_ = os.Remove(rawCombined)
	output.Success("export complete")
	return nil
}

func (e *Exporter) runMigration(ctx context.Context, opts api.MigrationOpts, outPath string) (err error) {
	output.Info(fmt.Sprintf("starting org migration for %s", strings.Join(opts.Repositories, ",")))
	migrationID, err := e.api.StartOrgMigration(ctx, e.cfg.Org, opts)
	if err != nil {
		return fmt.Errorf("failed to start migration: %w", err)
	}
	defer func() {
		if ctx.Err() != nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cleanupErr := e.api.DeleteMigrationArchive(cleanupCtx, e.cfg.Org, migrationID); cleanupErr != nil {
			output.Warn(fmt.Sprintf("failed to delete remote migration archive (best-effort): %v", cleanupErr))
		}
	}()

	if err := e.pollUntilExported(ctx, migrationID); err != nil {
		return err
	}
	if err := e.downloadArchive(ctx, migrationID, outPath); err != nil {
		return fmt.Errorf("failed to download archive: %w", err)
	}
	if err := atomicfs.ValidateTarHeader(outPath); err != nil {
		_ = os.Remove(outPath)
		return fmt.Errorf("downloaded archive failed validation: %w", err)
	}
	return nil
}

func (e *Exporter) baseToggles() api.BaseToggles {
	return api.BaseToggles{
		ExcludeReleases:    e.cfg.ExcludeReleases,
		ExcludeAttachments: e.cfg.ExcludeAttachments,
		LockRepositories:   e.cfg.LockRepositories,
	}
}

func (e *Exporter) repoSlug() string {
	return fmt.Sprintf("%s/%s", e.cfg.Org, e.cfg.Repo)
}

func archiveComplete(path string) bool {
	return atomicfs.IsFileComplete(path) && atomicfs.ValidateTarHeader(path) == nil
}

func (e *Exporter) pollUntilExported(ctx context.Context, id int64) error {
	spinner := output.Spinner("waiting for migration to be exported (state: pending)")
	defer func() {
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
			spinner.UpdateText(fmt.Sprintf("waiting for migration to be exported (state: %s)", m.State))
		}

		jitter := time.Duration(0)
		if delay > 0 {
			jitter = time.Duration(rng.Int63n(int64(delay)/4 + 1))
		}
		select {
		case <-ctx.Done():
			spinner.Fail("migration poll cancelled")
			return ctx.Err()
		case <-time.After(delay + jitter):
		}
		delay *= 2
		if delay > e.pollMax {
			delay = e.pollMax
		}
	}
}

func (e *Exporter) downloadArchive(ctx context.Context, id int64, outPath string) error {
	spinner := output.Spinner("downloading archive")
	err := atomicfs.WriteFileAtomicPath(outPath, func(partialPath string) error {
		return e.api.DownloadArchive(ctx, e.cfg.Org, id, partialPath)
	})
	if err != nil {
		spinner.Fail("download failed")
		return err
	}
	if fi, statErr := os.Stat(outPath); statErr == nil {
		spinner.Success(fmt.Sprintf("downloaded archive (%d MB)", fi.Size()/(1024*1024)))
	} else {
		spinner.Success("downloaded archive")
	}
	return nil
}

func splitTree(extractRoot, bareRepo, gitStage, metaStage string) error {
	relBare, err := filepath.Rel(extractRoot, bareRepo)
	if err != nil {
		return err
	}
	if relBare == "." || strings.HasPrefix(relBare, "..") || filepath.IsAbs(relBare) {
		return fmt.Errorf("bare repo %s is not under extract root %s", bareRepo, extractRoot)
	}
	parts := strings.Split(filepath.ToSlash(relBare), "/")
	if len(parts) < 3 || parts[0] != "repositories" {
		return fmt.Errorf("bare repo %s is not under repositories/", bareRepo)
	}
	if err := os.MkdirAll(gitStage, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(metaStage, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(extractRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".complete" {
			continue
		}
		src := filepath.Join(extractRoot, name)
		switch {
		case isSharedRootFile(name):
			if entry.IsDir() {
				return fmt.Errorf("shared root entry %s is a directory", name)
			}
			if err := copyPath(src, filepath.Join(gitStage, name)); err != nil {
				return err
			}
			if err := copyPath(src, filepath.Join(metaStage, name)); err != nil {
				return err
			}
		case name == "repositories":
			if !entry.IsDir() {
				return fmt.Errorf("repositories is not a directory")
			}
			if err := movePath(src, filepath.Join(gitStage, name)); err != nil {
				return err
			}
		default:
			if err := movePath(src, filepath.Join(metaStage, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func isSharedRootFile(name string) bool {
	for _, shared := range SharedRootFiles {
		if strings.HasSuffix(shared, ".json") {
			if name == shared {
				return true
			}
			continue
		}
		if strings.HasPrefix(name, shared) && strings.HasSuffix(name, ".json") {
			return true
		}
	}
	return false
}

func movePath(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !isCrossDevice(err) {
		return err
	}
	if err := atomicfs.CopyTree(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func replaceCompleteDir(src, dst string) error {
	if atomicfs.IsDirComplete(dst) {
		return nil
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := movePath(src, dst); err != nil {
		return err
	}
	return atomicfs.MarkDirComplete(dst)
}

func isCrossDevice(err error) bool {
	if errors.Is(err, syscall.EXDEV) {
		return true
	}
	var linkErr *os.LinkError
	return errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV)
}

func copyPath(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return atomicfs.CopyTree(src, dst)
	}
	return atomicfs.CopyFile(src, dst)
}
