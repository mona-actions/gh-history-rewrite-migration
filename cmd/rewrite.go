package cmd

import (
	"context"
	"fmt"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/filterrepo"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/largefiles"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/rewriter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// rewriteCmd implements the standalone `rewrite` subcommand.
var rewriteCmd = &cobra.Command{
	Use:   "rewrite",
	Short: "Rewrite git history using filter-repo before import",
	Long: `Run git filter-repo against the bare repository extracted by 'export'.

Supports three optional, composable rewrite operations:
  - --strip-large-files  : analyze + strip files exceeding the size threshold
  - --filter-repo-script : attach user callback scripts (kind via filename suffix)
  - --filter-repo-flag   : pass through arbitrary filter-repo flags

A confirmation gate (Gate 1) prompts before stripping; bypass with --yes.
The rewritten commit-map is copied to <work-dir>/commit-map for the remap step.`,
	RunE: runRewrite,
}

func init() {
	rewriteCmd.Flags().Bool("strip-large-files", false, "Analyze the repo and strip files exceeding --large-file-threshold")
	rewriteCmd.Flags().StringSlice("filter-repo-script", nil, "Path to a user filter-repo callback script (repeatable)")
	rewriteCmd.Flags().StringSlice("filter-repo-flag", nil, "Raw filter-repo flag/arg to pass through (repeatable)")
	rewriteCmd.Flags().Bool("yes", false, "Skip the strip-confirmation prompt (Gate 1)")
	rewriteCmd.Flags().Bool("non-interactive", false, "Error rather than prompt when a gate would block")

	rootCmd.AddCommand(rewriteCmd)
}

func runRewrite(cmd *cobra.Command, _ []string) error {
	if err := checkRequiredVars("WORK_DIR"); err != nil {
		return err
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	wd, err := workdir.New(viper.GetString("WORK_DIR"))
	if err != nil {
		return fmt.Errorf("failed to initialize work directory: %w", err)
	}
	if err := wd.Lock(); err != nil {
		return err
	}
	defer func() {
		if uerr := wd.Unlock(); uerr != nil {
			output.Warn(fmt.Sprintf("failed to release work-dir lock: %v", uerr))
		}
	}()

	thresholdStr := viper.GetString("LARGE_FILE_THRESHOLD")
	threshold, err := largefiles.ParseThreshold(thresholdStr)
	if err != nil {
		return fmt.Errorf("invalid --large-file-threshold: %w", err)
	}

	stripFlag, _ := cmd.Flags().GetBool("strip-large-files")
	scripts, _ := cmd.Flags().GetStringSlice("filter-repo-script")
	flags, _ := cmd.Flags().GetStringSlice("filter-repo-flag")
	yes, _ := cmd.Flags().GetBool("yes")
	nonInteractive, _ := cmd.Flags().GetBool("non-interactive")

	log := output.PackageLogger{}
	runner := filterrepo.New(filterrepo.DefaultExecer{}, log)
	analyzer := largefiles.New(runner, log, threshold)

	cfg := rewriter.Config{
		StripLargeFiles:    stripFlag,
		LargeFileThreshold: threshold,
		FilterRepoScripts:  scripts,
		FilterRepoFlags:    flags,
		SkipConfirm:        yes,
		NonInteractive:     nonInteractive,
	}

	rw := rewriter.New(wd, runner, analyzer, log, cfg)
	res, err := rw.Run(ctx)
	if err != nil {
		return err
	}
	if res != nil {
		res.Render(output.Table, output.Warn)
	}
	return nil
}
