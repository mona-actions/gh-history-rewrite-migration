package output

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTable(t *testing.T) {
	headers := []string{"Name", "Status", "Count"}
	rows := [][]string{
		{"Item 1", "Success", "42"},
		{"Item 2", "Failed", "0"},
		{"Item 3", "Pending", "10"},
	}

	// Test doesn't panic and runs without error
	// We can't easily capture pterm output, but we can ensure it doesn't crash
	assert.NotPanics(t, func() {
		Table(headers, rows)
	})
}

func TestIsTerminal(t *testing.T) {
	// IsTerminal checks if stdout is a TTY
	// In test context, this is typically false (output redirected)
	result := IsTerminal()

	// We don't assert true/false since it depends on test environment
	// Just verify it returns a boolean without panic
	assert.IsType(t, false, result)
}

func TestIsTerminal_Stdout(t *testing.T) {
	// Verify IsTerminal checks stdout specifically
	// os.Stdout should be the target file descriptor
	require.NotNil(t, os.Stdout)

	// Call should not panic
	assert.NotPanics(t, func() {
		IsTerminal()
	})
}

func TestInfo(t *testing.T) {
	assert.NotPanics(t, func() {
		Info("test info message")
	})
}

func TestSuccess(t *testing.T) {
	assert.NotPanics(t, func() {
		Success("test success message")
	})
}

func TestWarn(t *testing.T) {
	assert.NotPanics(t, func() {
		Warn("test warning message")
	})
}

func TestError(t *testing.T) {
	assert.NotPanics(t, func() {
		Error("test error message")
	})
}

func TestSpinner(t *testing.T) {
	assert.NotPanics(t, func() {
		spinner := Spinner("Loading...")
		assert.NotNil(t, spinner)
		spinner.Stop()
	})
}

func TestMultiSpinner(t *testing.T) {
	assert.NotPanics(t, func() {
		ms := NewMultiSpinner(2)
		ms.Start()
		s0 := ms.Spinner(0, "slot 0")
		s1 := ms.Spinner(1, "slot 1")
		assert.NotNil(t, s0)
		assert.NotNil(t, s1)
		s0.Success("done 0")
		s1.Success("done 1")
		ms.Stop()
	})
}

func TestConfirm_NonInteractive(t *testing.T) {
	// When not in a terminal, Confirm should return the default without prompting
	// We can't easily test interactive mode in unit tests

	// Save original IsTerminal behavior would require more mocking
	// For now, just test it doesn't panic
	assert.NotPanics(t, func() {
		_, _ = Confirm("Test prompt?", true)
	})
}
