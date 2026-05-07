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

		if err := checkRequiredVars("WORK_DIR", "ORG", "REPO", "TARGET_ORG"); err != nil {
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
		useGHStorage, _ := cmd.Flags().GetBool("use-github-storage")
		azureConn, _ := cmd.Flags().GetString("azure-storage-connection-string")
		awsBucket, _ := cmd.Flags().GetString("aws-bucket-name")
		awsRegion, _ := cmd.Flags().GetString("aws-region")
		targetAPIURL, _ := cmd.Flags().GetString("target-api-url")
		repoVisibility, _ := cmd.Flags().GetString("target-repo-visibility")
		skipReleases, _ := cmd.Flags().GetBool("skip-releases")
		lockSource, _ := cmd.Flags().GetBool("lock-source-repo")
		noSSL, _ := cmd.Flags().GetBool("no-ssl-verify")

		wd, err := workdir.New(resolveWorkDir(cmd))
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
		}

		imp := importer.New(wd, cfg, nil)
		// Cobra prints errors returned from RunE; don't double-print.
		return imp.Run(ctx)
	},
}

func init() {
	importCmd.Flags().String("target-repo", "", "Target repository name (defaults to --repo)")
	importCmd.Flags().Bool("confirm", false, "Skip the interactive confirmation prompt")
	importCmd.Flags().Bool("use-github-storage", false, "Use GitHub's blob storage (required for GHES migrations)")
	importCmd.Flags().String("azure-storage-connection-string", "", "Azure storage connection string for blob upload")
	importCmd.Flags().String("aws-bucket-name", "", "AWS S3 bucket name for archive upload")
	importCmd.Flags().String("aws-region", "", "AWS region for the S3 bucket")
	importCmd.Flags().String("target-api-url", "", "Target API URL (defaults to https://api.github.com)")
	importCmd.Flags().String("target-repo-visibility", "", "Target repo visibility: public, private, or internal")
	importCmd.Flags().Bool("skip-releases", false, "Skip releases when importing")
	importCmd.Flags().Bool("lock-source-repo", false, "Lock source repo during import")
	importCmd.Flags().Bool("no-ssl-verify", false, "Disable SSL verification for GHES communication")

	rootCmd.AddCommand(importCmd)
}
