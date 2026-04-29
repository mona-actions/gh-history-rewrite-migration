package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestAPI is a thin wrapper around the package-level NewForTesting so the
// existing test bodies can stay terse. NewForTesting lives in a non-_test
// file because it's also used by the orchestrator e2e test in
// internal/migrate.
func newTestAPI(t *testing.T, serverURL string) *API {
	t.Helper()
	a, err := NewForTesting(serverURL)
	require.NoError(t, err)
	return a
}

func TestStartOrgMigration_HTTP(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintln(w, `{"id":4242,"state":"pending"}`)
	}))
	defer srv.Close()

	a := newTestAPI(t, srv.URL)
	id, err := a.StartOrgMigration(context.Background(), "acme", MigrationOpts{
		Repositories:       []string{"acme/widget"},
		LockRepositories:   true,
		ExcludeAttachments: true,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(4242), id)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Contains(t, gotPath, "/orgs/acme/migrations")
}

func TestStartOrgMigration_HTTP_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	a := newTestAPI(t, srv.URL)
	_, err := a.StartOrgMigration(context.Background(), "acme", MigrationOpts{
		Repositories: []string{"acme/widget"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to start migration")
}

func TestGetMigration_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{
			"id": 99,
			"state": "exported",
			"created_at": "2024-01-01T00:00:00Z",
			"updated_at": "2024-01-02T00:00:00Z"
		}`)
	}))
	defer srv.Close()
	a := newTestAPI(t, srv.URL)
	m, err := a.GetMigration(context.Background(), "acme", 99)
	require.NoError(t, err)
	assert.Equal(t, int64(99), m.ID)
	assert.Equal(t, "exported", m.State)
	assert.Equal(t, 2024, m.CreatedAt.Year())
	assert.Equal(t, 2024, m.UpdatedAt.Year())
}

func TestGetMigration_HTTP_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	a := newTestAPI(t, srv.URL)
	_, err := a.GetMigration(context.Background(), "acme", 99)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get migration status")
}

func TestDeleteMigrationArchive_HTTP(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	a := newTestAPI(t, srv.URL)
	require.NoError(t, a.DeleteMigrationArchive(context.Background(), "acme", 7))
	assert.Equal(t, http.MethodDelete, gotMethod)
}

func TestDeleteMigrationArchive_HTTP_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	a := newTestAPI(t, srv.URL)
	err := a.DeleteMigrationArchive(context.Background(), "acme", 7)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to delete migration archive")
}

// TestDownloadArchive_HTTP exercises the redirect → stream-to-disk pipeline.
// The github.Client returns the redirect Location for the archive URL endpoint;
// our DownloadArchive impl then GETs that URL via a separate http.Client and
// writes the response body to disk.
func TestDownloadArchive_HTTP(t *testing.T) {
	const body = "fake-tarball-bytes"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 1. The github.Client hits this endpoint expecting a redirect; it
	//    extracts the Location header and returns it to api.DownloadArchive.
	mux.HandleFunc("/orgs/acme/migrations/77/archive", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/download/77.tar.gz", http.StatusFound)
	})
	// 2. The follow-up GET that DownloadArchive makes.
	mux.HandleFunc("/download/77.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	})

	a := newTestAPI(t, srv.URL)
	dst := filepath.Join(t.TempDir(), "archive.tar.gz")
	require.NoError(t, a.DownloadArchive(context.Background(), "acme", 77, dst))

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))
}

func TestDownloadArchive_HTTP_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"gone"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	a := newTestAPI(t, srv.URL)
	dst := filepath.Join(t.TempDir(), "archive.tar.gz")
	err := a.DownloadArchive(context.Background(), "acme", 1, dst)
	require.Error(t, err)
}

func TestReachable_HTTP_GitHubCom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Zen returns a plain-text body.
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "Anything added dilutes everything else.")
	}))
	defer srv.Close()
	a := newTestAPI(t, srv.URL)
	a.hostname = "github.com" // force the Zen() path
	require.NoError(t, a.Reachable(context.Background()))
}

func TestReachable_HTTP_GHES(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"verifiable_password_authentication":true}`)
	}))
	defer srv.Close()
	a := newTestAPI(t, srv.URL)
	// hostname != "github.com" → APIMeta path
	require.NoError(t, a.Reachable(context.Background()))
}

func TestReachable_HTTP_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	a := newTestAPI(t, srv.URL)
	a.hostname = "github.com"
	err := a.Reachable(context.Background())
	require.Error(t, err)
}
