// Package output provides structured console output helpers (tables, summaries).
package output

import (
	"fmt"
	"os"

	"github.com/pterm/pterm"
	"github.com/spf13/viper"
	"golang.org/x/term"
)

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
	pterm.Info.Println(message)
}

// Success prints a success message using pterm's success printer.
func Success(message string) {
	if viper.GetBool("NO_COLOR") {
		fmt.Println(message)
		return
	}
	pterm.Success.Println(message)
}

// Warn prints a warning message using pterm's warning printer.
func Warn(message string) {
	if viper.GetBool("NO_COLOR") {
		fmt.Println("WARNING:", message)
		return
	}
	pterm.Warning.Println(message)
}

// Error prints an error message using pterm's error printer.
func Error(message string) {
	if viper.GetBool("NO_COLOR") {
		fmt.Fprintln(os.Stderr, "ERROR:", message)
		return
	}
	pterm.Error.Println(message)
}

// Spinner creates and returns a new spinner with the given title.
// Callers should call Start() to begin the spinner and Success()/Fail() to finish.
func Spinner(title string) *pterm.SpinnerPrinter {
	spinner, _ := pterm.DefaultSpinner.Start(title)
	return spinner
}

// Confirm prompts the user for yes/no confirmation using pterm's interactive confirm.
// Returns true if user confirms (yes), false otherwise.
// defaultYes controls the default selection when user just presses enter.
func Confirm(prompt string, defaultYes bool) (bool, error) {
	if !IsTerminal() {
		// Non-interactive mode: default to the specified default
		return defaultYes, nil
	}

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
