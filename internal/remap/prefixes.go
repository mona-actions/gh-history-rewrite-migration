package remap

// SHABearingPrefixes lists every gei-archive metadata-prefix file we believe
// can carry commit SHA references. Used as the prefix list passed to
// commitremap.ProcessFiles (NOT upstream's DefaultPrefixes(), which is
// incomplete — see docs/upstream-issues/).
var SHABearingPrefixes = []string{
	// VERIFIED — present in real GHES export fixtures, contains SHAs:
	"issues",
	"issue_events",
	"issue_comments",
	"pull_requests",

	// UNVERIFIED — added defensively based on plausibility (richer repos
	// likely produce these; verified by unknown-prefix warning + real exports):
	"pull_request_review_comments",
	"pull_request_reviews",
	"commit_comments",
}

// KnownNonSHAPrefixes lists prefix-shaped filenames we DO NOT process — used
// to compute the "unknown prefix" warning surface.
var KnownNonSHAPrefixes = []string{
	"organizations",
	"repositories",
	"users",
	"attachments",
	"schema", // schema.json — not numbered, but matches *_*.json regex if interpreted loosely; included for completeness
}
