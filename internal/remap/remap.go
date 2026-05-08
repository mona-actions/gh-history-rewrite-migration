// Package remap rewrites commit SHA references embedded in GitHub migration
// metadata archives after git-filter-repo has rewritten repository history.
package remap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mona-actions/gh-commit-remap/pkg/archive"
	"github.com/mona-actions/gh-commit-remap/pkg/commitremap"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/atomicfs"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// Input bundles every path the Remapper needs. All path fields MUST be
// absolute. Use Validate before calling Run to surface configuration problems
// with clear error messages instead of opaque IO failures deep inside remap.
type Input struct {
	WorkDir              *workdir.WorkDir
	RawMetadataArchive   string
	CommitMapPath        string
	MetadataExtractedDir string
}

// Result is what a successful Run returns. The two archive paths MUST be the
// canonical work-dir paths so the orchestrator can pass them straight to GEI.
type Result struct {
	GitArchivePath      string
	MetadataArchivePath string
	CommitsRemapped     int
	FilesScanned        int
	Warnings            []string
}

// Remapper is the abstraction over gh-commit-remap.
type Remapper interface {
	Run(ctx context.Context, in Input) (Result, error)
}

// RealRemapper rewrites SHA-bearing metadata JSON files in a GEI archive.
type RealRemapper struct {
	Logger output.Logger
}

func NewReal(logger output.Logger) *RealRemapper {
	return &RealRemapper{Logger: logger}
}

func (r *RealRemapper) Run(ctx context.Context, in Input) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := Validate(in); err != nil {
		return Result{}, err
	}

	finalArchive := in.WorkDir.MetadataArchive()
	if atomicfs.IsFileComplete(finalArchive) {
		warnings := []string{"metadata archive already exists; skipped"}
		r.emitWarnings(warnings)
		return Result{
			GitArchivePath:      in.WorkDir.GitArchive(),
			MetadataArchivePath: finalArchive,
			Warnings:            warnings,
		}, nil
	}

	commitMap, err := commitremap.ParseCommitMap(in.CommitMapPath)
	if err != nil {
		return Result{}, fmt.Errorf("parse commit-map: %w", err)
	}

	if len(commitMap) == 0 {
		if err := atomicfs.WriteFileAtomicPath(finalArchive, func(partial string) error {
			if atomicfs.IsDirComplete(in.MetadataExtractedDir) {
				return archive.ReTarDir(in.MetadataExtractedDir, partial)
			}
			return atomicfs.CopyFile(in.RawMetadataArchive, partial)
		}); err != nil {
			return Result{}, err
		}
		warnings := []string{
			"commit-map is empty; no SHA rewrites needed (filter-repo produced no rewrites). Metadata archive copied unchanged.",
		}
		r.emitWarnings(warnings)
		return Result{
			GitArchivePath:      in.WorkDir.GitArchive(),
			MetadataArchivePath: finalArchive,
			CommitsRemapped:     0,
			Warnings:            warnings,
		}, nil
	}

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !atomicfs.IsDirComplete(in.MetadataExtractedDir) {
		if err := os.RemoveAll(in.MetadataExtractedDir); err != nil {
			return Result{}, fmt.Errorf("reset metadata extraction dir: %w", err)
		}
		if _, err := archive.UnTar(in.RawMetadataArchive, in.MetadataExtractedDir); err != nil {
			return Result{}, fmt.Errorf("untar metadata: %w", err)
		}
		if err := atomicfs.MarkDirComplete(in.MetadataExtractedDir); err != nil {
			return Result{}, err
		}
	}

	extractRoot, err := workdir.DescendIntoSingleSubdir(in.MetadataExtractedDir)
	if err != nil {
		return Result{}, err
	}

	metadataDirs, err := workdir.FindMetadataDirs(extractRoot, SHABearingPrefixes)
	if err != nil {
		return Result{}, err
	}
	if len(metadataDirs) == 0 {
		return Result{}, fmt.Errorf("no metadata files matching SHA-bearing prefixes found under %s; archive may be empty or format changed", extractRoot)
	}
	if len(metadataDirs) > 1 {
		return Result{}, fmt.Errorf("multiple metadata directories found under %s; multi-repo migrations are not supported: %s", extractRoot, strings.Join(metadataDirs, ", "))
	}
	metaDir := metadataDirs[0]

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	stats, err := commitremap.ProcessFiles(metaDir, SHABearingPrefixes, commitMap)
	if err != nil {
		return Result{}, fmt.Errorf("process files: %w", err)
	}

	var warnings []string
	if stats.FilesChanged() == 0 {
		warnings = append(warnings, fmt.Sprintf("commit-map has %d entries but 0 files were rewritten — possible archive format mismatch in %s", len(commitMap), metaDir))
	}

	for _, p := range findUnknownPrefixes(metaDir) {
		warnings = append(warnings, fmt.Sprintf("unrecognized prefix file %q under %s — SHAs in this file (if any) were NOT rewritten; please file a bug", p, metaDir))
	}
	r.emitWarnings(warnings)

	if err := atomicfs.WriteFileAtomicPath(finalArchive, func(partial string) error {
		return archive.ReTarDir(in.MetadataExtractedDir, partial)
	}); err != nil {
		return Result{}, err
	}

	return Result{
		GitArchivePath:      in.WorkDir.GitArchive(),
		MetadataArchivePath: finalArchive,
		CommitsRemapped:     len(commitMap),
		FilesScanned:        stats.FilesScanned,
		Warnings:            warnings,
	}, nil
}

func (r *RealRemapper) emitWarnings(warnings []string) {
	if r.Logger == nil {
		return
	}
	for _, warning := range warnings {
		r.Logger.Warn(warning)
	}
}

// Validate sanity-checks an Input before the remap phase begins.
func Validate(in Input) error {
	if in.WorkDir == nil {
		return errors.New("remap: WorkDir is nil")
	}
	if in.CommitMapPath == "" {
		return errors.New("remap: CommitMapPath is empty")
	}
	if _, err := os.Stat(in.CommitMapPath); err != nil {
		return fmt.Errorf("commit-map missing: %w", err)
	}
	if in.MetadataExtractedDir == "" {
		return errors.New("remap: MetadataExtractedDir is empty")
	}
	if !atomicfs.IsDirComplete(in.MetadataExtractedDir) {
		if in.RawMetadataArchive == "" {
			return errors.New("remap: RawMetadataArchive is empty")
		}
		if _, err := os.Stat(in.RawMetadataArchive); err != nil {
			return fmt.Errorf("raw metadata archive missing: %w", err)
		}
	}
	return nil
}

// findUnknownPrefixes lists *_*.json files at the root of metaDir, subtracts
// SHABearingPrefixes ∪ KnownNonSHAPrefixes, returns the unique prefix names
// of any remainders.
func findUnknownPrefixes(metaDir string) []string {
	matches, _ := filepath.Glob(filepath.Join(metaDir, "*_*.json"))
	known := map[string]bool{}
	for _, p := range SHABearingPrefixes {
		known[p] = true
	}
	for _, p := range KnownNonSHAPrefixes {
		known[p] = true
	}

	seen := map[string]bool{}
	var unknown []string
	for _, m := range matches {
		base := filepath.Base(m)
		stem := strings.TrimSuffix(base, ".json")
		idx := strings.LastIndex(stem, "_")
		if idx <= 0 || !allDigits(stem[idx+1:]) {
			continue
		}
		prefix := stem[:idx]
		if known[prefix] || seen[prefix] {
			continue
		}
		seen[prefix] = true
		unknown = append(unknown, prefix)
	}
	sort.Strings(unknown)
	return unknown
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
