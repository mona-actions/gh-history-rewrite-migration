package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/api"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/exporter"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// exportCmd implements the standalone `export` subcommand.
var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export a repository archive from a source organization",
	Long: `Export a single repository as a combined migration archive using the
GitHub REST migrations API. The archive is downloaded to the work directory
and extracted in place; the bare repository is then ready for downstream
history rewriting.`,
	RunE: runExport,
}

func init() {
	exportCmd.Flags().Bool("exclude-releases", false, "Exclude release assets from the archive")
	exportCmd.Flags().Bool("exclude-metadata", false, "Exclude issue/PR metadata from the archive")
	exportCmd.Flags().Bool("exclude-attachments", false, "Exclude issue/PR attachments from the archive")
	exportCmd.Flags().Bool("lock-repositories", false, "Lock source repositories during migration")
	exportCmd.Flags().String("export-mode", "two", "Export mode: two or combined")

	viper.BindPFlag("EXCLUDE_RELEASES", exportCmd.Flags().Lookup("exclude-releases"))
	viper.BindPFlag("EXCLUDE_METADATA", exportCmd.Flags().Lookup("exclude-metadata"))
	viper.BindPFlag("EXCLUDE_ATTACHMENTS", exportCmd.Flags().Lookup("exclude-attachments"))
	viper.BindPFlag("LOCK_REPOSITORIES", exportCmd.Flags().Lookup("lock-repositories"))
	viper.BindPFlag("EXPORT_MODE", exportCmd.Flags().Lookup("export-mode"))

	rootCmd.AddCommand(exportCmd)
}

// warnInertExportFlags surfaces a one-line warning for any export-side
// flag that the underlying go-github v62 MigrationOptions does not yet
// support. It reads from BOTH the cobra flag (covers explicit CLI use,
// including from `migrate` whose flags aren't viper-bound) and from
// viper (covers env-var-only use like EXCLUDE_RELEASES=true). When the
// SDK gains these fields, plumb them into exporter.Config and delete
// this helper.
//
// Defined here (not in internal/exporter) because the warning is a
// pure CLI-UX concern: the library is silent about flags it doesn't
// know about. Callable from both `export` and `migrate` to keep the
// behavior identical.
func warnInertExportFlags(cmd *cobra.Command) {
	check := func(flag, viperKey, msg string) {
		if f := cmd.Flag(flag); f != nil {
			if v, _ := cmd.Flags().GetBool(flag); v {
				output.Warn(msg)
				return
			}
		}
		if viperKey != "" && viper.GetBool(viperKey) {
			output.Warn(msg)
		}
	}
	check("exclude-releases", "EXCLUDE_RELEASES",
		"--exclude-releases is currently inert (go-github v62 does not expose this option)")
	check("exclude-metadata", "EXCLUDE_METADATA",
		"--exclude-metadata is currently inert (go-github v62 does not expose this option)")
}

func exportModeValue(cmd *cobra.Command) string {
	if f := cmd.Flags().Lookup("export-mode"); f != nil && f.Changed {
		return f.Value.String()
	}
	return viper.GetString("EXPORT_MODE")
}

func runExport(cmd *cobra.Command, _ []string) error {
	if err := checkRequiredVars("ORG", "REPO", "WORK_DIR", "SOURCE_HOSTNAME"); err != nil {
		return err
	}

	token := os.Getenv("GH_SOURCE_PAT")
	if token == "" {
		return fmt.Errorf("GH_SOURCE_PAT environment variable is required")
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
		return fmt.Errorf("failed to lock work directory: %w", err)
	}
	defer func() {
		if err := wd.Unlock(); err != nil {
			output.Warn(fmt.Sprintf("failed to unlock work directory: %v", err))
		}
	}()

	apiClient, err := api.New(ctx, viper.GetString("SOURCE_HOSTNAME"), token)
	if err != nil {
		return fmt.Errorf("failed to construct API client: %w", err)
	}

	warnInertExportFlags(cmd)

	mode, err := exporter.ParseMode(exportModeValue(cmd))
	if err != nil {
		return err
	}
	exp := exporter.New(apiClient, wd, exporter.Config{
		Org:                viper.GetString("ORG"),
		Repo:               viper.GetString("REPO"),
		Mode:               mode,
		LockRepositories:   viper.GetBool("LOCK_REPOSITORIES"),
		ExcludeReleases:    viper.GetBool("EXCLUDE_RELEASES"),
		ExcludeAttachments: viper.GetBool("EXCLUDE_ATTACHMENTS"),
	})

	return exp.Run(ctx)
}
