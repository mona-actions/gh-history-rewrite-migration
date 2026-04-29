package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/api"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/doctor"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/exporter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/filterrepo"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/importer"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/largefiles"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/migrate"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/remap"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/rewriter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// remapLogger adapts internal/output's Info/Warn package functions to
// the remap.Logger interface. Defined here (rather than in remap) so
// the remap package stays free of any output dependency.
type remapLogger struct{}

func (remapLogger) Info(m string) { output.Info(m) }
func (remapLogger) Warn(m string) { output.Warn(m) }

// migrateCmd is the PRIMARY user-facing command. It chains the four
// migration phases (export → rewrite → remap → import) plus an optional
// preflight (doctor) into a single end-to-end run.
//
// The standalone subcommands (`export`, `rewrite`, `import`) remain
// available for advanced workflows where users want to re-run an
// individual phase. `migrate` is the recommended entry point for the
// 95% case.
var migrateCmd = &cobra.Command{
	Use:     "migrate",
	GroupID: "primary",
	Short:   "Run the full migration end-to-end (export → rewrite → remap → import)",
	Long: `Migrate a single repository between GitHub organizations, optionally
rewriting history along the way.

Phases:
  1. (preflight)  doctor checks for git-filter-repo, gh-gei, PATs, ...
  2. export       download a combined migration archive from the source org
  3. rewrite      run git filter-repo (strip large files / scripts / flags)
                  Gate 1: confirms before strip; bypass with --yes
  4. remap        rewrite SHAs in metadata JSONs (currently stub - pending
                  upstream gh-commit-remap release; aborts if invoked)
  5. import       push the rewritten archive into the target org via gh gei
                  Gate 2: confirms before import; bypass with --confirm

Required env vars: GH_SOURCE_PAT (read access to the source repo),
GH_PAT (write access to the target org).`,
	RunE: runMigrate,
}

func init() {
	// Register a "primary" group so cobra's --help renders migrate
	// at the top above the alphabetically-ordered advanced
	// subcommands. The other *Cmd values are package-level vars
	// initialized before any init() runs, so reaching across files
	// to assign GroupID here is safe regardless of init() order.
	rootCmd.AddGroup(&cobra.Group{ID: "primary", Title: "Primary command:"})
	rootCmd.AddGroup(&cobra.Group{ID: "advanced", Title: "Advanced (per-phase) commands:"})
	doctorCmd.GroupID = "advanced"
	exportCmd.GroupID = "advanced"
	rewriteCmd.GroupID = "advanced"
	importCmd.GroupID = "advanced"

	// Phase: import target.
	migrateCmd.Flags().String("target-repo", "", "Target repository name (defaults to --repo)")

	// Phase: export.
	migrateCmd.Flags().Bool("exclude-releases", false, "Exclude release assets from the archive")
	migrateCmd.Flags().Bool("exclude-metadata", false, "Exclude issue/PR metadata from the archive")
	migrateCmd.Flags().Bool("exclude-attachments", false, "Exclude issue/PR attachments from the archive")
	migrateCmd.Flags().Bool("lock-repositories", false, "Lock source repositories during migration")

	// Phase: rewrite.
	migrateCmd.Flags().Bool("strip-large-files", false, "Analyze the repo and strip files exceeding --large-file-threshold")
	migrateCmd.Flags().StringSlice("filter-repo-script", nil, "Path to a user filter-repo callback script (repeatable)")
	migrateCmd.Flags().StringSlice("filter-repo-flag", nil, "Raw filter-repo flag/arg to pass through (repeatable)")

	// Gates.
	migrateCmd.Flags().Bool("yes", false, "Skip the strip-confirmation prompt (Gate 1)")
	migrateCmd.Flags().Bool("confirm", false, "Skip the import-confirmation prompt (Gate 2)")
	migrateCmd.Flags().Bool("non-interactive", false, "Error rather than prompt when a gate would block")

	// Lifecycle.
	migrateCmd.Flags().Bool("resume", false, "Resume an existing work-dir instead of aborting on prior artifacts")
	migrateCmd.Flags().Bool("skip-doctor", false, "Skip the doctor preflight checks (not recommended)")
	_ = migrateCmd.Flags().MarkHidden("skip-doctor")

	rootCmd.AddCommand(migrateCmd)
}

// runMigrate is the cobra RunE for `migrate`. It performs the flag
// resolution that subcommand-only flags (target-repo, confirm) need,
// constructs each phase, and hands them to the orchestrator.
func runMigrate(cmd *cobra.Command, _ []string) error {
	if err := checkRequiredVars("ORG", "REPO", "TARGET_ORG", "WORK_DIR", "SOURCE_HOSTNAME"); err != nil {
		return err
	}

	sourceToken := os.Getenv("GH_SOURCE_PAT")
	if sourceToken == "" {
		return fmt.Errorf("GH_SOURCE_PAT environment variable is required")
	}
	// GH_PAT is checked again inside the importer; we deliberately
	// don't fail-fast here because doctor is a more user-friendly
	// place to surface this.

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// target-repo defaults to --repo, mirroring import.go.
	targetRepo, _ := cmd.Flags().GetString("target-repo")
	if targetRepo == "" {
		targetRepo = viper.GetString("REPO")
	}
	if targetRepo == "" {
		return fmt.Errorf("--target-repo (or --repo) is required")
	}

	// Local CLI-only flags. Gate skip-flags must NEVER come from
	// env vars; they are interactive-safety toggles.
	yes, _ := cmd.Flags().GetBool("yes")
	confirm, _ := cmd.Flags().GetBool("confirm")
	nonInteractive, _ := cmd.Flags().GetBool("non-interactive")
	resume, _ := cmd.Flags().GetBool("resume")
	skipDoctor, _ := cmd.Flags().GetBool("skip-doctor")
	stripLarge, _ := cmd.Flags().GetBool("strip-large-files")
	scripts, _ := cmd.Flags().GetStringSlice("filter-repo-script")
	frFlags, _ := cmd.Flags().GetStringSlice("filter-repo-flag")

	// Inert-flag warnings — shared helper so `migrate` and `export`
	// stay in lockstep on which flags currently no-op upstream.
	warnInertExportFlags(cmd)

	// Work-dir + lock.
	wd, err := workdir.New(viper.GetString("WORK_DIR"))
	if err != nil {
		return fmt.Errorf("failed to initialize work directory: %w", err)
	}
	output.Info(fmt.Sprintf("Work directory: %s", viper.GetString("WORK_DIR")))
	if err := wd.Lock(); err != nil {
		return fmt.Errorf("failed to lock work directory: %w", err)
	}
	defer func() {
		if uerr := wd.Unlock(); uerr != nil {
			output.Warn(fmt.Sprintf("failed to release work-dir lock: %v", uerr))
		}
	}()

	// Phase 0: doctor.
	var doctorRunner migrate.DoctorRunner
	if !skipDoctor {
		doctorRunner = doctor.New(doctor.Config{
			WorkDir:        viper.GetString("WORK_DIR"),
			SourceHostname: viper.GetString("SOURCE_HOSTNAME"),
			SourceToken:    sourceToken,
			TargetToken:    os.Getenv("GH_PAT"),
		}, nil)
	}

	// Phase 1: exporter.
	apiClient, err := api.New(ctx, viper.GetString("SOURCE_HOSTNAME"), sourceToken)
	if err != nil {
		return fmt.Errorf("failed to construct API client: %w", err)
	}
	exclAttachments, _ := cmd.Flags().GetBool("exclude-attachments")
	lockRepos, _ := cmd.Flags().GetBool("lock-repositories")
	exp := exporter.New(apiClient, wd, exporter.Config{
		Org:                viper.GetString("ORG"),
		Repo:               viper.GetString("REPO"),
		LockRepositories:   lockRepos,
		ExcludeAttachments: exclAttachments,
	})

	// Phase 2: rewriter.
	threshold, err := largefiles.ParseThreshold(viper.GetString("LARGE_FILE_THRESHOLD"))
	if err != nil {
		return fmt.Errorf("invalid --large-file-threshold: %w", err)
	}
	log := outputLogger{} // defined in cmd/rewrite.go
	frRunner := filterrepo.New(filterrepo.DefaultExecer{}, log)
	analyzer := largefiles.New(frRunner, log, threshold)
	rw := rewriter.New(wd, frRunner, analyzer, log, rewriter.Config{
		StripLargeFiles:    stripLarge,
		LargeFileThreshold: threshold,
		FilterRepoScripts:  scripts,
		FilterRepoFlags:    frFlags,
		SkipConfirm:        yes,
		NonInteractive:     nonInteractive,
	})

	// Phase 3: remapper (stub for now; swap to a real impl once
	// gh-commit-remap publishes pkg/).
	remapper := remap.NewStub(remapLogger{})

	// Phase 4: importer.
	imp := importer.New(wd, importer.Config{
		TargetOrg:      viper.GetString("TARGET_ORG"),
		TargetRepo:     targetRepo,
		SourceHostname: viper.GetString("SOURCE_HOSTNAME"),
		Confirm:        confirm,
	}, nil)

	// Construct remap input. BareRepoPath isn't yet known at
	// orchestration time (extraction happens during export); the
	// stub doesn't read it, and the real remapper will resolve it
	// itself via wd.BareRepoPath() so we leave it empty here.
	remapIn := remap.Input{
		WorkDir:       wd,
		CommitMapPath: wd.CommitMap(),
		ExtractedDir:  wd.Extracted(),
	}

	// NonInteractive is already plumbed into rewriter.Config above;
	// importer enforces TTY requirements via its own --confirm gate
	// and currently has no NonInteractive field, so no further
	// wiring is needed here.

	orch := migrate.New(
		wd, doctorRunner, exp, rw, remapper, imp,
		remapIn,
		migrate.Config{
			TargetRepoURL: fmt.Sprintf("https://github.com/%s/%s",
				viper.GetString("TARGET_ORG"), targetRepo),
			Resume: resume,
		},
		migrate.Printers{
			Info:    output.Info,
			Warn:    output.Warn,
			Error:   output.Error,
			Success: output.Success,
			Table:   output.Table,
		},
	)

	return orch.Run(ctx)
}
