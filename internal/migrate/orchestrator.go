// Package migrate implements the end-to-end orchestration of the
// `migrate` command: doctor preflight → export → rewrite → remap →
// import. The Orchestrator owns phase sequencing, idempotency/resume
// semantics, and surfacing of intermediate results (rewrite summary,
// warnings) to the user.
//
// The package is library-only; cobra/viper wiring lives in
// cmd/migrate.go. Each phase is invoked through a minimal interface so
// tests can substitute fakes without spinning up real subprocesses or
// HTTP listeners.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/atomicfs"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/remap"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/rewriter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// DoctorRunner is the minimal preflight surface used by the orchestrator.
// *doctor.Doctor satisfies this directly.
type DoctorRunner interface {
	Run(ctx context.Context) error
}

// ExporterRunner is the minimal export-phase surface used by the
// orchestrator. *exporter.Exporter satisfies this directly.
type ExporterRunner interface {
	Run(ctx context.Context) error
}

// RewriterRunner is the minimal rewrite-phase surface used by the
// orchestrator. *rewriter.Rewriter satisfies this directly. A nil
// Result with a nil error indicates an idempotent skip.
type RewriterRunner interface {
	Run(ctx context.Context, inputs ...rewriter.Input) (*rewriter.Result, error)
}

// ImporterRunner is the minimal import-phase surface used by the
// orchestrator. *importer.Importer satisfies this directly.
type ImporterRunner interface {
	Run(ctx context.Context) error
}

// Printers bundles the user-facing output sinks the orchestrator needs.
// Defaulting to the package-level functions in internal/output keeps the
// orchestrator decoupled from pterm while still letting tests capture
// every emitted line.
type Printers struct {
	Info    func(msg string)
	Warn    func(msg string)
	Error   func(msg string)
	Success func(msg string)
	Table   func(headers []string, rows [][]string)
}

// Config carries the orchestrator-level decisions that don't belong to
// any single phase. It is intentionally tiny: gate-skip flags, TTY
// behavior, and per-phase tuning all live in the corresponding phase
// configs (rewriter.Config, importer.Config, exporter.Config) and are
// owned by the caller, who passes them in via already-constructed
// phase runners. The orchestrator deliberately does NOT re-derive that
// configuration: keeping the wiring in cmd/migrate.go means tests can
// exercise orchestration logic without re-implementing flag plumbing.
type Config struct {
	// TargetRepoURL is rendered in the final success banner. It is
	// purely cosmetic; the importer is the source of truth for what
	// was actually pushed.
	TargetRepoURL string

	// Resume controls behavior when the work-dir already contains
	// artifacts from a prior run. When false (the default), the
	// orchestrator aborts before any phase runs so users don't
	// accidentally clobber or fork a half-finished migration.
	// When true, the orchestrator proceeds and relies on each
	// phase's internal idempotency (raw archives, commit map,
	// final archives, etc.) to skip already-completed work.
	Resume bool
}

// Orchestrator chains the migration phases together. It is constructed
// once per `migrate` invocation and is not safe for concurrent use.
type Orchestrator struct {
	wd       *workdir.WorkDir
	doctor   DoctorRunner
	exporter ExporterRunner
	rewriter RewriterRunner
	remapper remap.Remapper
	importer ImporterRunner

	cfg     Config
	remapIn remap.Input
	out     Printers
}

// New constructs an Orchestrator from already-built phase runners. The
// doctor runner may be nil to skip preflight (e.g. --skip-doctor). All
// other runners are required by callers; the orchestrator does not
// silently no-op them.
//
// out fields default to no-ops if nil so library tests don't need to
// thread printers everywhere.
func New(
	wd *workdir.WorkDir,
	doctor DoctorRunner,
	exporter ExporterRunner,
	rw RewriterRunner,
	remapper remap.Remapper,
	importer ImporterRunner,
	remapIn remap.Input,
	cfg Config,
	out Printers,
) *Orchestrator {
	return &Orchestrator{
		wd:       wd,
		doctor:   doctor,
		exporter: exporter,
		rewriter: rw,
		remapper: remapper,
		importer: importer,
		cfg:      cfg,
		remapIn:  remapIn,
		out:      withDefaults(out),
	}
}

func withDefaults(p Printers) Printers {
	noop := func(string) {}
	noopTable := func([]string, [][]string) {}
	if p.Info == nil {
		p.Info = noop
	}
	if p.Warn == nil {
		p.Warn = noop
	}
	if p.Error == nil {
		p.Error = noop
	}
	if p.Success == nil {
		p.Success = noop
	}
	if p.Table == nil {
		p.Table = noopTable
	}
	return p
}

// ErrResumeRequired is returned by Run when the work-dir already
// contains artifacts and Config.Resume is false. The CLI maps this to
// a friendly "pass --resume to continue" message.
var ErrResumeRequired = errors.New(
	"migrate: work-dir already contains artifacts; pass --resume to continue or remove the directory")

// Run executes the full migration pipeline in order. It short-circuits
// on the first phase error. The rewrite summary is rendered as soon as
// the rewrite phase returns so users see what changed even if a later
// phase fails. No automatic cleanup is performed on failure: artifacts
// are preserved so the user can re-run with --resume after fixing the
// underlying issue.
func (o *Orchestrator) Run(ctx context.Context) error {
	// Resume / fresh-start gate. Phases handle finer-grained
	// idempotency themselves; this is just a safety check so we
	// don't silently mix artifacts from two unrelated migrations.
	if !o.cfg.Resume && o.workDirHasArtifacts() {
		return ErrResumeRequired
	}

	// Phase 0: doctor preflight (skippable via nil runner).
	if o.doctor != nil {
		o.out.Info("Running preflight checks (doctor)...")
		if err := o.doctor.Run(ctx); err != nil {
			return fmt.Errorf("preflight failed: %w", err)
		}
	}

	// Phase 1: export.
	o.out.Info("Phase 1/3: Exporting source repository archive...")
	if err := o.exporter.Run(ctx); err != nil {
		return fmt.Errorf("export phase failed: %w", err)
	}

	// Phase 2: rewrite + remap (Gate 1 lives inside the rewriter).
	o.out.Info("Phase 2/3: Rewriting history (filter-repo) and remapping metadata SHAs...")
	res, err := o.rewriter.Run(ctx)
	if err != nil {
		return fmt.Errorf("rewrite phase failed: %w", err)
	}
	if res != nil {
		// Render the table immediately so users see what
		// happened before any later phase fails. Result.Render
		// also surfaces res.Warnings via the Warn printer.
		res.Render(o.out.Table, o.out.Warn)
	} else {
		o.out.Info("Rewrite skipped (already complete in this work-dir).")
	}

	// Remap: only runs if filter-repo produced a commit-map (i.e., rewrites occurred).
	wd := o.wd
	if wd == nil {
		wd = o.remapIn.WorkDir
	}
	if wd != nil && wd.HasCommitMap() {
		o.out.Info("Remapping commit SHAs in metadata...")
		remapRes, err := o.remapper.Run(ctx, o.remapIn)
		if err != nil {
			return fmt.Errorf("remap phase failed (raw metadata archive %q, commit map %q): %w",
				o.remapIn.RawMetadataArchive, o.remapIn.CommitMapPath, err)
		}
		for _, warning := range remapRes.Warnings {
			o.out.Warn(warning)
		}
	} else {
		o.out.Warn("no rewrites performed; using original metadata archive")
		// Copy raw metadata archive to the expected location so importer finds it.
		raw := o.remapIn.RawMetadataArchive
		if wd != nil {
			raw = wd.RawMetadataArchive()
			final := wd.MetadataArchive()
			if _, err := os.Stat(raw); err == nil {
				if err := atomicfs.CopyFile(raw, final); err != nil {
					return fmt.Errorf("failed to copy metadata archive: %w", err)
				}
			}
		}
	}

	// Phase 3: import (Gate 2 lives inside the importer).
	o.out.Info("Phase 3/3: Importing into target organization...")
	if err := o.importer.Run(ctx); err != nil {
		return fmt.Errorf("import phase failed: %w", err)
	}

	if o.cfg.TargetRepoURL != "" {
		o.out.Success(fmt.Sprintf("Migration complete: %s", o.cfg.TargetRepoURL))
	} else {
		o.out.Success("Migration complete.")
	}
	return nil
}

// workDirHasArtifacts reports whether the work-dir already has any of
// the canonical phase outputs from a prior run. A nil work-dir returns
// false so library tests can construct an Orchestrator without touching
// disk.
func (o *Orchestrator) workDirHasArtifacts() bool {
	if o.wd == nil {
		return false
	}
	return atomicfs.IsFileComplete(o.wd.RawGitArchive()) ||
		atomicfs.IsFileComplete(o.wd.RawMetadataArchive()) ||
		o.wd.HasCommitMap() ||
		o.wd.HasGitArchive() ||
		o.wd.HasMetadataArchive()
}
