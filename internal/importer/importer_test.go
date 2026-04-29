package importer

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mona-actions/gh-history-rewrite-migration/internal/workdir"
)

// stubExecer records calls and returns canned results.
type stubExecer struct {
	lookErr   error
	runErr    error
	gotName   string
	gotArgs   []string
	gotEnv    []string
	runCalled bool
}

func (s *stubExecer) LookPath(name string) (string, error) {
	if s.lookErr != nil {
		return "", s.lookErr
	}
	return "/usr/bin/" + name, nil
}

func (s *stubExecer) Run(ctx context.Context, name string, args []string, env []string) error {
	s.runCalled = true
	s.gotName = name
	s.gotArgs = args
	s.gotEnv = env
	return s.runErr
}

// newWorkDir returns a *workdir.WorkDir with both archives written so the
// HasGitArchive / HasMetadataArchive checks pass.
func newWorkDir(t *testing.T, withArchives bool) *workdir.WorkDir {
	t.Helper()
	root := t.TempDir()
	wd, err := workdir.New(root)
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}
	if withArchives {
		if err := os.WriteFile(wd.GitArchive(), []byte("git"), 0644); err != nil {
			t.Fatalf("write git archive: %v", err)
		}
		if err := os.WriteFile(wd.MetadataArchive(), []byte("meta"), 0644); err != nil {
			t.Fatalf("write metadata archive: %v", err)
		}
	}
	return wd
}

// withPATEnv sets GH_SOURCE_PAT / GH_PAT for the test and restores prior
// values via t.Cleanup.
func withPATEnv(t *testing.T, source, target string) {
	t.Helper()
	prevSource, hadSource := os.LookupEnv("GH_SOURCE_PAT")
	prevTarget, hadTarget := os.LookupEnv("GH_PAT")
	if source == "" {
		os.Unsetenv("GH_SOURCE_PAT")
	} else {
		os.Setenv("GH_SOURCE_PAT", source)
	}
	if target == "" {
		os.Unsetenv("GH_PAT")
	} else {
		os.Setenv("GH_PAT", target)
	}
	t.Cleanup(func() {
		if hadSource {
			os.Setenv("GH_SOURCE_PAT", prevSource)
		} else {
			os.Unsetenv("GH_SOURCE_PAT")
		}
		if hadTarget {
			os.Setenv("GH_PAT", prevTarget)
		} else {
			os.Unsetenv("GH_PAT")
		}
	})
}

func argsContain(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j, w := range want {
			if args[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestRun_HappyPath_GitHubCom(t *testing.T) {
	wd := newWorkDir(t, true)
	withPATEnv(t, "src-token", "tgt-token")

	stub := &stubExecer{}
	imp := New(wd, Config{
		TargetOrg:      "dest-org",
		TargetRepo:     "dest-repo",
		SourceHostname: "github.com",
		Confirm:        true,
	}, stub)

	if err := imp.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !stub.runCalled {
		t.Fatal("expected Execer.Run to be called")
	}
	if !strings.HasSuffix(stub.gotName, "/gh") {
		t.Errorf("expected gh binary, got %q", stub.gotName)
	}
	wantPairs := [][]string{
		{"gei", "migrate-repo"},
		{"--github-target-org", "dest-org"},
		{"--target-repo", "dest-repo"},
		{"--git-archive-path", wd.GitArchive()},
		{"--metadata-archive-path", wd.MetadataArchive()},
	}
	for _, p := range wantPairs {
		if !argsContain(stub.gotArgs, p...) {
			t.Errorf("expected args to contain %v, got %v", p, stub.gotArgs)
		}
	}
	for _, a := range stub.gotArgs {
		if a == "--ghes-api-url" {
			t.Errorf("did not expect --ghes-api-url for github.com source; args=%v", stub.gotArgs)
		}
	}
}

func TestRun_GHESSource_AddsAPIURL(t *testing.T) {
	wd := newWorkDir(t, true)
	withPATEnv(t, "src-token", "tgt-token")

	stub := &stubExecer{}
	imp := New(wd, Config{
		TargetOrg:      "dest-org",
		TargetRepo:     "dest-repo",
		SourceHostname: "ghes.example.com",
		Confirm:        true,
	}, stub)

	if err := imp.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !argsContain(stub.gotArgs, "--ghes-api-url", "https://ghes.example.com/api/v3") {
		t.Errorf("expected --ghes-api-url https://ghes.example.com/api/v3 in args, got %v", stub.gotArgs)
	}
}

func TestRun_MissingArchives(t *testing.T) {
	withPATEnv(t, "src", "tgt")

	t.Run("no git archive", func(t *testing.T) {
		wd := newWorkDir(t, false)
		// Only write metadata.
		os.WriteFile(wd.MetadataArchive(), []byte("m"), 0644)
		imp := New(wd, Config{TargetOrg: "o", TargetRepo: "r", Confirm: true}, &stubExecer{})
		err := imp.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "git archive not found") {
			t.Fatalf("expected git archive error, got %v", err)
		}
	})

	t.Run("no metadata archive", func(t *testing.T) {
		wd := newWorkDir(t, false)
		os.WriteFile(wd.GitArchive(), []byte("g"), 0644)
		imp := New(wd, Config{TargetOrg: "o", TargetRepo: "r", Confirm: true}, &stubExecer{})
		err := imp.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "metadata archive not found") {
			t.Fatalf("expected metadata archive error, got %v", err)
		}
	})
}

func TestRun_MissingPATs(t *testing.T) {
	wd := newWorkDir(t, true)

	t.Run("missing GH_SOURCE_PAT", func(t *testing.T) {
		withPATEnv(t, "", "tgt")
		imp := New(wd, Config{TargetOrg: "o", TargetRepo: "r", Confirm: true}, &stubExecer{})
		err := imp.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "GH_SOURCE_PAT") {
			t.Fatalf("expected GH_SOURCE_PAT error, got %v", err)
		}
	})

	t.Run("missing GH_PAT", func(t *testing.T) {
		withPATEnv(t, "src", "")
		imp := New(wd, Config{TargetOrg: "o", TargetRepo: "r", Confirm: true}, &stubExecer{})
		err := imp.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "GH_PAT") {
			t.Fatalf("expected GH_PAT error, got %v", err)
		}
	})
}

func TestRun_NoTTY_RequiresConfirm(t *testing.T) {
	// Tests run with stdout redirected to a pipe, so IsTerminal() is false.
	wd := newWorkDir(t, true)
	withPATEnv(t, "src", "tgt")
	imp := New(wd, Config{
		TargetOrg:  "o",
		TargetRepo: "r",
		Confirm:    false,
	}, &stubExecer{})
	err := imp.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--confirm required") {
		t.Fatalf("expected --confirm required error, got %v", err)
	}
}

func TestRun_PATsInEnvNotArgs(t *testing.T) {
	wd := newWorkDir(t, true)
	withPATEnv(t, "secret-source-pat-xyz", "secret-target-pat-abc")

	stub := &stubExecer{}
	imp := New(wd, Config{
		TargetOrg:      "o",
		TargetRepo:     "r",
		SourceHostname: "github.com",
		Confirm:        true,
	}, stub)

	if err := imp.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Args must NOT contain the PAT tokens or the env-var names with values.
	for _, a := range stub.gotArgs {
		if strings.Contains(a, "secret-source-pat-xyz") ||
			strings.Contains(a, "secret-target-pat-abc") ||
			strings.Contains(a, "GH_SOURCE_PAT") ||
			strings.Contains(a, "GH_PAT") {
			t.Errorf("argv leaked PAT/env name: %q (args=%v)", a, stub.gotArgs)
		}
	}

	// Env must contain both PAT entries exactly once.
	var sawSource, sawTarget int
	for _, kv := range stub.gotEnv {
		if kv == "GH_SOURCE_PAT=secret-source-pat-xyz" {
			sawSource++
		}
		if kv == "GH_PAT=secret-target-pat-abc" {
			sawTarget++
		}
	}
	if sawSource != 1 {
		t.Errorf("expected exactly one GH_SOURCE_PAT entry, got %d", sawSource)
	}
	if sawTarget != 1 {
		t.Errorf("expected exactly one GH_PAT entry, got %d", sawTarget)
	}
}
