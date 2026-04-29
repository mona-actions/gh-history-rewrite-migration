package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "gh-history-rewrite-migration",
	Short: "Orchestrate GitHub repository migrations with history rewriting",
	Long: `A GitHub CLI extension for orchestrating repository migrations with history rewriting.

This tool helps migrate repositories between GitHub organizations while optionally
rewriting history to remove large files, apply custom filters, and optimize the
repository before import.`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	// Global flags available to all subcommands
	rootCmd.PersistentFlags().String("work-dir", "", "Working directory for migration artifacts (default: ./work)")
	rootCmd.PersistentFlags().String("org", "", "Source organization")
	rootCmd.PersistentFlags().String("repo", "", "Source repository name")
	rootCmd.PersistentFlags().String("target-org", "", "Target organization")
	rootCmd.PersistentFlags().String("source-hostname", "github.com", "Source GitHub hostname (for GHES support)")
	rootCmd.PersistentFlags().String("large-file-threshold", "400M", "Threshold for large file detection")
	rootCmd.PersistentFlags().Bool("no-color", false, "Disable colored output")

	// Set environment variable prefix: GHHRM (GitHub History Rewrite Migration)
	viper.SetEnvPrefix("GHHRM")
	viper.AutomaticEnv()

	// Bind flags to Viper
	// Priority: Flag value > Environment variable > Default value
	viper.BindPFlag("WORK_DIR", rootCmd.PersistentFlags().Lookup("work-dir"))
	viper.BindPFlag("ORG", rootCmd.PersistentFlags().Lookup("org"))
	viper.BindPFlag("REPO", rootCmd.PersistentFlags().Lookup("repo"))
	viper.BindPFlag("TARGET_ORG", rootCmd.PersistentFlags().Lookup("target-org"))
	viper.BindPFlag("SOURCE_HOSTNAME", rootCmd.PersistentFlags().Lookup("source-hostname"))
	viper.BindPFlag("LARGE_FILE_THRESHOLD", rootCmd.PersistentFlags().Lookup("large-file-threshold"))
	viper.BindPFlag("NO_COLOR", rootCmd.PersistentFlags().Lookup("no-color"))

	// Set default values
	viper.SetDefault("WORK_DIR", "./work")
	viper.SetDefault("SOURCE_HOSTNAME", "github.com")
	viper.SetDefault("LARGE_FILE_THRESHOLD", "400M")

	// Bind environment variables explicitly for PAT authentication
	viper.BindEnv("GH_SOURCE_PAT")
	viper.BindEnv("GH_PAT")
}

// checkRequiredVars validates that all required configuration values are set
func checkRequiredVars(required ...string) error {
	for _, key := range required {
		if viper.GetString(key) == "" {
			return fmt.Errorf("%s is required", key)
		}
	}
	return nil
}
