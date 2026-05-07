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
	Short:   "Run the full migration end-to-end (export → rewrite/remap → import)",
	Long: `Migrate a single repository between GitHub organizations, optionally
rewriting history along the way.

Phases:
  1. (preflight)  doctor checks for git-filter-repo, gh-gei, PATs, ...
  2. export       download migration archives from the source org
  3. rewrite      run git filter-repo (strip large files / scripts / flags)
                  then remap SHAs in metadata JSONs via gh-commit-remap
                  Gate 1: confirms before strip; bypass with --yes
  4. import       push the rewritten archives into the target org via gh gei
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
	migrateCmd.Flags().String("export-mode", "two", "Export mode: two or combined")
	viper.BindPFlag("EXPORT_MODE", migrateCmd.Flags().Lookup("export-mode"))

	// Phase: rewrite.
	migrateCmd.Flags().Bool("strip-large-files", false, "Analyze the repo and strip files exceeding --large-file-threshold")
	migrateCmd.Flags().StringSlice("filter-repo-script", nil, "Path to a user filter-repo callback script (repeatable)")
	migrateCmd.Flags().StringSlice("filter-repo-flag", nil, "Raw filter-repo flag/arg to pass through (repeatable)")

	// Phase: import.
	migrateCmd.Flags().Bool("use-github-storage", false, "Use GitHub's blob storage (required for GHES migrations)")
	migrateCmd.Flags().String("azure-storage-connection-string", "", "Azure storage connection string for blob upload")
	migrateCmd.Flags().String("aws-bucket-name", "", "AWS S3 bucket name for archive upload")
	migrateCmd.Flags().String("aws-region", "", "AWS region for the S3 bucket")
	migrateCmd.Flags().String("target-api-url", "", "Target API URL (defaults to https://api.github.com)")
	migrateCmd.Flags().String("target-repo-visibility", "", "Target repo visibility: public, private, or internal")
	migrateCmd.Flags().Bool("skip-releases", false, "Skip releases when importing")
	migrateCmd.Flags().Bool("lock-source-repo", false, "Lock source repo during import")
	migrateCmd.Flags().Bool("no-ssl-verify", false, "Disable SSL verification for GHES communication")

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

	// Import-phase flags.
	useGHStorage, _ := cmd.Flags().GetBool("use-github-storage")
	azureConn, _ := cmd.Flags().GetString("azure-storage-connection-string")
	awsBucket, _ := cmd.Flags().GetString("aws-bucket-name")
	awsRegion, _ := cmd.Flags().GetString("aws-region")
	targetAPIURL, _ := cmd.Flags().GetString("target-api-url")
	repoVisibility, _ := cmd.Flags().GetString("target-repo-visibility")
	skipReleases, _ := cmd.Flags().GetBool("skip-releases")
	lockSource, _ := cmd.Flags().GetBool("lock-source-repo")
	noSSL, _ := cmd.Flags().GetBool("no-ssl-verify")

	// Inert-flag warnings — shared helper so `migrate` and `export`
	// stay in lockstep on which flags currently no-op upstream.
	warnInertExportFlags(cmd)

	// Work-dir + lock.
	workDirPath := resolveWorkDir(cmd)
	wd, err := workdir.New(workDirPath)
	if err != nil {
		return fmt.Errorf("failed to initialize work directory: %w", err)
	}
	output.Info(fmt.Sprintf("Work directory: %s", workDirPath))
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
			WorkDir:        workDirPath,
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
	exclReleases, _ := cmd.Flags().GetBool("exclude-releases")
	lockRepos, _ := cmd.Flags().GetBool("lock-repositories")
	mode, err := exporter.ParseMode(viper.GetString("EXPORT_MODE"))
	if err != nil {
		return err
	}
	exp := exporter.New(apiClient, wd, exporter.Config{
		Org:                viper.GetString("ORG"),
		Repo:               viper.GetString("REPO"),
		Mode:               mode,
		LockRepositories:   lockRepos,
		ExcludeReleases:    exclReleases,
		ExcludeAttachments: exclAttachments,
	})

	// Phase 2: rewriter.
	threshold, err := largefiles.ParseThreshold(viper.GetString("LARGE_FILE_THRESHOLD"))
	if err != nil {
		return fmt.Errorf("invalid --large-file-threshold: %w", err)
	}
	log := output.PackageLogger{}
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

	// Phase 3: remapper.
	remapper := remap.NewReal(output.PackageLogger{})

	// Phase 4: importer.
	imp := importer.New(wd, importer.Config{
		SourceOrg:                    viper.GetString("ORG"),
		SourceRepo:                   viper.GetString("REPO"),
		TargetOrg:                    viper.GetString("TARGET_ORG"),
		TargetRepo:                   targetRepo,
		SourceHostname:               viper.GetString("SOURCE_HOSTNAME"),
		TargetAPIURL:                 targetAPIURL,
		TargetRepoVisibility:         repoVisibility,
		UseGitHubStorage:             useGHStorage,
		AzureStorageConnectionString: azureConn,
		AWSBucketName:                awsBucket,
		AWSRegion:                    awsRegion,
		SkipReleases:                 skipReleases,
		LockSourceRepo:               lockSource,
		NoSSLVerify:                  noSSL,
		Confirm:                      confirm,
	}, nil)

	remapIn := remap.Input{
		WorkDir:              wd,
		RawMetadataArchive:   wd.RawMetadataArchive(),
		CommitMapPath:        wd.CommitMap(),
		MetadataExtractedDir: wd.MetadataExtractedDir(),
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
