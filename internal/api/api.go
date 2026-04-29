package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gofri/go-github-ratelimit/github_ratelimit"
	"github.com/google/go-github/v62/github"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

// API wraps GitHub REST and GraphQL clients with rate limiting support.
type API struct {
	client   *github.Client
	clientV4 *githubv4.Client
	hostname string
}

// MigrationOpts contains options for starting an organization migration.
type MigrationOpts struct {
	Repositories       []string
	LockRepositories   bool
	ExcludeAttachments bool
}

// Migration represents a GitHub organization migration.
type Migration struct {
	ID        int64
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// New creates a new API instance configured for the specified hostname and token.
// For github.com, uses default endpoints. For GHES, uses https://<hostname>/api/v3 and https://<hostname>/api/graphql.
func New(ctx context.Context, hostname, token string) (*API, error) {
	if token == "" {
		return nil, fmt.Errorf("token is required")
	}

	// Create OAuth2 token source
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)

	// Create HTTP client with rate limiting
	rateLimiter, err := github_ratelimit.NewRateLimitWaiterClient(
		oauth2.NewClient(ctx, ts).Transport,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create rate limiter: %w", err)
	}

	var client *github.Client
	var clientV4 *githubv4.Client

	if hostname == "github.com" {
		// Use default endpoints for GitHub.com
		client = github.NewClient(rateLimiter)
		clientV4 = githubv4.NewClient(rateLimiter)
	} else {
		// Use GHES endpoints
		baseURL := fmt.Sprintf("https://%s/api/v3/", hostname)
		graphqlURL := fmt.Sprintf("https://%s/api/graphql", hostname)

		client, err = github.NewClient(rateLimiter).WithEnterpriseURLs(baseURL, baseURL)
		if err != nil {
			return nil, fmt.Errorf("failed to create GHES client: %w", err)
		}

		clientV4 = githubv4.NewEnterpriseClient(graphqlURL, rateLimiter)
	}

	return &API{
		client:   client,
		clientV4: clientV4,
		hostname: hostname,
	}, nil
}

// Reachable verifies the API is accessible by making a simple request.
// For github.com, uses Zen(). For GHES, uses APIMeta() or Root().
func (a *API) Reachable(ctx context.Context) error {
	if a.hostname == "github.com" {
		// Use Zen endpoint for github.com
		_, _, err := a.client.Zen(ctx)
		if err != nil {
			return fmt.Errorf("failed to reach GitHub API: %w", err)
		}
		return nil
	}

	// For GHES, use APIMeta endpoint
	_, _, err := a.client.APIMeta(ctx)
	if err != nil {
		return fmt.Errorf("failed to reach GitHub Enterprise API at %s: %w", a.hostname, err)
	}
	return nil
}

// StartOrgMigration starts an organization migration with the specified options.
// Returns the migration ID on success.
func (a *API) StartOrgMigration(ctx context.Context, org string, opts MigrationOpts) (int64, error) {
	migrationOpts := &github.MigrationOptions{
		LockRepositories:   opts.LockRepositories,
		ExcludeAttachments: opts.ExcludeAttachments,
	}

	result, _, err := a.client.Migrations.StartMigration(ctx, org, opts.Repositories, migrationOpts)
	if err != nil {
		return 0, fmt.Errorf("failed to start migration: %w", err)
	}

	return result.GetID(), nil
}

// GetMigration retrieves the status of an organization migration.
func (a *API) GetMigration(ctx context.Context, org string, id int64) (*Migration, error) {
	migration, _, err := a.client.Migrations.MigrationStatus(ctx, org, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get migration status: %w", err)
	}

	// Parse time strings (CreatedAt and UpdatedAt are strings in v62)
	createdAt, _ := time.Parse(time.RFC3339, migration.GetCreatedAt())
	updatedAt, _ := time.Parse(time.RFC3339, migration.GetUpdatedAt())

	return &Migration{
		ID:        migration.GetID(),
		State:     migration.GetState(),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

// DownloadArchive downloads the migration archive to the specified destination path.
// Follows redirects and streams the content to disk.
// Respects context cancellation for long downloads.
func (a *API) DownloadArchive(ctx context.Context, org string, id int64, dest string) error {
	// Get the archive URL (this returns a redirect)
	url, err := a.client.Migrations.MigrationArchiveURL(ctx, org, id)
	if err != nil {
		return fmt.Errorf("failed to get archive URL: %w", err)
	}

	// Create HTTP client that follows redirects
	httpClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // Allow redirects
		},
	}

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Make the request
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Create destination file
	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer out.Close()

	// Stream to disk
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write archive: %w", err)
	}

	return nil
}

// DeleteMigrationArchive deletes the migration archive from GitHub storage.
// This is a best-effort cleanup operation.
func (a *API) DeleteMigrationArchive(ctx context.Context, org string, id int64) error {
	_, err := a.client.Migrations.DeleteMigration(ctx, org, id)
	if err != nil {
		return fmt.Errorf("failed to delete migration archive: %w", err)
	}
	return nil
}
