// Package largefiles implements the analyze → flag → cleanup workflow for
// removing oversized blobs from git history before migration.
//
// It builds on internal/filterrepo: the Analyzer struct calls
// filterrepo.Runner.Analyze, applies a max+cumulative threshold rule, and
// produces both an in-memory Report and the cleanup.txt file consumed by
// filterrepo.Runner.RunCombined (via CombinedOpts.PathsFromFile).
//
// This package is library-only — it registers no cobra commands. The
// rewrite/migrate orchestrators wire it up.
package largefiles

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/filterrepo"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
)

// Analyzer flags paths whose history-wide footprint exceeds a byte threshold.
type Analyzer struct {
	runner    *filterrepo.Runner
	log       output.Logger
	threshold int64
}

// New constructs an Analyzer. thresholdBytes must be > 0 (see
// ParseThreshold for parsing user-supplied size strings).
func New(runner *filterrepo.Runner, log output.Logger, thresholdBytes int64) *Analyzer {
	return &Analyzer{runner: runner, log: log, threshold: thresholdBytes}
}

// Reason classifies why a path was flagged.
const (
	ReasonSingleBlob = "single blob"
	ReasonCumulative = "cumulative"
	ReasonBoth       = "both"
)

// Report is the result of Analyze: which paths exceed the threshold, and why.
type Report struct {
	// Threshold in bytes used for this report.
	Threshold int64
	// Flagged paths, sorted descending by Footprint
	// (max of MaxDeletedUnpackedBytes and CumulativeBytes).
	Flagged []FlaggedPath
}

// FlaggedPath records a single offending path together with the reason it
// tripped the threshold.
type FlaggedPath struct {
	Path                    string
	MaxDeletedUnpackedBytes int64
	CumulativeBytes         int64
	// Reason is one of ReasonSingleBlob, ReasonCumulative, or ReasonBoth.
	Reason string
}

// Footprint returns the larger of MaxDeletedUnpackedBytes and
// CumulativeBytes — the canonical "size impact" metric used for sort
// order, summary totals, and threshold comparisons. Mirrors
// filterrepo.PathStats.Footprint so callers don't re-derive the formula.
func (p FlaggedPath) Footprint() int64 {
	if p.MaxDeletedUnpackedBytes > p.CumulativeBytes {
		return p.MaxDeletedUnpackedBytes
	}
	return p.CumulativeBytes
}

// Analyze runs filter-repo's analyze pass against bareRepoPath and applies
// the max+cumulative threshold rule:
//
//	flagged ⇔ max(MaxDeletedUnpackedBytes, CumulativeBytes) > threshold
//
// Reason is "single blob" when only the max trips, "cumulative" when only
// the sum trips, and "both" when both do.
func (a *Analyzer) Analyze(ctx context.Context, bareRepoPath string) (*Report, error) {
	if a.threshold <= 0 {
		return nil, fmt.Errorf("threshold must be > 0, got %d", a.threshold)
	}
	if a.runner == nil {
		return nil, errors.New("largefiles: runner is nil")
	}
	res, err := a.runner.Analyze(ctx, bareRepoPath)
	if err != nil {
		return nil, err
	}
	return a.report(res), nil
}

// report is the pure-function core, separated for testability.
//
// The input slice is assumed sorted descending by Footprint (the contract
// of filterrepo.Runner.Analyze). Filtering preserves that order, so this
// method does not re-sort.
func (a *Analyzer) report(res *filterrepo.AnalyzeResult) *Report {
	rep := &Report{Threshold: a.threshold}
	for _, p := range res.Paths {
		maxTrips := p.MaxDeletedUnpackedBytes > a.threshold
		cumTrips := p.CumulativeBytes > a.threshold
		if !maxTrips && !cumTrips {
			continue
		}
		reason := ReasonBoth
		switch {
		case maxTrips && !cumTrips:
			reason = ReasonSingleBlob
		case !maxTrips && cumTrips:
			reason = ReasonCumulative
		}
		rep.Flagged = append(rep.Flagged, FlaggedPath{
			Path:                    p.Path,
			MaxDeletedUnpackedBytes: p.MaxDeletedUnpackedBytes,
			CumulativeBytes:         p.CumulativeBytes,
			Reason:                  reason,
		})
	}
	return rep
}

// WriteCleanupFile writes one flagged path per line to dest in the order
// they appear in r.Flagged. The resulting file is suitable as the
// --paths-from-file argument for filterrepo.Runner.RunCombined
// (CombinedOpts.PathsFromFile).
//
// An empty Flagged slice still writes an empty file — callers should
// short-circuit before calling if that is undesirable.
func (r *Report) WriteCleanupFile(dest string) error {
	if r == nil {
		return errors.New("largefiles: report is nil")
	}
	if dest == "" {
		return errors.New("largefiles: dest is required")
	}
	var b strings.Builder
	for _, p := range r.Flagged {
		b.WriteString(p.Path)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(dest, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	return nil
}

// sizeUnits maps a normalized suffix to its byte multiplier. Suffixes are
// matched case-insensitively. We accept both decimal (K, M, G) and binary
// (Ki, Mi, Gi) conventions; for parity with `git filter-repo` and the
// PayPal doc the bare K/M/G form is decimal-but-treated-as-1024 to match
// most engineers' mental model — explicitly documented in README.
var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"GB", 1 << 30},
	{"GIB", 1 << 30},
	{"GI", 1 << 30},
	{"G", 1 << 30},
	{"MB", 1 << 20},
	{"MIB", 1 << 20},
	{"MI", 1 << 20},
	{"M", 1 << 20},
	{"KB", 1 << 10},
	{"KIB", 1 << 10},
	{"KI", 1 << 10},
	{"K", 1 << 10},
	{"B", 1},
}

// ParseThreshold parses a size string with an optional suffix (case-insensitive):
//
//	"400M", "400MB", "1G", "1.5GB", "1024K", "1048576" (bare bytes)
//
// Negative, zero, empty, or unparseable values are rejected. The returned
// value is in bytes.
func ParseThreshold(s string) (int64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, errors.New("threshold is empty")
	}
	upper := strings.ToUpper(trimmed)

	var mult int64 = 1
	numPart := upper
	for _, u := range sizeUnits {
		if strings.HasSuffix(upper, u.suffix) {
			mult = u.mult
			numPart = strings.TrimSpace(upper[:len(upper)-len(u.suffix)])
			break
		}
	}
	if numPart == "" {
		return 0, fmt.Errorf("threshold %q has no numeric component", s)
	}

	val, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("threshold %q: %w", s, err)
	}
	if val <= 0 {
		return 0, fmt.Errorf("threshold %q must be > 0", s)
	}
	bytes := val * float64(mult)
	if bytes > math.MaxInt64 || math.IsInf(bytes, 0) || math.IsNaN(bytes) {
		return 0, fmt.Errorf("threshold %q overflows int64", s)
	}
	result := int64(bytes)
	if result <= 0 {
		return 0, fmt.Errorf("threshold %q rounds to <= 0 bytes", s)
	}
	return result, nil
}
