// Package rewriter orchestrates the history-rewrite phase of the
// migration: large-file analysis & strip, user-supplied callback scripts,
// and arbitrary filter-repo passthrough flags. It is the only package that
// composes filterrepo + largefiles + workdir into a single Run() pipeline.
//
// rewriter is library-only — it registers no cobra commands. cmd/rewrite.go
// is the thin CLI shim that wires Config from flags/env and calls Run.
package rewriter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/filterrepo"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/largefiles"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/remap"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// Logger is the minimal logging surface the rewriter needs. A nil logger
// is safe — internal helpers fall back to the package-level `output`
// printers so library callers without a logger still see progress.
type Logger interface {
	Info(msg string)
	Warn(msg string)
	Error(msg string)
}

// Config holds user-supplied configuration for a rewrite run.
type Config struct {
	// StripLargeFiles enables the analyze→cleanup.txt→strip workflow.
	StripLargeFiles bool
	// LargeFileThreshold is the resolved byte threshold used by the
	// Analyzer. The caller is responsible for parsing user-supplied
	// strings via largefiles.ParseThreshold before constructing the
	// analyzer; rewriter does not re-parse.
	LargeFileThreshold int64
	// FilterRepoScripts is a list of paths to user-supplied callback
	// scripts. Their kind is dispatched via filename suffix (see
	// filterrepo.CallbackKindFor). Duplicate kinds are rejected.
	FilterRepoScripts []string
	// FilterRepoFlags is a list of raw filter-repo argv tokens to pass
	// through. They are validated against filterrepo.ValidateUserFlags.
	FilterRepoFlags []string
	// SkipConfirm bypasses the Gate-1 confirmation prompt before strip.
	SkipConfirm bool
	// NonInteractive turns any would-be prompt into a hard error so
	// scripted runs surface gating issues instead of hanging.
	NonInteractive bool
}

// Result is the structured outcome of a successful Run, suitable for
// rendering as a summary table.
type Result struct {
	StripPerformed   bool
	PathsStripped    []string
	LargestStripped  int64
	BytesFreed       int64
	ScriptsRun       []string
	UserFlagsApplied []string
	CommitsRemapped  int
	Warnings         []string
}

// runnerIface is the minimal subset of *filterrepo.Runner the rewriter
// uses. Defining it locally lets tests inject a stub without touching
// filterrepo internals.
type runnerIface interface {
	StripPaths(ctx context.Context, bareRepoPath, pathsFromFile string) error
	RunCallbackScripts(ctx context.Context, bareRepoPath string, scriptPaths []string) error
	Run(ctx context.Context, bareRepoPath string, args []string) error
}

// analyzerIface is the minimal subset of *largefiles.Analyzer the
// rewriter uses.
type analyzerIface interface {
	Analyze(ctx context.Context, bareRepoPath string) (*largefiles.Report, error)
}

// confirmFn matches output.Confirm so tests can stub the Gate-1 prompt.
type confirmFn func(prompt string, defaultYes bool) (bool, error)

// Rewriter orchestrates a single rewrite run.
type Rewriter struct {
	wd       *workdir.WorkDir
	runner   runnerIface
	analyzer analyzerIface
	log      Logger
	cfg      Config

	// Injectable for tests.
	confirm confirmFn
	isTTY   func() bool
}

// New constructs a Rewriter wired to production filterrepo / largefiles
// implementations. The logger may be nil.
func New(wd *workdir.WorkDir, runner *filterrepo.Runner, analyzer *largefiles.Analyzer, log Logger, cfg Config) *Rewriter {
	return newWithDeps(wd, runner, analyzer, log, cfg)
}

// newWithDeps is the shared constructor. Tests pass interface stubs;
// New() passes the concrete production types (which implement the
// interfaces directly).
func newWithDeps(wd *workdir.WorkDir, runner runnerIface, analyzer analyzerIface, log Logger, cfg Config) *Rewriter {
	return &Rewriter{
		wd:       wd,
		runner:   runner,
		analyzer: analyzer,
		log:      log,
		cfg:      cfg,
		confirm:  output.Confirm,
		isTTY:    output.IsTerminal,
	}
}

func (r *Rewriter) info(msg string) {
	if r.log != nil {
		r.log.Info(msg)
		return
	}
	output.Info(msg)
}

func (r *Rewriter) warn(msg string) {
	if r.log != nil {
		r.log.Warn(msg)
		return
	}
	output.Warn(msg)
}

// Run executes the rewrite pipeline. The order of side effects is:
//
//  1. Idempotency check (skip if commit-map already handed off).
//  2. Validate user flags + callback script kinds.
//  3. Optional large-file analyze → cleanup.txt → Gate 1 → strip.
//  4. Optional user callback scripts (separate filter-repo invocation).
//  5. Optional user passthrough flags (separate filter-repo invocation).
//  6. Informational warnings (GPG, LFS).
//  7. Commit-map handoff to wd.CommitMap().
//
// Returns a *Result describing what happened, or an error on the first
// fatal step. A nil Result with nil error indicates an idempotent skip.
func (r *Rewriter) Run(ctx context.Context) (*Result, error) {
	bareRepoPath, err := r.wd.BareRepoPath()
	if err != nil {
		return nil, err
	}
	bareCommitMap := filepath.Join(bareRepoPath, "filter-repo", "commit-map")

	// 1. Idempotency.
	if r.wd.HasCommitMap() {
		if _, statErr := os.Stat(bareCommitMap); statErr == nil {
			r.info("rewrite already applied; skipping")
			return nil, nil
		}
	}

	// 2a. Validate user passthrough flags.
	if len(r.cfg.FilterRepoFlags) > 0 {
		if err := filterrepo.ValidateUserFlags(r.cfg.FilterRepoFlags, r.cfg.StripLargeFiles); err != nil {
			return nil, err
		}
	}
	// 2b. Validate callback scripts.
	if err := r.validateScripts(); err != nil {
		return nil, err
	}

	result := &Result{}

	// 3. Large-files analyze + strip.
	if r.cfg.StripLargeFiles {
		if err := r.runStrip(ctx, bareRepoPath, result); err != nil {
			return nil, err
		}
	}

	// 4. User callback scripts.
	if len(r.cfg.FilterRepoScripts) > 0 {
		if err := r.runner.RunCallbackScripts(ctx, bareRepoPath, r.cfg.FilterRepoScripts); err != nil {
			return nil, fmt.Errorf("filter-repo callbacks: %w", err)
		}
		for _, p := range r.cfg.FilterRepoScripts {
			result.ScriptsRun = append(result.ScriptsRun, filepath.Base(p))
		}
	}

	// 5. User passthrough flags.
	if len(r.cfg.FilterRepoFlags) > 0 {
		if err := r.runner.Run(ctx, bareRepoPath, r.cfg.FilterRepoFlags); err != nil {
			return nil, fmt.Errorf("filter-repo passthrough: %w", err)
		}
		result.UserFlagsApplied = sanitizeUserFlags(r.cfg.FilterRepoFlags)
	}

	// 6a. GPG warning — informational; filter-repo always strips signatures.
	rewriteRan := result.StripPerformed || len(result.ScriptsRun) > 0 || len(result.UserFlagsApplied) > 0
	if rewriteRan {
		w := "git filter-repo strips GPG signatures; rewritten commits will be unsigned."
		result.Warnings = append(result.Warnings, w)
		r.warn(w)
	}

	// 6b. LFS orphan warning.
	if w := r.lfsWarning(bareRepoPath); w != "" {
		result.Warnings = append(result.Warnings, w)
		r.warn(w)
	}

	// 7. Commit-map handoff.
	if w := r.handoffCommitMap(bareCommitMap); w != "" {
		result.Warnings = append(result.Warnings, w)
		r.warn(w)
	}
	if r.wd.HasCommitMap() {
		if n, err := remap.CountCommitsRemapped(r.wd.CommitMap()); err == nil {
			result.CommitsRemapped = n
		}
	}

	return result, nil
}

func (r *Rewriter) runStrip(ctx context.Context, bareRepoPath string, result *Result) error {
	report, err := r.analyzer.Analyze(ctx, bareRepoPath)
	if err != nil {
		return fmt.Errorf("analyze large files: %w", err)
	}
	if len(report.Flagged) == 0 {
		r.info("no files exceed threshold; nothing to strip")
		return nil
	}
	if err := report.WriteCleanupFile(r.wd.CleanupTxt()); err != nil {
		return fmt.Errorf("write cleanup.txt: %w", err)
	}
	r.printFlaggedTable(report)

	// Gate 1.
	if !r.cfg.SkipConfirm {
		if r.cfg.NonInteractive {
			return errors.New("--yes required to strip files when --non-interactive is set")
		}
		if !r.isTTY() {
			return errors.New("--yes required to strip files when not running on a TTY")
		}
		prompt := fmt.Sprintf(
			"Strip these %d paths from history? This rewrites all commits.",
			len(report.Flagged),
		)
		ok, err := r.confirm(prompt, false)
		if err != nil {
			return fmt.Errorf("confirmation prompt failed: %w", err)
		}
		if !ok {
			return errors.New("rewrite aborted by user")
		}
	}

	if err := r.runner.StripPaths(ctx, bareRepoPath, r.wd.CleanupTxt()); err != nil {
		return fmt.Errorf("filter-repo strip: %w", err)
	}
	result.StripPerformed = true
	for _, f := range report.Flagged {
		result.PathsStripped = append(result.PathsStripped, f.Path)
		fp := f.Footprint()
		result.BytesFreed += fp
		if fp > result.LargestStripped {
			result.LargestStripped = fp
		}
	}
	return nil
}

func (r *Rewriter) validateScripts() error {
	seen := map[string]string{}
	for _, p := range r.cfg.FilterRepoScripts {
		kind, err := filterrepo.CallbackKindFor(p)
		if err != nil {
			return err
		}
		if other, dup := seen[kind]; dup {
			return fmt.Errorf("duplicate %s scripts: %s and %s", kind, other, p)
		}
		seen[kind] = p
	}
	return nil
}

func (r *Rewriter) printFlaggedTable(report *largefiles.Report) {
	headers := []string{"path", "max-blob", "cumulative", "reason"}
	rows := make([][]string, 0, len(report.Flagged))
	for _, f := range report.Flagged {
		rows = append(rows, []string{
			f.Path,
			output.HumanBytes(f.MaxDeletedUnpackedBytes),
			output.HumanBytes(f.CumulativeBytes),
			f.Reason,
		})
	}
	output.Table(headers, rows)
}

// lfsWarning returns a non-empty message when probable LFS objects are
// detected near the bare repo. filter-repo does not rewrite LFS pointer
// mappings; the operator must handle LFS separately.
func (r *Rewriter) lfsWarning(bareRepoPath string) string {
	candidates := []string{
		filepath.Join(bareRepoPath, "lfs", "objects"),
		filepath.Join(r.wd.Extracted(), "git-lfs"),
	}
	for _, p := range candidates {
		info, err := os.Stat(p)
		if err == nil && info.IsDir() {
			return fmt.Sprintf("LFS objects detected at %s — git filter-repo does not rewrite LFS pointer mappings; review LFS handling separately.", p)
		}
	}
	return ""
}

// handoffCommitMap copies (or renames) the filter-repo-emitted commit-map
// to wd.CommitMap(). On any failure (incl. missing source) it returns a
// human-readable warning rather than erroring — a missing commit-map only
// degrades the downstream remap step, it does not invalidate the rewrite.
func (r *Rewriter) handoffCommitMap(srcCommitMap string) string {
	info, err := os.Stat(srcCommitMap)
	if err != nil || info.IsDir() {
		return "no commit-map produced by filter-repo; remap step will be unable to translate SHAs."
	}
	dst := r.wd.CommitMap()
	if err := os.Rename(srcCommitMap, dst); err == nil {
		return ""
	}
	if err := copyFile(srcCommitMap, dst); err != nil {
		return fmt.Sprintf("failed to copy commit-map to %s: %v", dst, err)
	}
	return ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// sanitizeUserFlags returns the argv tokens with any embedded callback
// body redacted. We never log or surface user-supplied script bodies.
func sanitizeUserFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	for _, t := range flags {
		if i := strings.IndexByte(t, '='); i >= 0 {
			name := t[:i]
			if strings.HasSuffix(name, "-callback") {
				out = append(out, name+"=<redacted>")
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// HumanBytes is re-exported from the output package as a convenience
// for callers that already import rewriter (tests, callers rendering
// Result fields). New code should import internal/output directly.
func HumanBytes(n int64) string { return output.HumanBytes(n) }
