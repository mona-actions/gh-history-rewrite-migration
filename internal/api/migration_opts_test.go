package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func marshalMigrationOpts(t *testing.T, opts MigrationOpts) map[string]any {
	t.Helper()

	data, err := json.Marshal(opts)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	return got
}

func TestGitOnlyMigrationOptsWireShape(t *testing.T) {
	got := marshalMigrationOpts(t, GitOnlyMigrationOpts("acme/widget"))

	assert.Equal(t, []any{"acme/widget"}, got["repositories"])
	assert.Equal(t, true, got["exclude_metadata"])
	assert.NotContains(t, got, "exclude_git_data")
	assert.NotContains(t, got, "exclude_owner_projects")
	assert.NotContains(t, got, "exclude_releases")
	assert.NotContains(t, got, "lock_repositories")
	assert.NotContains(t, got, "exclude_attachments")
}

func TestMetadataOnlyMigrationOptsWireShape(t *testing.T) {
	base := BaseToggles{
		ExcludeReleases:    true,
		ExcludeAttachments: true,
		LockRepositories:   true,
	}
	got := marshalMigrationOpts(t, MetadataOnlyMigrationOpts("acme/widget", base))

	assert.Equal(t, []any{"acme/widget"}, got["repositories"])
	assert.NotContains(t, got, "exclude_metadata")
	assert.Equal(t, true, got["exclude_git_data"])
	assert.Equal(t, true, got["exclude_owner_projects"])
	assert.Equal(t, true, got["exclude_releases"])
	assert.Equal(t, true, got["lock_repositories"])
	assert.Equal(t, true, got["exclude_attachments"])
}

func TestCombinedMigrationOptsWireShape(t *testing.T) {
	base := BaseToggles{
		ExcludeReleases:    true,
		ExcludeAttachments: true,
		LockRepositories:   true,
	}
	got := marshalMigrationOpts(t, CombinedMigrationOpts("acme/widget", base))

	assert.Equal(t, []any{"acme/widget"}, got["repositories"])
	assert.NotContains(t, got, "exclude_metadata")
	assert.NotContains(t, got, "exclude_git_data")
	assert.NotContains(t, got, "exclude_owner_projects")
	assert.Equal(t, true, got["exclude_releases"])
	assert.Equal(t, true, got["lock_repositories"])
	assert.Equal(t, true, got["exclude_attachments"])
}

func TestMigrationOptsConstructorsDoNotSetMutuallyExclusiveFlags(t *testing.T) {
	base := BaseToggles{
		ExcludeReleases:    true,
		ExcludeAttachments: true,
		LockRepositories:   true,
	}
	constructors := map[string]MigrationOpts{
		"git-only":      GitOnlyMigrationOpts("acme/widget"),
		"metadata-only": MetadataOnlyMigrationOpts("acme/widget", base),
		"combined":      CombinedMigrationOpts("acme/widget", base),
	}

	for name, opts := range constructors {
		t.Run(name, func(t *testing.T) {
			assert.False(t, opts.ExcludeMetadata && opts.ExcludeGitData)
		})
	}
}
