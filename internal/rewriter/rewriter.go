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

	"github.com/mona-actions/gh-commit-remap/pkg/archive"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/atomicfs"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/filterrepo"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/largefiles"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

const defaultLargeFileThreshold = 400 * 1024 * 1024

// Input is the v2, mode-unaware rewrite contract. The rewriter derives all
// artifact locations from WorkDir instead of accepting mode-specific paths.
type Input struct {
	WorkDir         *workdir.WorkDir
	LargeFileThresh int64
	UserCallbacks   []string

	// Existing user-facing flags preserved from the v1 config.
	StripLargeFiles   bool
	FilterRepoScripts []string
	FilterRepoFlags   []string
	SkipConfirm       bool
	NonInteractive    bool
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
// filterrepo internals. All mutating filter-repo work flows through a
// single Run call so exactly one commit-map is produced.
type runnerIface interface {
	Run(ctx context.Context, bareRepoPath string, opts filterrepo.CombinedOpts) error
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
	log      output.Logger
	cfg      Config

	// Injectable for tests.
	confirm confirmFn
	isTTY   func() bool
}

// New constructs a Rewriter wired to production filterrepo / largefiles
// implementations. The logger may be nil.
func New(wd *workdir.WorkDir, runner *filterrepo.Runner, analyzer *largefiles.Analyzer, log output.Logger, cfg Config) *Rewriter {
	return newWithDeps(wd, runner, analyzer, log, cfg)
}

// newWithDeps is the shared constructor. Tests pass interface stubs;
// New() passes the concrete production types (which implement the
// interfaces directly).
func newWithDeps(wd *workdir.WorkDir, runner runnerIface, analyzer analyzerIface, log output.Logger, cfg Config) *Rewriter {
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

// Run executes the v2 mode-unaware rewrite pipeline. New callers pass an
// Input explicitly; legacy callers constructed with New may omit it, in which
// case the constructor WorkDir/Config are used.
func (r *Rewriter) Run(ctx context.Context, inputs ...Input) (*Result, error) {
	in, err := r.resolveInput(inputs)
	if err != nil {
		return nil, err
	}
	r.wd = in.WorkDir
	r.cfg = Config{
		StripLargeFiles:    in.StripLargeFiles,
		LargeFileThreshold: in.LargeFileThresh,
		FilterRepoScripts:  append([]string(nil), in.FilterRepoScripts...),
		FilterRepoFlags:    append([]string(nil), in.FilterRepoFlags...),
		SkipConfirm:        in.SkipConfirm,
		NonInteractive:     in.NonInteractive,
	}

	rawGit := in.WorkDir.RawGitArchive()
	extractDir := in.WorkDir.GitExtractedDir()
	finalArchive := in.WorkDir.GitArchive()

	if atomicfs.IsFileComplete(finalArchive) {
		r.info("rewritten git archive already exists; skipping")
		return r.cachedResult(), nil
	}

	if !atomicfs.IsDirComplete(extractDir) {
		if err := os.RemoveAll(extractDir); err != nil {
			return nil, fmt.Errorf("prepare git extraction dir: %w", err)
		}
		if _, err := archive.UnTar(rawGit, extractDir); err != nil {
			return nil, fmt.Errorf("extract raw git archive %s: %w", rawGit, err)
		}
		if err := atomicfs.MarkDirComplete(extractDir); err != nil {
			return nil, fmt.Errorf("mark git extraction complete: %w", err)
		}
	}

	bareRepoPath, err := workdir.FindBareRepo(extractDir)
	if err != nil {
		return nil, wrapFindBareRepoError(extractDir, err)
	}
	bareCommitMap := filepath.Join(bareRepoPath, "filter-repo", "commit-map")

	if len(r.cfg.FilterRepoFlags) > 0 {
		if err := filterrepo.ValidateUserFlags(r.cfg.FilterRepoFlags, r.cfg.StripLargeFiles); err != nil {
			return nil, err
		}
	}
	if err := r.validateScripts(); err != nil {
		return nil, err
	}

	result := &Result{}

	var pathsFromFile string
	if r.cfg.StripLargeFiles {
		stripReady, err := r.prepareStrip(ctx, bareRepoPath, result)
		if err != nil {
			return nil, err
		}
		if stripReady {
			pathsFromFile = r.wd.CleanupTxt()
		}
	}

	rewriteRan := pathsFromFile != "" || len(r.cfg.FilterRepoScripts) > 0 || len(r.cfg.FilterRepoFlags) > 0
	if rewriteRan {
		opts := filterrepo.CombinedOpts{
			StripActive:      r.cfg.StripLargeFiles,
			PathsFromFile:    pathsFromFile,
			ScriptPaths:      r.cfg.FilterRepoScripts,
			PassthroughFlags: r.cfg.FilterRepoFlags,
		}
		if err := r.runner.Run(ctx, bareRepoPath, opts); err != nil {
			return nil, fmt.Errorf("filter-repo rewrite: %w", err)
		}
		if pathsFromFile != "" {
			result.StripPerformed = true
		}
		for _, p := range r.cfg.FilterRepoScripts {
			result.ScriptsRun = append(result.ScriptsRun, filepath.Base(p))
		}
		if len(r.cfg.FilterRepoFlags) > 0 {
			result.UserFlagsApplied = sanitizeUserFlags(r.cfg.FilterRepoFlags)
		}

		w := "git filter-repo strips GPG signatures; rewritten commits will be unsigned."
		result.Warnings = append(result.Warnings, w)
		r.warn(w)
	}

	if w := r.lfsWarning(bareRepoPath); w != "" {
		result.Warnings = append(result.Warnings, w)
		r.warn(w)
	}

	if rewriteRan {
		if w := r.handoffCommitMap(bareCommitMap); w != "" {
			result.Warnings = append(result.Warnings, w)
			r.warn(w)
		}
		if r.wd.HasCommitMap() {
			if n, err := filterrepo.CountCommitsRemapped(r.wd.CommitMap()); err == nil {
				result.CommitsRemapped = n
			}
		}
	}

	if err := atomicfs.WriteFileAtomicPath(finalArchive, func(partialPath string) error {
		return archive.ReTarDir(extractDir, partialPath)
	}); err != nil {
		return nil, fmt.Errorf("write rewritten git archive: %w", err)
	}

	return result, nil
}

func (r *Rewriter) resolveInput(inputs []Input) (Input, error) {
	if len(inputs) > 1 {
		return Input{}, errors.New("rewriter: Run accepts at most one Input")
	}
	if len(inputs) == 1 {
		in := inputs[0]
		if in.WorkDir == nil {
			return Input{}, errors.New("rewriter: WorkDir is nil")
		}
		if in.LargeFileThresh <= 0 {
			in.LargeFileThresh = defaultLargeFileThreshold
		}
		if len(in.UserCallbacks) > 0 {
			in.FilterRepoFlags = append(callbackFlags(in.UserCallbacks), in.FilterRepoFlags...)
		}
		return in, nil
	}
	if r.wd == nil {
		return Input{}, errors.New("rewriter: WorkDir is nil")
	}
	threshold := r.cfg.LargeFileThreshold
	if threshold <= 0 {
		threshold = defaultLargeFileThreshold
	}
	return Input{
		WorkDir:           r.wd,
		LargeFileThresh:   threshold,
		StripLargeFiles:   r.cfg.StripLargeFiles,
		FilterRepoScripts: append([]string(nil), r.cfg.FilterRepoScripts...),
		FilterRepoFlags:   append([]string(nil), r.cfg.FilterRepoFlags...),
		SkipConfirm:       r.cfg.SkipConfirm,
		NonInteractive:    r.cfg.NonInteractive,
	}, nil
}

func callbackFlags(callbacks []string) []string {
	flags := make([]string, 0, len(callbacks))
	for _, cb := range callbacks {
		flags = append(flags, "--commit-callback="+cb)
	}
	return flags
}

func (r *Rewriter) cachedResult() *Result {
	res := &Result{}
	if r.wd != nil && r.wd.HasCommitMap() {
		if n, err := filterrepo.CountCommitsRemapped(r.wd.CommitMap()); err == nil {
			res.CommitsRemapped = n
		}
	}
	return res
}

func wrapFindBareRepoError(root string, err error) error {
	switch {
	case errors.Is(err, workdir.ErrMultipleBareRepos):
		return fmt.Errorf("multi-repo migrations are not supported: extracted git archive contains multiple .git directories under %s; please migrate one repo at a time: %w", root, err)
	case errors.Is(err, workdir.ErrNoBareRepo):
		return fmt.Errorf("no .git directory found under %s; archive may be corrupt or empty: %w", root, err)
	default:
		return err
	}
}

// prepareStrip runs the read-only large-file analysis, writes cleanup.txt,
// prints the flagged table, and performs the Gate-1 confirmation. It does
// NOT execute filter-repo — the actual strip is folded into the single
// Run invocation. It populates the strip-related Result fields
// (paths, bytes) but leaves StripPerformed for the caller to set only after
// the unified rewrite succeeds. Returns true when there are flagged paths to
// strip (i.e. cleanup.txt is ready to feed Run).
func (r *Rewriter) prepareStrip(ctx context.Context, bareRepoPath string, result *Result) (bool, error) {
	report, err := r.analyzer.Analyze(ctx, bareRepoPath)
	if err != nil {
		return false, fmt.Errorf("analyze large files: %w", err)
	}
	if len(report.Flagged) == 0 {
		r.info("no files exceed threshold; nothing to strip")
		return false, nil
	}
	if err := report.WriteCleanupFile(r.wd.CleanupTxt()); err != nil {
		return false, fmt.Errorf("write cleanup.txt: %w", err)
	}
	r.printFlaggedTable(report)

	// Gate 1.
	if !r.cfg.SkipConfirm {
		if r.cfg.NonInteractive {
			return false, errors.New("--yes required to strip files when --non-interactive is set")
		}
		if !r.isTTY() {
			return false, errors.New("--yes required to strip files when not running on a TTY")
		}
		prompt := fmt.Sprintf(
			"Strip these %d paths from history? This rewrites all commits.",
			len(report.Flagged),
		)
		ok, err := r.confirm(prompt, false)
		if err != nil {
			return false, fmt.Errorf("confirmation prompt failed: %w", err)
		}
		if !ok {
			return false, errors.New("rewrite aborted by user")
		}
	}

	for _, f := range report.Flagged {
		result.PathsStripped = append(result.PathsStripped, f.Path)
		fp := f.Footprint()
		result.BytesFreed += fp
		if fp > result.LargestStripped {
			result.LargestStripped = fp
		}
	}
	return true, nil
}

func (r *Rewriter) validateScripts() error {
	// Seed seen-kinds from passthrough callback flags so a kind supplied
	// both via a --filter-repo-script and a --filter-repo-flag is caught
	// early (before analyze/Gate-1), matching Run's guard.
	seen := map[string]string{}
	for _, raw := range r.cfg.FilterRepoFlags {
		name := raw
		if i := strings.IndexByte(raw, '='); i >= 0 {
			name = raw[:i]
		}
		if strings.HasPrefix(name, "-") && strings.HasSuffix(name, "-callback") {
			seen[name] = "--filter-repo-flag " + name
		}
	}
	for _, p := range r.cfg.FilterRepoScripts {
		kind, err := filterrepo.CallbackKindFor(p)
		if err != nil {
			return err
		}
		if other, dup := seen[kind]; dup {
			return fmt.Errorf("duplicate %s callback: %s and %s", kind, other, p)
		}
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("callback script %s: %w", p, err)
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
	extractRoot := r.wd.GitExtractedDir()
	candidates := []string{
		filepath.Join(bareRepoPath, "lfs", "objects"),
		filepath.Join(extractRoot, "git-lfs"),
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
	if err := atomicfs.WriteFileAtomic(dst, func(w io.Writer) error {
		in, err := os.Open(srcCommitMap)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		_, err = io.Copy(w, in)
		return err
	}); err != nil {
		return fmt.Sprintf("failed to copy commit-map to %s: %v", dst, err)
	}
	return ""
}

// sanitizeUserFlags returns the argv tokens with any embedded callback
// body redacted. We never log or surface user-supplied script bodies.
// Both forms are handled: the inline "--x-callback=<body>" and the
// two-token "--x-callback <body>" (where the body is the following token).
func sanitizeUserFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	skipNext := false
	for _, t := range flags {
		if skipNext {
			out = append(out, "<redacted>")
			skipNext = false
			continue
		}
		if strings.HasPrefix(t, "--") && strings.HasSuffix(t, "-callback") {
			out = append(out, t)
			skipNext = true
			continue
		}
		if i := strings.IndexByte(t, '='); i >= 0 {
			name := t[:i]
			if strings.HasPrefix(name, "--") && strings.HasSuffix(name, "-callback") {
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
