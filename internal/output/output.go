// Package output provides structured console output helpers (tables, summaries).
package output

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/pterm/pterm"
	"github.com/spf13/viper"
	"golang.org/x/term"
)

// mu serializes all pterm calls; pterm has unsynchronized global state.
var mu sync.Mutex

// Logger is the minimal structured logging surface shared by packages that
// accept an injected logger.
type Logger interface {
	Info(message string, args ...any)
	Warn(message string, args ...any)
	Error(message string, args ...any)
}

// PackageLogger adapts this package's package-level output functions to Logger.
type PackageLogger struct{}

func (PackageLogger) Info(message string, args ...any)  { Info(formatMessage(message, args...)) }
func (PackageLogger) Warn(message string, args ...any)  { Warn(formatMessage(message, args...)) }
func (PackageLogger) Error(message string, args ...any) { Error(formatMessage(message, args...)) }

// Info prints an informational message using pterm's info printer.
func Info(message string) {
	if viper.GetBool("NO_COLOR") {
		fmt.Println(message)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	pterm.Info.Println(message)
}

// Success prints a success message using pterm's success printer.
func Success(message string) {
	if viper.GetBool("NO_COLOR") {
		fmt.Println(message)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	pterm.Success.Println(message)
}

// Warn prints a warning message using pterm's warning printer.
func Warn(message string) {
	if viper.GetBool("NO_COLOR") {
		fmt.Println("WARNING:", message)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	pterm.Warning.Println(message)
}

// Error prints an error message using pterm's error printer.
func Error(message string) {
	if viper.GetBool("NO_COLOR") {
		fmt.Fprintln(os.Stderr, "ERROR:", message)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	pterm.Error.Println(message)
}

// SafeSpinner wraps *pterm.SpinnerPrinter and guards every pterm call with mu,
// preventing races when concurrent goroutines each hold their own spinner.
// sp is nil in no-op mode (non-interactive terminals / NO_COLOR), where all
// methods become plain text-print operations — no pterm goroutines are spawned.
type SafeSpinner struct {
	sp *pterm.SpinnerPrinter
}

// IsActive reports whether the spinner is currently running.
func (s *SafeSpinner) IsActive() bool {
	if s.sp == nil {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	return s.sp.IsActive
}

// Stop stops the spinner without a status message.
func (s *SafeSpinner) Stop() error {
	if s.sp == nil {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	return s.sp.Stop()
}

// Success stops the spinner with a success message.
func (s *SafeSpinner) Success(msg ...interface{}) {
	if s.sp == nil {
		if len(msg) > 0 {
			fmt.Println(msg...)
		}
		return
	}
	mu.Lock()
	defer mu.Unlock()
	s.sp.Success(msg...)
}

// Fail stops the spinner with a failure message.
func (s *SafeSpinner) Fail(msg ...interface{}) {
	if s.sp == nil {
		if len(msg) > 0 {
			fmt.Fprintln(os.Stderr, msg...)
		}
		return
	}
	mu.Lock()
	defer mu.Unlock()
	s.sp.Fail(msg...)
}

// UpdateText changes the spinner's label while it is running.
func (s *SafeSpinner) UpdateText(text string) {
	if s.sp == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	s.sp.UpdateText(text)
}

// MultiSpinner manages multiple concurrent spinners via pterm's MultiPrinter.
// Create with NewMultiSpinner(n), call Start() before use, Stop() when done.
type MultiSpinner struct {
	multi   *pterm.MultiPrinter
	writers []io.Writer
	noOp    bool // true in non-TTY/NO_COLOR mode
}

func NewMultiSpinner(n int) *MultiSpinner {
	if viper.GetBool("NO_COLOR") || !IsTerminal() {
		return &MultiSpinner{noOp: true, writers: make([]io.Writer, n)}
	}
	multi := pterm.DefaultMultiPrinter
	writers := make([]io.Writer, n)
	for i := range writers {
		writers[i] = multi.NewWriter()
	}
	return &MultiSpinner{multi: &multi, writers: writers}
}

func (m *MultiSpinner) Start() {
	if m.noOp {
		return
	}
	m.multi.Start()
}

func (m *MultiSpinner) Stop() {
	if m.noOp {
		return
	}
	m.multi.Stop()
}

// Spinner creates a spinner on the given slot.
func (m *MultiSpinner) Spinner(slot int, title string) *SafeSpinner {
	if m.noOp {
		fmt.Println(title)
		return &SafeSpinner{sp: nil}
	}
	sp, _ := pterm.DefaultSpinner.WithWriter(m.writers[slot]).Start(title)
	return &SafeSpinner{sp: sp}
}

// Spinner creates and returns a SafeSpinner with the given title for single-spinner use.
//
// In non-interactive environments (no TTY or NO_COLOR set) it returns a no-op
// spinner that prints plain text instead of spawning pterm's animation goroutine.
// This is the correct behaviour for CI, tests, and piped output.
func Spinner(title string) *SafeSpinner {
	if viper.GetBool("NO_COLOR") || !IsTerminal() {
		fmt.Println(title)
		return &SafeSpinner{sp: nil}
	}
	mu.Lock()
	defer mu.Unlock()
	sp, _ := pterm.DefaultSpinner.Start(title)
	return &SafeSpinner{sp: sp}
}

// Confirm prompts the user for yes/no confirmation using pterm's interactive confirm.
// Returns true if user confirms (yes), false otherwise.
// defaultYes controls the default selection when user just presses enter.
func Confirm(prompt string, defaultYes bool) (bool, error) {
	if !IsTerminal() {
		// Non-interactive mode: default to the specified default
		return defaultYes, nil
	}

	mu.Lock()
	defer mu.Unlock()
	result, err := pterm.DefaultInteractiveConfirm.
		WithDefaultValue(defaultYes).
		Show(prompt)
	return result, err
}

// Table renders a table with the given headers and rows using pterm's table renderer.
// Each row should have the same number of elements as headers.
func Table(headers []string, rows [][]string) {
	if viper.GetBool("NO_COLOR") {
		// Simple text table without colors
		fmt.Println()
		for i, h := range headers {
			fmt.Print(h)
			if i < len(headers)-1 {
				fmt.Print("\t")
			}
		}
		fmt.Println()
		for _, row := range rows {
			for i, cell := range row {
				fmt.Print(cell)
				if i < len(row)-1 {
					fmt.Print("\t")
				}
			}
			fmt.Println()
		}
		fmt.Println()
		return
	}

	mu.Lock()
	defer mu.Unlock()
	tableData := pterm.TableData{headers}
	tableData = append(tableData, rows...)
	_ = pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()
}

// IsTerminal returns true if stdout is connected to a terminal (interactive mode).
// Returns false when output is redirected to a file or pipe.
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// HumanBytes formats n as a human-readable IEC byte string ("1.5 MiB").
// Useful for size summaries in tables and warnings.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func formatMessage(message string, args ...any) string {
	if len(args) == 0 {
		return message
	}
	return fmt.Sprintf(message, args...)
}
