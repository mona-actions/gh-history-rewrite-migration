package filterrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// preRewriteFastExportArgs are the git fast-export flags that feed the
// pre-rewrite pipeline. They are deliberately fixed:
//
//   - --all: export every ref. The pre-rewrite pipeline always operates on
//     the whole history; ref-narrowing via --refs is rejected up front (see
//     ValidatePreRewriteFlags) precisely because this is hardcoded.
//   - --signed-tags=strip: signatures cannot survive a rewrite anyway.
//   - --show-original-ids: emits `original-oid <sha>` so filter-repo can build
//     a commit-map that maps the ORIGINAL SHAs to the final ones. The
//     downstream metadata remap depends on this; without it the map is wrong.
//   - --reencode=no: pass commit-message bytes through unchanged.
//   - --use-done-feature: emit a trailing `done` so a script that truncates
//     the stream mid-way causes filter-repo to fail loudly (missing done)
//     instead of silently importing a partial history.
var preRewriteFastExportArgs = []string{
	"fast-export",
	"--all",
	"--signed-tags=strip",
	"--show-original-ids",
	"--reencode=no",
	"--use-done-feature",
}

// scriptEnvAllowlist is the set of parent environment variables forwarded to
// user pre-rewrite scripts. Everything else — notably the migration PATs
// (GH_PAT, GH_SOURCE_PAT) and any cloud credentials — is withheld. LC_ALL is
// forced to C separately so byte-level stream matching is locale-independent.
var scriptEnvAllowlist = []string{"PATH", "HOME", "LANG", "TMPDIR"}

// ValidatePreRewriteScripts resolves each script to an absolute path and
// verifies it is a regular, executable file beginning with a `#!` shebang.
// It runs before any process starts so misconfiguration fails fast and
// loudly rather than as an opaque mid-pipeline exec error.
func ValidatePreRewriteScripts(scripts []string) ([]string, error) {
	if len(scripts) == 0 {
		return nil, errors.New("no pre-rewrite scripts supplied")
	}
	abs := make([]string, 0, len(scripts))
	for _, s := range scripts {
		p, err := filepath.Abs(s)
		if err != nil {
			return nil, fmt.Errorf("pre-rewrite script %q: %w", s, err)
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("pre-rewrite script %s: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("pre-rewrite script %s: not a regular file", p)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return nil, fmt.Errorf("pre-rewrite script %s: not executable (chmod +x it and give it a #!shebang)", p)
		}
		if err := checkShebang(p); err != nil {
			return nil, err
		}
		abs = append(abs, p)
	}
	return abs, nil
}

// checkShebang verifies the file starts with "#!" so the kernel can pick the
// interpreter. We never construct an interpreter command line ourselves.
func checkShebang(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("pre-rewrite script %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	var head [2]byte
	n, _ := io.ReadFull(f, head[:])
	if n < 2 || head[0] != '#' || head[1] != '!' {
		return fmt.Errorf("pre-rewrite script %s: missing #! shebang on the first line (the script is exec'd directly so the kernel needs it to choose an interpreter)", path)
	}
	return nil
}

// sanitizedScriptEnv builds the minimal environment handed to user scripts:
// an allowlist of parent vars plus a forced LC_ALL=C.
func sanitizedScriptEnv() []string {
	env := make([]string, 0, len(scriptEnvAllowlist)+1)
	for _, k := range scriptEnvAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "LC_ALL=C")
	return env
}

// gitPipelineEnv returns the environment for the orchestrator's own git
// processes (fast-export, filter-repo). Unlike user scripts these are trusted,
// so they inherit the full parent environment; LC_ALL=C is appended last to
// guarantee byte-faithful, locale-independent stream handling.
func gitPipelineEnv() []string {
	return append(os.Environ(), "LC_ALL=C")
}

// ValidatePreRewriteFlags rejects passthrough flags that are
// incompatible with the pre-rewrite stdin pipeline. The pipeline hardcodes
// `git fast-export --all`, so a ref-narrowing flag would silently fail to
// constrain the exported history and could rewrite more than the operator
// intended.
func ValidatePreRewriteFlags(flags []string) error {
	for _, raw := range flags {
		if flagName(raw) == "--refs" {
			return errors.New("--refs (via --filter-repo-flag) cannot be combined with --pre-rewrite-script: the pre-rewrite pipeline exports the full history with `git fast-export --all`, so --refs would not constrain it; drop one of the two")
		}
	}
	return nil
}

// runPreRewritePipeline wires
//
//	git fast-export <fixed args> | script1 | … | git filter-repo --stdin <filterRepoArgs>
//
// as separate processes connected by OS pipes (no shell), runs them
// concurrently, and reports the FIRST failing stage in pipeline order so a
// genuine upstream error (e.g. a script crash) is surfaced instead of the
// downstream broken-pipe it triggers.
//
// filterRepoArgs is the already-assembled `["filter-repo", "--force", …]`
// argument list from Run; this function prepends the bare-repo config
// guard and appends `--stdin`.
func (r *Runner) runPreRewritePipeline(ctx context.Context, bareRepoPath string, filterRepoArgs, scripts []string) error {
	absScripts, err := ValidatePreRewriteScripts(scripts)
	if err != nil {
		return err
	}

	// Stage commands, in pipeline order.
	exportArgs := append([]string{"-c", "safe.bareRepository=all"}, preRewriteFastExportArgs...)
	frArgs := append([]string{"-c", "safe.bareRepository=all"}, filterRepoArgs...)
	frArgs = append(frArgs, "--stdin")

	type stage struct {
		name string
		cmd  *exec.Cmd
		err  *bytes.Buffer
	}
	stages := make([]stage, 0, len(absScripts)+2)

	feCmd := exec.CommandContext(ctx, r.bin, exportArgs...)
	feCmd.Dir = bareRepoPath
	feCmd.Env = gitPipelineEnv()
	stages = append(stages, stage{name: "git fast-export", cmd: feCmd})

	for _, s := range absScripts {
		sc := exec.CommandContext(ctx, s)
		sc.Dir = bareRepoPath
		sc.Env = sanitizedScriptEnv()
		stages = append(stages, stage{name: "pre-rewrite script " + filepath.Base(s), cmd: sc})
	}

	frCmd := exec.CommandContext(ctx, r.bin, frArgs...)
	frCmd.Dir = bareRepoPath
	frCmd.Env = gitPipelineEnv()
	stages = append(stages, stage{name: "git filter-repo --stdin", cmd: frCmd})

	// Capture each stage's stderr; the last stage's stdout is forwarded to
	// the runner's stdout (filter-repo progress/output).
	for i := range stages {
		buf := &bytes.Buffer{}
		stages[i].err = buf
		stages[i].cmd.Stderr = buf
	}
	stages[len(stages)-1].cmd.Stdout = r.stdout

	// Connect stdout(i) -> stdin(i+1) with OS pipes. Parent-side copies are
	// closed after every process has started so EOF propagates correctly.
	var toClose []*os.File
	defer func() {
		for _, f := range toClose {
			_ = f.Close()
		}
	}()
	for i := 0; i < len(stages)-1; i++ {
		pr, pw, perr := os.Pipe()
		if perr != nil {
			return fmt.Errorf("pre-rewrite pipeline: create pipe: %w", perr)
		}
		stages[i].cmd.Stdout = pw
		stages[i+1].cmd.Stdin = pr
		toClose = append(toClose, pr, pw)
	}

	r.info(fmt.Sprintf("running pre-rewrite pipeline: git %s | %d script(s) | git %s",
		strings.Join(preRewriteFastExportArgs, " "),
		len(absScripts),
		redactForLog(append(filterRepoArgs[1:], "--stdin"))))

	started := 0
	for i := range stages {
		if serr := stages[i].cmd.Start(); serr != nil {
			// Best-effort: kill anything already started before bailing.
			for j := 0; j < started; j++ {
				if stages[j].cmd.Process != nil {
					_ = stages[j].cmd.Process.Kill()
				}
			}
			return fmt.Errorf("pre-rewrite pipeline: start %s: %w", stages[i].name, serr)
		}
		started++
	}

	// All children have inherited the fds they need; drop the parent copies
	// so readers observe EOF when their writer exits.
	for _, f := range toClose {
		_ = f.Close()
	}
	toClose = nil

	// Wait for every stage; record each result.
	waitErrs := make([]error, len(stages))
	for i := range stages {
		waitErrs[i] = stages[i].cmd.Wait()
	}

	// Report the first failing stage in pipeline order.
	for i := range stages {
		if waitErrs[i] != nil {
			return fmt.Errorf("pre-rewrite pipeline: %s failed: %w (stderr=%q)",
				stages[i].name, waitErrs[i], strings.TrimSpace(stages[i].err.String()))
		}
	}
	return nil
}
