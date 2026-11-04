// Package remap rewrites commit SHA references embedded in GitHub migration
// metadata archives after git-filter-repo has rewritten repository history.
package remap

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
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
			if err := ctx.Err(); err != nil {
				return err
			}
			var err error
			if atomicfs.IsDirComplete(in.MetadataExtractedDir) {
				err = archive.ReTarDir(in.MetadataExtractedDir, partial)
			} else {
				err = atomicfs.CopyFile(in.RawMetadataArchive, partial)
			}
			if err != nil {
				return err
			}
			return ctx.Err()
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
	var stats commitremap.Stats
	var metaDir string
	var unknown []string
	if atomicfs.IsDirComplete(in.MetadataExtractedDir) {
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
		metaDir = metadataDirs[0]
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		stats, err = commitremap.ProcessFiles(metaDir, SHABearingPrefixes, commitMap, commitremap.ProcessOptions{})
		if err != nil {
			return Result{}, fmt.Errorf("process files: %w", err)
		}
		if stats.FilesScanned == 0 {
			return Result{}, fmt.Errorf("no metadata files matching SHA-bearing prefixes found under %s; archive may be empty or format changed", metaDir)
		}
		unknown = findUnknownPrefixes(metaDir)
		if err := atomicfs.WriteFileAtomicPath(finalArchive, func(partial string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := archive.ReTarDir(in.MetadataExtractedDir, partial); err != nil {
				return err
			}
			return ctx.Err()
		}); err != nil {
			return Result{}, err
		}
	} else {
		if err := atomicfs.WriteFileAtomicPath(finalArchive, func(partial string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			var err error
			stats, err = archive.StreamRemap(in.RawMetadataArchive, partial, commitMap, SHABearingPrefixes)
			if err != nil {
				return fmt.Errorf("stream remap: %w", err)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			metaDir, unknown, err = inspectMetadataLayout(ctx, in.RawMetadataArchive)
			if err != nil {
				return fmt.Errorf("inspect metadata layout: %w", err)
			}
			return ctx.Err()
		}); err != nil {
			return Result{}, err
		}
	}

	var warnings []string
	if stats.FilesChanged() == 0 {
		warnings = append(warnings, fmt.Sprintf("commit-map has %d entries but 0 files were rewritten — possible archive format mismatch in %s", len(commitMap), metaDir))
	}

	for _, p := range unknown {
		warnings = append(warnings, fmt.Sprintf("unrecognized prefix file %q under %s — SHAs in this file (if any) were NOT rewritten; please file a bug", p, metaDir))
	}
	r.emitWarnings(warnings)

	return Result{
		GitArchivePath:      in.WorkDir.GitArchive(),
		MetadataArchivePath: finalArchive,
		CommitsRemapped:     len(commitMap),
		FilesScanned:        stats.FilesScanned,
		Warnings:            warnings,
	}, nil
}

// Inspect after upstream safety validation, before publishing the partial archive.
// Stats expose changed files only; layout checks need every matched name.
func inspectMetadataLayout(ctx context.Context, file string) (string, []string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = gz.Close() }()
	prefixes := map[string]bool{}
	for _, prefix := range SHABearingPrefixes {
		prefixes[prefix] = true
	}
	roots := map[string]bool{}
	names := map[string][]string{}
	tr := tar.NewReader(gz)
	for {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			name := path.Clean(hdr.Name)
			dir := path.Dir(name)
			if commitremap.ShouldRemap(name, prefixes) {
				roots[dir] = true
			}
			if strings.HasSuffix(name, ".json") && strings.Contains(path.Base(name), "_") {
				names[dir] = append(names[dir], path.Base(name))
			}
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return "", nil, err
		}
	}
	// Tar EOF precedes the gzip trailer; drain it to verify checksum and size.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return "", nil, err
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	var dirs []string
	for dir := range roots {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		return "", nil, fmt.Errorf("no metadata files matching SHA-bearing prefixes found under %s; archive may be empty or format changed", file)
	}
	if len(dirs) > 1 {
		return "", nil, fmt.Errorf("multiple metadata directories found under %s; multi-repo migrations are not supported: %s", file, strings.Join(dirs, ", "))
	}
	return file + " (" + dirs[0] + ")", unknownPrefixes(names[dirs[0]]), nil
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
	return unknownPrefixes(matches)
}

func unknownPrefixes(matches []string) []string {
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
