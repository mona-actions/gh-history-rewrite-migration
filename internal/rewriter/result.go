package rewriter

import (
	"fmt"
	"strings"

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

	printer(
		[]string{"metric", "value"},
		[][]string{
			{"strip performed", boolStr(r.StripPerformed)},
			{"paths stripped", fmt.Sprintf("%d", len(r.PathsStripped))},
			{"largest stripped", output.HumanBytes(r.LargestStripped)},
			{"bytes freed", output.HumanBytes(r.BytesFreed)},
			{"pre-rewrite scripts", fmt.Sprintf("%d (%s)", len(r.PreScriptsRun), preScripts)},
			{"scripts run", fmt.Sprintf("%d (%s)", len(r.ScriptsRun), scripts)},
			{"flags applied", flags},
			{"commits remapped", fmt.Sprintf("%d", r.CommitsRemapped)},
		},
	)

	if len(r.PathsStripped) > 0 {
		rows := make([][]string, 0, len(r.PathsStripped))
		for _, p := range r.PathsStripped {
			rows = append(rows, []string{p})
		}
		printer([]string{"stripped path"}, rows)
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
