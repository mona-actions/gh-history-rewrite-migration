package doctor

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/api"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/output"
	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// CommandRunner abstracts command execution for testing.
type CommandRunner interface {
	Run(name string, args ...string) (string, error)
}

// DefaultCommandRunner implements CommandRunner using exec.Command.
type DefaultCommandRunner struct{}

// Run executes a command and returns its combined output.
func (r *DefaultCommandRunner) Run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Doctor performs preflight checks before migration operations.
type Doctor struct {
	runner         CommandRunner
	workDir        string
	sourceHostname string
	sourceToken    string
	targetToken    string
}

// Config holds configuration for Doctor preflight checks.
type Config struct {
	WorkDir        string
	SourceHostname string
	SourceToken    string
	TargetToken    string
}

// New creates a new Doctor instance with the specified configuration and command runner.
func New(cfg Config, runner CommandRunner) *Doctor {
	if runner == nil {
		runner = &DefaultCommandRunner{}
	}
	return &Doctor{
		runner:         runner,
		workDir:        cfg.WorkDir,
		sourceHostname: cfg.SourceHostname,
		sourceToken:    cfg.SourceToken,
		targetToken:    cfg.TargetToken,
	}
}

// CheckResult represents the result of a single preflight check.
type CheckResult struct {
	Name    string
	Status  Status
	Message string
}

// Status represents the outcome of a preflight check.
type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
)

// Symbol returns the visual representation of the status.
func (s Status) Symbol() string {
	switch s {
	case StatusOK:
		return "✓"
	case StatusWarn:
		return "⚠"
	case StatusFail:
		return "✗"
	default:
		return "?"
	}
}

// IsFatal returns true if the status represents a critical failure.
func (r CheckResult) IsFatal() bool {
	return r.Status == StatusFail
}

// Run executes all preflight checks and displays results.
func (d *Doctor) Run(ctx context.Context) error {
	results := []CheckResult{}

	// Check git-filter-repo
	results = append(results, d.checkGitFilterRepo())

	// Check gh-gei extension
	results = append(results, d.checkGhGeiExtension())

	// Check gh-gei version
	results = append(results, d.checkGhGeiVersion())

	// Check tar
	results = append(results, d.checkTar())

	// Check environment variables
	results = append(results, d.checkEnvVars())

	// Check source hostname reachability
	results = append(results, d.checkSourceReachable(ctx))

	// Check work directory
	results = append(results, d.checkWorkDir())

	// Display results as table
	d.displayResults(results)

	// Check if any critical checks failed
	for _, r := range results {
		if r.IsFatal() {
			return fmt.Errorf("preflight checks failed")
		}
	}

	return nil
}

func (d *Doctor) checkGitFilterRepo() CheckResult {
	out, err := d.runner.Run("git", "filter-repo", "--help")
	if err != nil {
		return CheckResult{
			Name:    "git-filter-repo",
			Status:  StatusFail,
			Message: "Not found or not executable",
		}
	}

	// Extract version if present
	versionRegex := regexp.MustCompile(`git-filter-repo\s+(\d+\.\d+\.\d+)`)
	matches := versionRegex.FindStringSubmatch(out)
	version := "installed"
	if len(matches) > 1 {
		version = matches[1]
	}

	return CheckResult{
		Name:    "git-filter-repo",
		Status:  StatusOK,
		Message: version,
	}
}

func (d *Doctor) checkGhGeiExtension() CheckResult {
	out, err := d.runner.Run("gh", "extension", "list")
	if err != nil {
		return CheckResult{
			Name:    "gh-gei extension",
			Status:  StatusFail,
			Message: "Unable to list gh extensions",
		}
	}

	if strings.Contains(out, "github/gh-gei") || strings.Contains(out, "gh-gei") {
		return CheckResult{
			Name:    "gh-gei extension",
			Status:  StatusOK,
			Message: "Installed",
		}
	}

	return CheckResult{
		Name:    "gh-gei extension",
		Status:  StatusFail,
		Message: "Not installed (run: gh extension install github/gh-gei)",
	}
}

func (d *Doctor) checkGhGeiVersion() CheckResult {
	out, err := d.runner.Run("gh", "gei", "--version")
	if err != nil {
		return CheckResult{
			Name:    "gh-gei version",
			Status:  StatusWarn,
			Message: "Unable to determine version",
		}
	}

	// Extract version number (e.g., "gh gei version 1.10.0")
	versionRegex := regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)
	matches := versionRegex.FindStringSubmatch(out)
	if len(matches) < 4 {
		return CheckResult{
			Name:    "gh-gei version",
			Status:  StatusWarn,
			Message: "Unable to parse version",
		}
	}

	major, _ := strconv.Atoi(matches[1])
	minor, _ := strconv.Atoi(matches[2])

	// Check if version >= 1.10.0
	if major > 1 || (major == 1 && minor >= 10) {
		return CheckResult{
			Name:    "gh-gei version",
			Status:  StatusOK,
			Message: fmt.Sprintf("%s.%s.%s (>= 1.10.0)", matches[1], matches[2], matches[3]),
		}
	}

	return CheckResult{
		Name:    "gh-gei version",
		Status:  StatusWarn,
		Message: fmt.Sprintf("%s.%s.%s (< 1.10.0, hidden flags may not be available)", matches[1], matches[2], matches[3]),
	}
}

func (d *Doctor) checkTar() CheckResult {
	_, err := d.runner.Run("tar", "--version")
	if err != nil {
		return CheckResult{
			Name:    "tar",
			Status:  StatusFail,
			Message: "Not found on PATH",
		}
	}

	return CheckResult{
		Name:    "tar",
		Status:  StatusOK,
		Message: "Available",
	}
}

func (d *Doctor) checkEnvVars() CheckResult {
	missing := []string{}

	if d.sourceToken == "" {
		missing = append(missing, "GH_SOURCE_PAT")
	}
	if d.targetToken == "" {
		missing = append(missing, "GH_PAT")
	}

	if len(missing) > 0 {
		return CheckResult{
			Name:    "Environment variables",
			Status:  StatusFail,
			Message: fmt.Sprintf("Missing: %s", strings.Join(missing, ", ")),
		}
	}

	return CheckResult{
		Name:    "Environment variables",
		Status:  StatusOK,
		Message: "GH_SOURCE_PAT, GH_PAT set",
	}
}

func (d *Doctor) checkSourceReachable(ctx context.Context) CheckResult {
	if d.sourceToken == "" {
		return CheckResult{
			Name:    "Source API reachability",
			Status:  StatusWarn,
			Message: "Skipped (no GH_SOURCE_PAT)",
		}
	}

	client, err := api.New(ctx, d.sourceHostname, d.sourceToken)
	if err != nil {
		return CheckResult{
			Name:    "Source API reachability",
			Status:  StatusFail,
			Message: fmt.Sprintf("Failed to create client: %v", err),
		}
	}

	if err := client.Reachable(ctx); err != nil {
		return CheckResult{
			Name:    "Source API reachability",
			Status:  StatusFail,
			Message: fmt.Sprintf("Unable to reach %s", d.sourceHostname),
		}
	}

	return CheckResult{
		Name:    "Source API reachability",
		Status:  StatusOK,
		Message: fmt.Sprintf("%s is reachable", d.sourceHostname),
	}
}

func (d *Doctor) checkWorkDir() CheckResult {
	// Use WorkDir.New for existence and writability checks
	wd, err := workdir.New(d.workDir)
	if err != nil {
		return CheckResult{
			Name:    "Work directory",
			Status:  StatusFail,
			Message: err.Error(),
		}
	}

	// Check free space
	availableGB, err := wd.FreeSpaceGB()
	if err != nil {
		// If we can't get disk space, just confirm writable
		return CheckResult{
			Name:    "Work directory",
			Status:  StatusOK,
			Message: fmt.Sprintf("%s is writable", d.workDir),
		}
	}

	minGB := uint64(10) // Default minimum 10GB
	if availableGB < minGB {
		return CheckResult{
			Name:    "Work directory",
			Status:  StatusWarn,
			Message: fmt.Sprintf("%s has only %dGB free (recommend >= %dGB)", d.workDir, availableGB, minGB),
		}
	}

	return CheckResult{
		Name:    "Work directory",
		Status:  StatusOK,
		Message: fmt.Sprintf("%s (%dGB free)", d.workDir, availableGB),
	}
}

func (d *Doctor) displayResults(results []CheckResult) {
	output.Info("Running preflight checks...\n")

	headers := []string{"Check", "Status", "Details"}
	rows := [][]string{}

	for _, r := range results {
		rows = append(rows, []string{r.Name, r.Status.Symbol(), r.Message})
	}

	output.Table(headers, rows)
}
