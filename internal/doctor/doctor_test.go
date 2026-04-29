package doctor

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// MockCommandRunner implements CommandRunner for testing.
type MockCommandRunner struct {
	commands map[string]mockCommand
}

type mockCommand struct {
	output string
	err    error
}

func NewMockCommandRunner() *MockCommandRunner {
	return &MockCommandRunner{
		commands: make(map[string]mockCommand),
	}
}

func (m *MockCommandRunner) AddCommand(cmdline string, output string, err error) {
	m.commands[cmdline] = mockCommand{output: output, err: err}
}

func (m *MockCommandRunner) Run(name string, args ...string) (string, error) {
	cmdline := name
	for _, arg := range args {
		cmdline += " " + arg
	}

	if cmd, ok := m.commands[cmdline]; ok {
		return cmd.output, cmd.err
	}

	return "", fmt.Errorf("command not found: %s", cmdline)
}

func TestNew(t *testing.T) {
	cfg := Config{
		WorkDir:        "./work",
		SourceHostname: "github.com",
		SourceToken:    "test-token",
		TargetToken:    "test-token",
	}

	doctor := New(cfg, nil)
	assert.NotNil(t, doctor)
	assert.Equal(t, "./work", doctor.workDir)
	assert.Equal(t, "github.com", doctor.sourceHostname)
}

func TestCheckGitFilterRepo_Success(t *testing.T) {
	runner := NewMockCommandRunner()
	runner.AddCommand("git filter-repo --help", "git-filter-repo 2.38.0\n\nUsage: git filter-repo...", nil)

	cfg := Config{WorkDir: "./work"}
	doctor := New(cfg, runner)
	result := doctor.checkGitFilterRepo()

	assert.Equal(t, StatusOK, result.Status)
	assert.Equal(t, "2.38.0", result.Message)
}

func TestCheckGitFilterRepo_NotFound(t *testing.T) {
	runner := NewMockCommandRunner()
	runner.AddCommand("git filter-repo --help", "", fmt.Errorf("command not found"))

	cfg := Config{WorkDir: "./work"}
	doctor := New(cfg, runner)
	result := doctor.checkGitFilterRepo()

	assert.Equal(t, StatusFail, result.Status)
	assert.Contains(t, result.Message, "Not found")
}

func TestCheckGhGeiExtension_Installed(t *testing.T) {
	runner := NewMockCommandRunner()
	runner.AddCommand("gh extension list", "github/gh-gei\ngithub/gh-copilot", nil)

	cfg := Config{WorkDir: "./work"}
	doctor := New(cfg, runner)
	result := doctor.checkGhGeiExtension()

	assert.Equal(t, StatusOK, result.Status)
	assert.Equal(t, "Installed", result.Message)
}

func TestCheckGhGeiExtension_NotInstalled(t *testing.T) {
	runner := NewMockCommandRunner()
	runner.AddCommand("gh extension list", "github/gh-copilot", nil)

	cfg := Config{WorkDir: "./work"}
	doctor := New(cfg, runner)
	result := doctor.checkGhGeiExtension()

	assert.Equal(t, StatusFail, result.Status)
	assert.Contains(t, result.Message, "Not installed")
}

func TestCheckGhGeiVersion_ValidVersion(t *testing.T) {
	runner := NewMockCommandRunner()
	runner.AddCommand("gh gei --version", "gh gei version 1.10.0", nil)

	cfg := Config{WorkDir: "./work"}
	doctor := New(cfg, runner)
	result := doctor.checkGhGeiVersion()

	assert.Equal(t, StatusOK, result.Status)
	assert.Contains(t, result.Message, "1.10.0")
}

func TestCheckGhGeiVersion_OldVersion(t *testing.T) {
	runner := NewMockCommandRunner()
	runner.AddCommand("gh gei --version", "gh gei version 1.9.0", nil)

	cfg := Config{WorkDir: "./work"}
	doctor := New(cfg, runner)
	result := doctor.checkGhGeiVersion()

	assert.Equal(t, StatusWarn, result.Status)
	assert.Contains(t, result.Message, "1.9.0")
	assert.Contains(t, result.Message, "< 1.10.0")
}

func TestCheckTar_Success(t *testing.T) {
	runner := NewMockCommandRunner()
	runner.AddCommand("tar --version", "tar (GNU tar) 1.34", nil)

	cfg := Config{WorkDir: "./work"}
	doctor := New(cfg, runner)
	result := doctor.checkTar()

	assert.Equal(t, StatusOK, result.Status)
	assert.Equal(t, "Available", result.Message)
}

func TestCheckEnvVars_AllSet(t *testing.T) {
	cfg := Config{
		WorkDir:     "./work",
		SourceToken: "test-source-token",
		TargetToken: "test-target-token",
	}
	doctor := New(cfg, NewMockCommandRunner())

	result := doctor.checkEnvVars()
	assert.Equal(t, StatusOK, result.Status)
	assert.Contains(t, result.Message, "GH_SOURCE_PAT")
	assert.Contains(t, result.Message, "GH_PAT")
}

func TestCheckEnvVars_Missing(t *testing.T) {
	cfg := Config{
		WorkDir:     "./work",
		SourceToken: "",
		TargetToken: "",
	}
	doctor := New(cfg, NewMockCommandRunner())

	result := doctor.checkEnvVars()
	assert.Equal(t, StatusFail, result.Status)
	assert.Contains(t, result.Message, "Missing")
}

func TestCheckWorkDir_Success(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := Config{WorkDir: tmpDir}
	doctor := New(cfg, NewMockCommandRunner())

	result := doctor.checkWorkDir()
	assert.Equal(t, StatusOK, result.Status)
}

func TestCheckWorkDir_NotWritable(t *testing.T) {
	// Use a path that definitely won't be writable (root directory file)
	cfg := Config{WorkDir: "/root/readonly"}
	doctor := New(cfg, NewMockCommandRunner())

	result := doctor.checkWorkDir()
	// May fail to create or fail to write, either is acceptable
	assert.Contains(t, []Status{StatusFail, StatusWarn}, result.Status)
}

func TestCheckSourceReachable_NoToken(t *testing.T) {
	cfg := Config{
		WorkDir:        "./work",
		SourceHostname: "github.com",
		SourceToken:    "",
	}
	doctor := New(cfg, NewMockCommandRunner())

	result := doctor.checkSourceReachable(context.Background())
	assert.Equal(t, StatusWarn, result.Status)
	assert.Contains(t, result.Message, "Skipped")
}

func TestRun(t *testing.T) {
	tmpDir := t.TempDir()

	runner := NewMockCommandRunner()
	runner.AddCommand("git filter-repo --help", "git-filter-repo 2.38.0", nil)
	runner.AddCommand("gh extension list", "github/gh-gei", nil)
	runner.AddCommand("gh gei --version", "gh gei version 1.10.0", nil)
	runner.AddCommand("tar --version", "tar 1.34", nil)

	cfg := Config{
		WorkDir:        tmpDir,
		SourceHostname: "github.com",
		SourceToken:    "test-source",
		TargetToken:    "test-target",
	}
	doctor := New(cfg, runner)

	// Run should not panic
	assert.NotPanics(t, func() {
		doctor.Run(context.Background())
	})
}
