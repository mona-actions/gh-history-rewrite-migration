// Package filterrepo wraps the `git filter-repo` external tool.
//
// This package is library-only: it does not register any cobra commands.
// Higher-level packages (largefiles, rewriter, doctor) compose it to build
// the user-facing migration workflow.
//
// The wrapper enforces three guarantees:
//
//   - Reserved orchestrator-managed flags (--force, --analyze, --dry-run,
//     --debug) and, when the large-file strip workflow is active, the
//     path-selection family (--invert-paths, --paths-from-file, --path,
//     --paths, --path-glob, --path-regex) are rejected up-front rather than
//     silently surprising users.
//   - Callback scripts are dispatched by filename suffix; unknown suffixes
//     are hard errors and duplicate kinds are rejected.
//   - Script bodies and tokens are never logged. Only paths and command
//     arguments (already user-supplied) make it to logs.
package filterrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
)

// DefaultExecer is the production Execer that shells out via os/exec and
// streams stdout/stderr to the writers the runner provides. Mirrors the
// importer.DefaultExecer pattern so command shims don't re-roll the
// boilerplate themselves.
type DefaultExecer struct{}

// LookPath delegates to exec.LookPath.
func (DefaultExecer) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// Run executes name with args in dir, streaming stdout/stderr to the
// provided writers. ctx cancellation interrupts the child process.
func (DefaultExecer) Run(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	c.Stdout = stdout
	c.Stderr = stderr
	return c.Run()
}

// Execer abstracts process execution so callers can inject a fake in tests.
type Execer interface {
	// LookPath behaves like exec.LookPath: returns the resolved absolute
	// path to the named binary, or an error if it is not on PATH.
	LookPath(name string) (string, error)
	// Run executes name with the given args from working directory dir,
	// streaming stdout / stderr to the provided writers.
	Run(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) error
}

// Runner exposes high-level operations on top of the git filter-repo binary.
type Runner struct {
	execer Execer
	log    output.Logger
	stdout io.Writer
	bin    string // resolved path to git (filter-repo runs as a git subcommand)
}

// New constructs a Runner. The execer must be non-nil; the logger may be nil.
// Filter-repo's stdout is forwarded to os.Stdout by default; use
// WithStdout to redirect it.
func New(execer Execer, log output.Logger) *Runner {
	return &Runner{execer: execer, log: log, stdout: os.Stdout, bin: "git"}
}

// WithStdout replaces the writer that receives filter-repo's stdout. A nil
// writer reverts to os.Stdout. Returns the receiver for chaining.
func (r *Runner) WithStdout(w io.Writer) *Runner {
	if w == nil {
		w = os.Stdout
	}
	r.stdout = w
	return r
}

func (r *Runner) info(msg string) {
	if r.log != nil {
		r.log.Info(msg)
	}
}

// reservedFlags are always rejected — the orchestrator manages them itself.
var reservedFlags = map[string]string{
	"--force":   "the orchestrator sets --force internally; do not pass it via --filter-repo-flag",
	"--analyze": "--analyze is invoked internally by the large-file workflow",
	"--dry-run": "--dry-run is rejected because rewrite must actually rewrite history",
	"--debug":   "--debug conflicts with the orchestrator's verbose output",
}

// stripBlockedFlags are only rejected when --strip-large-files is active,
// because the orchestrator generates --invert-paths --paths-from-file
// itself; letting the user toggle them flips the meaning of cleanup.txt.
var stripBlockedFlags = map[string]struct{}{
	"--invert-paths":    {},
	"--paths-from-file": {},
	"--path":            {},
	"--paths":           {},
	"--path-glob":       {},
	"--path-regex":      {},
}

// callbackKinds maps filename suffix → filter-repo callback flag.
var callbackKinds = map[string]string{
	".commit-callback.py":   "--commit-callback",
	".email-callback.py":    "--email-callback",
	".blob-callback.py":     "--blob-callback",
	".filename-callback.py": "--filename-callback",
	".message-callback.py":  "--message-callback",
	".refname-callback.py":  "--refname-callback",
	".tag-callback.py":      "--tag-callback",
	".reset-callback.py":    "--reset-callback",
}

// AnalyzeResult is the parsed output of `git filter-repo --analyze`.
type AnalyzeResult struct {
	// Paths aggregates per-path size statistics across the whole history.
	// Sorted descending by Footprint(), then by Path ascending.
	Paths []PathStats
}

// PathStats holds size statistics for a single path observed in history.
type PathStats struct {
	// Path as recorded in the bare repo (may contain spaces/UTF-8).
	Path string
	// MaxDeletedUnpackedBytes is the largest unpacked size observed in
	// path-deleted-sizes.txt for this path. It approximates "max single
	// blob ever observed" but is only accurate for paths that were
	// deleted at some point. For paths never recorded as deleted, this
	// falls back to CumulativeBytes — see filter-repo's
	// blobs-shas-and-paths.txt for true per-blob granularity.
	MaxDeletedUnpackedBytes int64
	// CumulativeBytes is the total unpacked size summed across all
	// revisions of this path.
	CumulativeBytes int64
}

// Footprint returns the larger of MaxDeletedUnpackedBytes and
// CumulativeBytes. It is the canonical ranking metric used for sort
// order and threshold comparisons.
func (p PathStats) Footprint() int64 {
	if p.MaxDeletedUnpackedBytes > p.CumulativeBytes {
		return p.MaxDeletedUnpackedBytes
	}
	return p.CumulativeBytes
}

// Version returns the trimmed output of `git filter-repo --version`.
func (r *Runner) Version(ctx context.Context) (string, error) {
	if _, err := r.execer.LookPath(r.bin); err != nil {
		return "", fmt.Errorf("git not found on PATH: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if err := r.execer.Run(ctx, "", r.bin, []string{"filter-repo", "--version"}, &stdout, &stderr); err != nil {
		return "", fmt.Errorf("git filter-repo --version failed: %w (stderr=%q)", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Analyze runs `git filter-repo --analyze` from inside bareRepoPath and
// parses the resulting per-path size files. It returns aggregated stats
// suitable for threshold-based flagging.
func (r *Runner) Analyze(ctx context.Context, bareRepoPath string) (*AnalyzeResult, error) {
	if bareRepoPath == "" {
		return nil, errors.New("bareRepoPath is required")
	}
	r.info("running git filter-repo --analyze")
	var stdout, stderr bytes.Buffer
	if err := r.execer.Run(ctx, bareRepoPath, r.bin, []string{"filter-repo", "--analyze"}, &stdout, &stderr); err != nil {
		return nil, fmt.Errorf("git filter-repo --analyze failed: %w (stderr=%q)", err, stderr.String())
	}

	allSizes := filepath.Join(bareRepoPath, "filter-repo", "analysis", "path-all-sizes.txt")
	deletedSizes := filepath.Join(bareRepoPath, "filter-repo", "analysis", "path-deleted-sizes.txt")

	// path-all-sizes.txt → cumulative size per path (unpacked column).
	cumulative, err := parseSizesFile(allSizes, false)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", allSizes, err)
	}

	// path-deleted-sizes.txt → max single-observation size per path.
	// Multiple rows per path are possible; we keep the max.
	maxBlob, err := parseSizesFile(deletedSizes, true)
	if err != nil {
		// Deleted-sizes is only present when paths were deleted at some
		// point. Missing file is not fatal.
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("parse %s: %w", deletedSizes, err)
		}
		maxBlob = map[string]int64{}
	}

	result := &AnalyzeResult{}
	for path, cum := range cumulative {
		mb, ok := maxBlob[path]
		if !ok {
			// Never observed as deleted: best estimate of the max blob is
			// the cumulative (single revision implies max == cumulative;
			// multiple revisions may underestimate, but we bias safe by
			// not overstating).
			mb = cum
		}
		result.Paths = append(result.Paths, PathStats{
			Path:                    path,
			MaxDeletedUnpackedBytes: mb,
			CumulativeBytes:         cum,
		})
	}
	// Stable order: descending by Footprint, then path asc. Consumers
	// (e.g. internal/largefiles) rely on this contract and may filter
	// without re-sorting.
	sort.Slice(result.Paths, func(i, j int) bool {
		fi, fj := result.Paths[i].Footprint(), result.Paths[j].Footprint()
		if fi != fj {
			return fi > fj
		}
		return result.Paths[i].Path < result.Paths[j].Path
	})
	return result, nil
}

// parseSizesFile parses a filter-repo *-sizes.txt analysis file. The format,
// after a few header lines, is whitespace-separated columns:
//
//	Format: unpacked size, packed size, date deleted, path name
//	    12345        6789 <none>     foo/bar.zip
//
// We treat the unpacked size as the per-path size value. The path is
// everything from column 4 onward, joined with single spaces — paths may
// contain spaces but never tabs or newlines in practice.
//
// If keepMax is true, multiple rows for the same path collapse to the max
// unpacked size. Otherwise, the final occurrence wins (path-all-sizes
// emits one row per path).
func parseSizesFile(path string, keepMax bool) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	out := map[string]int64{}
	scanner := bufio.NewScanner(f)
	// Allow long lines (paths can be deep).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Skip header / format lines: anything not starting with a digit
		// after trimming whitespace is metadata.
		if trimmed[0] < '0' || trimmed[0] > '9' {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 4 {
			continue
		}
		unpacked, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			// Non-numeric first field: treat as a header we didn't
			// recognize and skip rather than fail.
			continue
		}
		// Path = everything after the first 3 fields (unpacked, packed, date).
		// Reconstruct from the original line by finding the 3rd whitespace
		// boundary, to preserve internal spacing in paths.
		pathName := extractPathColumn(trimmed)
		if pathName == "" {
			continue
		}
		if keepMax {
			if cur, ok := out[pathName]; !ok || unpacked > cur {
				out[pathName] = unpacked
			}
		} else {
			out[pathName] = unpacked
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// extractPathColumn returns column 4+ from a filter-repo sizes line where
// columns 1-3 (unpacked, packed, date) are single tokens.
func extractPathColumn(line string) string {
	// Walk past 3 whitespace-delimited tokens.
	idx := 0
	for col := 0; col < 3; col++ {
		// skip leading spaces
		for idx < len(line) && (line[idx] == ' ' || line[idx] == '\t') {
			idx++
		}
		// skip token
		for idx < len(line) && line[idx] != ' ' && line[idx] != '\t' {
			idx++
		}
	}
	// skip whitespace before path
	for idx < len(line) && (line[idx] == ' ' || line[idx] == '\t') {
		idx++
	}
	if idx >= len(line) {
		return ""
	}
	return line[idx:]
}

// ValidateUserFlags rejects flags the orchestrator reserves. When
// stripActive is true, it additionally rejects the path-selection family
// (because --invert-paths flips the meaning of orchestrator-generated
// cleanup.txt).
//
// Duplicate *-callback flags of the same kind are rejected.
//
// Each entry in flags is an individual argv token, e.g.
//
//	["--commit-callback=path/to/cb.py", "--refs", "main"]
func ValidateUserFlags(flags []string, stripActive bool) error {
	callbackSeen := map[string]struct{}{}
	skipNext := false
	for _, raw := range flags {
		if skipNext {
			skipNext = false
			continue
		}
		name := flagName(raw)
		if name == "" {
			continue
		}
		if reason, blocked := reservedFlags[name]; blocked {
			return fmt.Errorf("flag %s is reserved: %s", name, reason)
		}
		if stripActive {
			if _, blocked := stripBlockedFlags[name]; blocked {
				return fmt.Errorf("flag %s is not allowed when --strip-large-files is active: it would conflict with the orchestrator-generated cleanup.txt; either drop --strip-large-files or remove %s", name, name)
			}
		}
		if strings.HasSuffix(name, "-callback") {
			if _, dup := callbackSeen[name]; dup {
				return fmt.Errorf("duplicate %s flag: filter-repo accepts only one body per callback kind", name)
			}
			callbackSeen[name] = struct{}{}
			// Two-token form (--x-callback <body>): the body follows as its
			// own token; skip it so a flag-like body isn't validated as a flag.
			if !strings.Contains(raw, "=") {
				skipNext = true
			}
		}
	}
	return nil
}

// flagName extracts the canonical flag name from a token like "--foo=bar"
// or "--foo". Returns "" for non-flag tokens.
func flagName(tok string) string {
	if !strings.HasPrefix(tok, "-") {
		return ""
	}
	if eq := strings.IndexByte(tok, '='); eq >= 0 {
		return tok[:eq]
	}
	return tok
}

// PassthroughCallbackKinds returns the set of callback kinds (e.g.
// "--commit-callback") present as flags among raw passthrough tokens. It
// skips the body of a two-token "--x-callback <body>" form so a flag-like
// body is never misread as a callback flag.
func PassthroughCallbackKinds(flags []string) map[string]bool {
	kinds := map[string]bool{}
	skipNext := false
	for _, raw := range flags {
		if skipNext {
			skipNext = false
			continue
		}
		name := flagName(raw)
		if strings.HasSuffix(name, "-callback") {
			kinds[name] = true
			if !strings.Contains(raw, "=") {
				skipNext = true
			}
		}
	}
	return kinds
}

// redactForLog joins args for logging but redacts callback bodies.
// Any arg following a --*-callback flag, or any --*-callback=<body> arg,
// has its body replaced with <redacted>.
func redactForLog(args []string) string {
	out := make([]string, 0, len(args))
	skipNext := false
	for _, a := range args {
		if skipNext {
			out = append(out, "<redacted>")
			skipNext = false
			continue
		}
		if strings.HasPrefix(a, "--") && strings.HasSuffix(a, "-callback") {
			out = append(out, a)
			skipNext = true
			continue
		}
		if i := strings.Index(a, "-callback="); i >= 0 {
			out = append(out, a[:i+len("-callback=")]+"<redacted>")
			continue
		}
		out = append(out, a)
	}
	return strings.Join(out, " ")
}

// CombinedOpts folds the strip, callback scripts, and passthrough flags
// into one filter-repo pass so exactly one original→final commit-map results.
type CombinedOpts struct {
	// StripActive is the --strip-large-files intent; drives path-selection
	// validation even when no paths are flagged. Never derived from PathsFromFile.
	StripActive bool
	// PathsFromFile adds --invert-paths --paths-from-file; set only when a
	// strip was requested AND at least one path was flagged.
	PathsFromFile string
	// ScriptPaths are callback scripts dispatched by filename suffix;
	// bodies are attached as flag/body pairs and never logged.
	ScriptPaths []string
	// PassthroughFlags are raw filter-repo tokens from --filter-repo-flag,
	// validated via ValidateUserFlags.
	PassthroughFlags []string

	// PreRewriteScripts filter the raw fast-export stream before filter-repo
	// parses it — the only place a parser-crashing object can be repaired.
	PreRewriteScripts []string
}

// Run executes one `git filter-repo` combining strip, callback scripts, and
// passthrough flags, yielding a single original→final commit-map. Validation
// matches the helpers it replaces; script bodies are never logged.
func (r *Runner) Run(ctx context.Context, bareRepoPath string, opts CombinedOpts) error {
	if err := ValidateUserFlags(opts.PassthroughFlags, opts.StripActive); err != nil {
		return err
	}
	if len(opts.PreRewriteScripts) > 0 {
		if err := ValidatePreRewriteFlags(opts.PassthroughFlags); err != nil {
			return err
		}
	}

	args := []string{"filter-repo", "--force"}
	if opts.PathsFromFile != "" {
		args = append(args, "--invert-paths", "--paths-from-file", opts.PathsFromFile)
		r.info(fmt.Sprintf("stripping paths listed in %s", opts.PathsFromFile))
	}

	// Seed seen-kinds from passthrough callback flags so a kind supplied
	// both via --filter-repo-flag and a script is rejected.
	seen := map[string]string{}
	for name := range PassthroughCallbackKinds(opts.PassthroughFlags) {
		seen[name] = "--filter-repo-flag " + name
	}

	for _, p := range opts.ScriptPaths {
		flag, err := CallbackKindFor(p)
		if err != nil {
			return err
		}
		if other, dup := seen[flag]; dup {
			return fmt.Errorf("duplicate %s callback: %s and %s — filter-repo accepts only one body per callback kind", flag, other, p)
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read callback script %s: %w", p, err)
		}
		seen[flag] = p
		args = append(args, flag, string(body))
		r.info(fmt.Sprintf("attaching %s callback from %s", flag, p))
	}

	args = append(args, opts.PassthroughFlags...)

	if len(opts.PreRewriteScripts) > 0 {
		return r.runPreRewritePipeline(ctx, bareRepoPath, args, opts.PreRewriteScripts)
	}

	r.info(fmt.Sprintf("running git %s", redactForLog(args)))
	var stderr bytes.Buffer
	if err := r.execer.Run(ctx, bareRepoPath, r.bin, args, r.stdout, &stderr); err != nil {
		return fmt.Errorf("git filter-repo failed: %w (stderr=%q)", err, stderr.String())
	}
	return nil
}

// CallbackKindFor returns the filter-repo flag (e.g. "--commit-callback")
// associated with the script's filename suffix. Unknown suffixes return an
// error so silent fallthrough is impossible.
func CallbackKindFor(scriptPath string) (string, error) {
	base := filepath.Base(scriptPath)
	for suffix, flag := range callbackKinds {
		if strings.HasSuffix(base, suffix) {
			return flag, nil
		}
	}
	return "", fmt.Errorf("unknown callback kind for %s: filename must end with one of %s", scriptPath, knownSuffixes())
}

func knownSuffixes() string {
	suffixes := make([]string, 0, len(callbackKinds))
	for s := range callbackKinds {
		suffixes = append(suffixes, s)
	}
	sort.Strings(suffixes)
	return strings.Join(suffixes, ", ")
}

// CountCommitsRemapped returns the number of old->new SHA mappings in a
// filter-repo commit-map file.
func CountCommitsRemapped(commitMapPath string) (int, error) {
	f, err := os.Open(commitMapPath)
	if err != nil {
		return 0, fmt.Errorf("filterrepo: open commit-map: %w", err)
	}
	defer func() { _ = f.Close() }()

	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "old" && fields[1] == "new" {
			continue
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("filterrepo: read commit-map: %w", err)
	}
	return count, nil
}
