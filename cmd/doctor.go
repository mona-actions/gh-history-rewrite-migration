package cmd

import (
	"context"
	"os"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/doctor"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// doctorCmd represents the doctor command
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Run preflight checks before migration",
	Long: `Run preflight checks to verify all required dependencies and configuration
are in place before starting a migration.

Checks include:
  - git-filter-repo installation
  - gh-gei extension installation and version
  - tar availability
  - Environment variables (GH_SOURCE_PAT, GH_PAT)
  - Source API reachability
  - Work directory writability and free space`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		// Build configuration from viper/env
		cfg := doctor.Config{
			WorkDir:        viper.GetString("WORK_DIR"),
			SourceHostname: viper.GetString("SOURCE_HOSTNAME"),
			SourceToken:    os.Getenv("GH_SOURCE_PAT"),
			TargetToken:    os.Getenv("GH_PAT"),
		}

		d := doctor.New(cfg, nil)
		if err := d.Run(ctx); err != nil {
			output.Error(err.Error())
			return err
		}

		output.Success("\nAll preflight checks passed!")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
