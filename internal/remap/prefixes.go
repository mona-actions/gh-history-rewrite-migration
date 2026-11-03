package remap

import "github.com/mona-actions/gh-commit-remap/pkg/commitremap"

// SHABearingPrefixes follows the released upstream metadata prefix list.
var SHABearingPrefixes = commitremap.DefaultPrefixes()

// KnownNonSHAPrefixes lists prefix-shaped filenames we DO NOT process — used
// to compute the "unknown prefix" warning surface.
var KnownNonSHAPrefixes = []string{
	"organizations",
	"repositories",
	"users",
	"attachments",
}
