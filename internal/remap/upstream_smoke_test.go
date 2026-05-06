package remap

import (
	"testing"

	"github.com/mona-actions/gh-commit-remap/pkg/archive"
	"github.com/mona-actions/gh-commit-remap/pkg/commitremap"
)

func TestUpstreamPublicAPISmoke(t *testing.T) {
	var _ = commitremap.ParseCommitMap
	var _ = commitremap.ProcessFiles
	var _ = commitremap.DefaultPrefixes
	var _ = archive.UnTar
	var _ = archive.ReTarDir
}
