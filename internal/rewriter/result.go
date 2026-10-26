package rewriter

import (
	"fmt"
	"strings"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/filterrepo"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
)

// TablePrinter renders a header row plus N data rows. It matches the
// signature of internal/output.Table so production callers can pass that
// directly; tests pass a buffer-backed implementation.
type TablePrinter func(headers []string, rows [][]string)

// WarnFn prints a warning line. Defaulted to output.Warn but injectable
// for tests that want to capture warnings without TTY noise.
type WarnFn func(msg string)

// Render writes a human-friendly summary of the rewrite result. If
// printer is nil it defaults to output.Table; if warn is nil it defaults
// to output.Warn.
func (r *Result) Render(printer TablePrinter, warn WarnFn) {
	if r == nil {
		return
	}
	if printer == nil {
		printer = output.Table
	}
	if warn == nil {
		warn = func(msg string) { output.Warn(msg) }
	}

	if r.AnalyzeOnly {
		printer(
			[]string{"metric", "value"},
			[][]string{
				{"mode", "analysis only (no changes)"},
				{"paths over threshold", fmt.Sprintf("%d", r.AnalyzedPaths)},
			},
		)
		for _, w := range r.Warnings {
			warn(w)
		}
		return
	}

	scripts := strings.Join(r.ScriptsRun, ", ")
	if scripts == "" {
		scripts = "(none)"
	}
	preScripts := strings.Join(r.PreScriptsRun, ", ")
	if preScripts == "" {
		preScripts = "(none)"
	}
	flags := strings.Join(r.UserFlagsApplied, " ")
	if flags == "" {
		flags = "(none)"
	}
	mode := r.StripMode.String()
	if r.StripMode == filterrepo.StripNone {
		mode = "(none)"
	}
	sizeMode := r.StripMode == filterrepo.StripBySize
	bytesLabel := "bytes freed"
	if sizeMode {
		// The scan counts unreachable blobs too, so these are upper bounds.
		bytesLabel = "bytes freed (up to, uncompressed)"
	}

	rows := [][]string{
		{"strip performed", boolStr(r.StripPerformed)},
		{"strip mode", mode},
	}
	if sizeMode {
		rows = append(rows, []string{"blobs stripped (up to)", fmt.Sprintf("%d", r.BlobsStripped)})
	} else {
		rows = append(rows, []string{"paths stripped", fmt.Sprintf("%d", len(r.PathsStripped))})
	}
	rows = append(rows,
		[]string{"largest stripped", output.HumanBytes(r.LargestStripped)},
		[]string{bytesLabel, output.HumanBytes(r.BytesFreed)},
		[]string{"pre-rewrite scripts", fmt.Sprintf("%d (%s)", len(r.PreScriptsRun), preScripts)},
		[]string{"scripts run", fmt.Sprintf("%d (%s)", len(r.ScriptsRun), scripts)},
		[]string{"flags applied", flags},
		[]string{"commits remapped", fmt.Sprintf("%d", r.CommitsRemapped)},
	)
	printer([]string{"metric", "value"}, rows)

	if !sizeMode && len(r.PathsStripped) > 0 {
		pathRows := make([][]string, 0, len(r.PathsStripped))
		for _, p := range r.PathsStripped {
			pathRows = append(pathRows, []string{p})
		}
		printer([]string{"stripped path"}, pathRows)
	}

	for _, w := range r.Warnings {
		warn(w)
	}
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
