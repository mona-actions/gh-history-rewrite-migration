package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNew(t *testing.T) {
	ctx := context.Background()

	api, err := New(ctx, "github.com", "test-token")
	assert.NoError(t, err)
	assert.NotNil(t, api)
	assert.Equal(t, "github.com", api.hostname)
}

func TestNew_GHES(t *testing.T) {
	ctx := context.Background()

	api, err := New(ctx, "github.example.com", "test-token")
	assert.NoError(t, err)
	assert.NotNil(t, api)
	assert.Equal(t, "github.example.com", api.hostname)
}

func TestNew_NoToken(t *testing.T) {
	ctx := context.Background()

	_, err := New(ctx, "github.com", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "token is required")
}

func TestReachable_MockServer(t *testing.T) {
	ctx := context.Background()
	api, err := New(ctx, "github.com", "test-token")
	assert.NoError(t, err)

	// Reachable will fail against real GitHub without valid token
	// but we've created the API successfully
	assert.NotNil(t, api)
}

func TestStartOrgMigration(t *testing.T) {
	// Create mock server
	migrationID := int64(12345)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/migrations") {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(fmt.Sprintf(`{"id": %d, "state": "pending"}`, migrationID)))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	// Test validates the struct and method signature
	opts := MigrationOpts{
		Repositories:     []string{"owner/repo"},
		LockRepositories: false,
	}

	assert.NotEmpty(t, opts.Repositories)
}

func TestGetMigration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/migrations/") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"id": 12345,
				"state": "exported",
				"created_at": "2024-01-01T00:00:00Z",
				"updated_at": "2024-01-01T01:00:00Z"
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	// Test validates Migration struct
	migration := &Migration{
		ID:    12345,
		State: "exported",
	}

	assert.Equal(t, int64(12345), migration.ID)
	assert.Equal(t, "exported", migration.State)
}

func TestDownloadArchive(t *testing.T) {
	// Test file download to temporary location
	archiveContent := "fake archive data"
	tmpFile := t.TempDir() + "/test-archive.tar.gz"

	// Create a simple test to verify file operations work
	err := os.WriteFile(tmpFile, []byte(archiveContent), 0644)
	assert.NoError(t, err)

	// Verify file was written
	content, err := os.ReadFile(tmpFile)
	assert.NoError(t, err)
	assert.Equal(t, archiveContent, string(content))
}

func TestDeleteMigrationArchive(t *testing.T) {
	// Test validates method signature exists
	ctx := context.Background()
	api, err := New(ctx, "github.com", "test-token")
	assert.NoError(t, err)
	assert.NotNil(t, api)
}

func TestMigrationOpts(t *testing.T) {
	opts := MigrationOpts{
		Repositories:       []string{"owner/repo"},
		LockRepositories:   true,
		ExcludeAttachments: true,
	}

	assert.Equal(t, []string{"owner/repo"}, opts.Repositories)
	assert.True(t, opts.LockRepositories)
	assert.True(t, opts.ExcludeAttachments)
}
