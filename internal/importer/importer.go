// Package importer wraps the `gh gei migrate-repo` command to import a
// rewritten archive into a target GitHub.com (GHEC) organization.
//
// The importer is intentionally a thin orchestration layer: it performs
// confirmation gating, validates that the rewritten archives are present,
// looks up the gh binary, builds the gei argv, and streams the child
// process output to the parent. Personal Access Tokens are passed via
// the environment (GH_SOURCE_PAT / GH_PAT) and never via argv.
package importer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// Config holds the user-provided configuration for an import run.
type Config struct {
	// TargetOrg is the destination GitHub.com (GHEC) organization.
	TargetOrg string
	// TargetRepo is the destination repository name.
	TargetRepo string
	// SourceHostname identifies the source instance. "" or "github.com"
	// means GitHub.com; any other value is treated as a GHES hostname.
	SourceHostname string
	// Confirm bypasses the interactive confirmation gate when true.
	Confirm bool
}

// Execer abstracts the subset of os/exec used by the importer so tests
// can substitute a fake without spawning real processes.
type Execer interface {
	// LookPath behaves like exec.LookPath.
	LookPath(name string) (string, error)
	// Run runs name with args and the given environment. Implementations
	// should stream stdout/stderr to the parent process.
	Run(ctx context.Context, name string, args []string, env []string) error
}

// DefaultExecer is the production Execer that wraps os/exec and streams
// child process output to the parent's stdout/stderr.
type DefaultExecer struct{}

// LookPath delegates to exec.LookPath.
func (DefaultExecer) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// Run executes name with args, inheriting stdout/stderr from the parent
// process so users see live progress from gh gei.
func (DefaultExecer) Run(ctx context.Context, name string, args []string, env []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// Importer orchestrates an import via `gh gei migrate-repo`.
type Importer struct {
	wd     *workdir.WorkDir
	cfg    Config
	execer Execer
}

// New constructs an Importer. If execer is nil, DefaultExecer is used.
func New(wd *workdir.WorkDir, cfg Config, execer Execer) *Importer {
	if execer == nil {
		execer = DefaultExecer{}
	}
	return &Importer{wd: wd, cfg: cfg, execer: execer}
}

// Run executes the import workflow. Cheap, deterministic preconditions
// (archives present, gh binary located, PATs set) are validated before
// the interactive confirmation gate so users aren't asked to confirm a
// run that would have failed anyway.
//
// Order:
//  1. Verify rewritten archives exist.
//  2. Locate `gh` on PATH.
//  3. Validate GH_SOURCE_PAT / GH_PAT env vars are set.
//  4. Build gei migrate-repo argv (no PATs in argv).
//  5. Confirmation gate (Gate 2).
//  6. Invoke gh gei, streaming output to the parent process.
func (i *Importer) Run(ctx context.Context) error {
	// 1. Verify archives.
	if !i.wd.HasGitArchive() {
		return fmt.Errorf("git archive not found at %s; run rewrite first", i.wd.GitArchive())
	}
	if !i.wd.HasMetadataArchive() {
		return fmt.Errorf("metadata archive not found at %s; run rewrite first", i.wd.MetadataArchive())
	}

	// 2. Locate gh binary.
	ghPath, err := i.execer.LookPath("gh")
	if err != nil {
		return fmt.Errorf("gh CLI not found in PATH: %w", err)
	}

	// 3. PATs via env, never argv. Validate before any user-facing prompt.
	sourcePAT := os.Getenv("GH_SOURCE_PAT")
	targetPAT := os.Getenv("GH_PAT")
	if sourcePAT == "" {
		return fmt.Errorf("GH_SOURCE_PAT environment variable is required")
	}
	if targetPAT == "" {
		return fmt.Errorf("GH_PAT environment variable is required")
	}

	// 4. Build args (PATs are NEVER placed here).
	args := []string{
		"gei", "migrate-repo",
		"--github-target-org", i.cfg.TargetOrg,
		"--target-repo", i.cfg.TargetRepo,
		"--git-archive-path", i.wd.GitArchive(),
		"--metadata-archive-path", i.wd.MetadataArchive(),
	}
	if i.cfg.SourceHostname != "" && i.cfg.SourceHostname != "github.com" {
		args = append(args, "--ghes-api-url",
			fmt.Sprintf("https://%s/api/v3", i.cfg.SourceHostname))
	}

	// 5. Confirmation gate (after preconditions pass).
	if !i.cfg.Confirm {
		if !output.IsTerminal() {
			return fmt.Errorf("--confirm required when not running on a TTY")
		}
		prompt := fmt.Sprintf(
			"About to import rewritten archive into %s/%s. Proceed?",
			i.cfg.TargetOrg, i.cfg.TargetRepo,
		)
		ok, err := output.Confirm(prompt, false)
		if err != nil {
			return fmt.Errorf("confirmation prompt failed: %w", err)
		}
		if !ok {
			return fmt.Errorf("import aborted by user")
		}
	}

	// 6. Run gh gei. No spinner here: the child streams its own
	// progress to stdout/stderr, and a pterm spinner on the same TTY
	// would interleave with that output.
	env := buildEnv(os.Environ(), sourcePAT, targetPAT)
	output.Info("Running gh gei migrate-repo...")
	if err := i.execer.Run(ctx, ghPath, args, env); err != nil {
		return fmt.Errorf("gh gei migrate-repo failed: %w", err)
	}
	output.Success("Import complete")
	return nil
}

// buildEnv returns base with GH_SOURCE_PAT / GH_PAT set (replacing any
// existing entries with the same key) so child processes see exactly
// one definition for each.
func buildEnv(base []string, sourcePAT, targetPAT string) []string {
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		// Preserve order but drop existing PAT entries; we re-append below.
		if strings.HasPrefix(kv, "GH_SOURCE_PAT=") || strings.HasPrefix(kv, "GH_PAT=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "GH_SOURCE_PAT="+sourcePAT, "GH_PAT="+targetPAT)
	return out
}
