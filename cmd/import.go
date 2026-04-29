package cmd

import (
	"context"
	"fmt"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/importer"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// importCmd imports a rewritten archive into the target GHEC org via gh gei.
var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Import the rewritten archive into the target organization",
	Long: `Import a previously rewritten git+metadata archive pair into the target
GitHub.com (GHEC) organization using the hidden gh gei migrate-repo flags.

Requires GH_SOURCE_PAT and GH_PAT environment variables. Prompts for
confirmation unless --confirm is supplied; --confirm is required when
running without a TTY.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		if err := checkRequiredVars("WORK_DIR", "TARGET_ORG"); err != nil {
			return err
		}

		// target-repo defaults to --repo if not explicitly set. Read
		// directly from the local flag (not viper) so we don't pollute
		// the global key namespace and don't accidentally let
		// GHHRM_TARGET_REPO env-override a subcommand-only flag.
		targetRepo, _ := cmd.Flags().GetString("target-repo")
		if targetRepo == "" {
			targetRepo = viper.GetString("REPO")
		}
		if targetRepo == "" {
			return fmt.Errorf("--target-repo (or --repo) is required")
		}

		// --confirm is a CLI-only safety gate — never read from env.
		confirm, _ := cmd.Flags().GetBool("confirm")

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

		cfg := importer.Config{
			TargetOrg:      viper.GetString("TARGET_ORG"),
			TargetRepo:     targetRepo,
			SourceHostname: viper.GetString("SOURCE_HOSTNAME"),
			Confirm:        confirm,
		}

		imp := importer.New(wd, cfg, nil)
		// Cobra prints errors returned from RunE; don't double-print.
		return imp.Run(ctx)
	},
}

func init() {
	importCmd.Flags().String("target-repo", "", "Target repository name (defaults to --repo)")
	importCmd.Flags().Bool("confirm", false, "Skip the interactive confirmation prompt")

	rootCmd.AddCommand(importCmd)
}
