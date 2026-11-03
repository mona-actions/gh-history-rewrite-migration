package remap

import (
	"path/filepath"
	"testing"

	"github.com/mona-actions/gh-commit-remap/pkg/archive"
	"github.com/mona-actions/gh-commit-remap/pkg/commitremap"
)

func TestUpstreamPublicAPISmoke(t *testing.T) {
	fx := newFixture(t, map[string]string{"pull_request_review_threads_1.json": oldSHA1}, commitMap(oldSHA1+" "+newSHA1))
	m, err := commitremap.ParseCommitMap(fx.in.CommitMapPath)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := commitremap.ProcessFiles(fx.src, commitremap.DefaultPrefixes(), m, commitremap.ProcessOptions{})
	if err != nil || stats.TotalReplacements() != 1 {
		t.Fatalf("directory API: %+v, %v", stats, err)
	}
	out := filepath.Join(t.TempDir(), "stream.tar.gz")
	stats, err = archive.StreamRemap(fx.in.RawMetadataArchive, out, m, commitremap.DefaultPrefixes())
	if err != nil || stats.TotalReplacements() != 1 {
		t.Fatalf("stream API: %+v, %v", stats, err)
	}
}
