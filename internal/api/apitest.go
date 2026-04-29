package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/google/go-github/v62/github"
)

// NewForTesting builds an *API whose underlying github.Client points at the
// given URL (typically an httptest.Server URL). It is intended ONLY for
// tests — production code MUST use New(), which enforces the hostname /
// HTTPS plumbing required against real GitHub.com or GHES.
//
// Lives in a non-`_test.go` file so it can be called from other packages'
// integration tests; access to api.API's unexported fields requires the
// helper to be defined in package api itself.
func NewForTesting(serverURL string) (*API, error) {
	base, err := url.Parse(strings.TrimSuffix(serverURL, "/") + "/")
	if err != nil {
		return nil, err
	}
	c := github.NewClient(http.DefaultClient)
	c.BaseURL = base
	c.UploadURL = base
	return &API{client: c, hostname: "test.local"}, nil
}
