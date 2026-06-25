package filterrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// preRewriteFastExportArgs are the fixed git fast-export flags feeding the
// pre-rewrite pipeline:
//
//	--all                always whole-history (--refs is rejected up front)
//	--signed-tags=strip  signatures can't survive a rewrite
//	--show-original-ids  emit original-oid lines so the commit-map is correct
//	--reencode=no        pass message bytes through unchanged
//	--use-done-feature   trailing `done` so a truncated stream fails loudly
var preRewriteFastExportArgs = []string{
	"fast-export",
	"--all",
	"--signed-tags=strip",
	"--show-original-ids",
	"--reencode=no",
	"--use-done-feature",
}

// scriptEnvAllowlist is the set of parent environment variables forwarded to
// user pre-rewrite scripts; everything else (notably the migration PATs) is
// withheld. LC_ALL is forced to C separately, for locale-independent matching.
var scriptEnvAllowlist = []string{"PATH", "HOME", "LANG", "TMPDIR"}

// ValidatePreRewriteScripts resolves each script to an absolute path and
// verifies it is a regular, executable file with a `#!` shebang, so
// misconfiguration fails fast instead of mid-pipeline.
func ValidatePreRewriteScripts(scripts []string) ([]string, error) {
	if len(scripts) == 0 {
		return nil, errors.New("no pre-rewrite scripts supplied")
	}
	if runtime.GOOS == "windows" {
		return nil, errors.New("--pre-rewrite-script is not supported on Windows (it execs shebang scripts, which is POSIX-only); run the migration from Linux, macOS, or WSL")
	}
	abs := make([]string, 0, len(scripts))
	for _, scriptPath := range scripts {
		p, err := filepath.Abs(scriptPath)
		if err != nil {
			return nil, fmt.Errorf("pre-rewrite script %q: %w", scriptPath, err)
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
	for _, key := range scriptEnvAllowlist {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	env = append(env, "LC_ALL=C")
	return env
}

// gitPipelineEnv returns the environment for the orchestrator's own trusted git
// processes: the full parent environment plus a forced LC_ALL=C.
func gitPipelineEnv() []string {
	return append(os.Environ(), "LC_ALL=C")
}

// ValidatePreRewriteFlags rejects passthrough flags incompatible with the
// pre-rewrite pipeline. It hardcodes `git fast-export --all`, so a ref-narrowing
// flag would silently fail to constrain the exported history.
func ValidatePreRewriteFlags(flags []string) error {
	for _, raw := range flags {
		if flagName(raw) == "--refs" {
			return errors.New("--refs (via --filter-repo-flag) cannot be combined with --pre-rewrite-script: the pre-rewrite pipeline exports the full history with `git fast-export --all`, so --refs would not constrain it; drop one of the two")
		}
	}
	return nil
}

// pipelineStage is one process in the fast-export | scripts | filter-repo chain.
type pipelineStage struct {
	name string
	cmd  *exec.Cmd
	err  *cappedBuffer
}

// maxStageStderr caps the stderr retained per stage; 1 MiB is ample to diagnose a failure.
const maxStageStderr = 1 << 20

// cappedBuffer is an io.Writer that keeps at most maxStageStderr bytes, retaining
// the most recent (tail) output where a fatal error message appears.
type cappedBuffer struct {
	buf       []byte
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	if len(c.buf) > maxStageStderr {
		c.truncated = true
		c.buf = c.buf[len(c.buf)-maxStageStderr:] //keep only last 1MiB
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if c.truncated {
		return "…[earlier stderr truncated]\n" + string(c.buf)
	}
	return string(c.buf)
}

// runPreRewritePipeline wires
//
//	git fast-export <fixed args> | script1 | … | git filter-repo --stdin <filterRepoArgs>
//
// as separate processes connected by OS pipes (no shell). filterRepoArgs is the
// assembled list from Run; this prepends the bare-repo config guard and appends
// `--stdin`.
func (r *Runner) runPreRewritePipeline(ctx context.Context, bareRepoPath string, filterRepoArgs, scripts []string) error {
	absScripts, err := ValidatePreRewriteScripts(scripts)
	if err != nil {
		return err
	}

	exportArgs := append([]string{"-c", "safe.bareRepository=all"}, preRewriteFastExportArgs...)
	frArgs := append([]string{"-c", "safe.bareRepository=all"}, filterRepoArgs...)
	frArgs = append(frArgs, "--stdin")

	stages := make([]pipelineStage, 0, len(absScripts)+2)

	// First stage: export the whole history as a fast-import byte stream.
	exportCmd := exec.CommandContext(ctx, r.bin, exportArgs...)
	exportCmd.Dir = bareRepoPath
	exportCmd.Env = gitPipelineEnv()
	stages = append(stages, pipelineStage{name: "git fast-export", cmd: exportCmd})

	// Middle stages: each user script filters the stream in turn.
	for _, scriptPath := range absScripts {
		scriptCmd := exec.CommandContext(ctx, scriptPath)
		scriptCmd.Dir = bareRepoPath
		scriptCmd.Env = sanitizedScriptEnv()
		stages = append(stages, pipelineStage{name: "pre-rewrite script " + filepath.Base(scriptPath), cmd: scriptCmd})
	}

	// Last stage: filter-repo re-imports the repaired stream and rewrites history.
	filterRepoCmd := exec.CommandContext(ctx, r.bin, frArgs...)
	filterRepoCmd.Dir = bareRepoPath
	filterRepoCmd.Env = gitPipelineEnv()
	stages = append(stages, pipelineStage{name: "git filter-repo --stdin", cmd: filterRepoCmd})

	// Capture each stage's stderr (bounded); forward the last stage's stdout.
	for stageIdx := range stages {
		buf := &cappedBuffer{}
		stages[stageIdx].err = buf
		stages[stageIdx].cmd.Stderr = buf
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
	for stageIdx := 0; stageIdx < len(stages)-1; stageIdx++ {
		pr, pw, perr := os.Pipe()
		if perr != nil {
			return fmt.Errorf("pre-rewrite pipeline: create pipe: %w", perr)
		}
		stages[stageIdx].cmd.Stdout = pw
		stages[stageIdx+1].cmd.Stdin = pr
		toClose = append(toClose, pr, pw)
	}

	r.info(fmt.Sprintf("running pre-rewrite pipeline: git %s | %d script(s) | git %s",
		strings.Join(preRewriteFastExportArgs, " "),
		len(absScripts),
		redactForLog(append(append([]string{}, filterRepoArgs...), "--stdin"))))

	started := 0
	for stageIdx := range stages {
		if serr := stages[stageIdx].cmd.Start(); serr != nil {
			// Best-effort: kill anything already started before bailing.
			for startedIdx := 0; startedIdx < started; startedIdx++ {
				if stages[startedIdx].cmd.Process != nil {
					_ = stages[startedIdx].cmd.Process.Kill()
				}
			}
			return fmt.Errorf("pre-rewrite pipeline: start %s: %w", stages[stageIdx].name, serr)
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
	for stageIdx := range stages {
		waitErrs[stageIdx] = stages[stageIdx].cmd.Wait()
	}
	return reportPipelineFailure(stages, waitErrs)
}

// reportPipelineFailure returns the root-cause error among the stage results.
// A SIGPIPE death is usually a victim of a downstream stage exiting early, so it
// prefers the first non-SIGPIPE failure and falls back to a SIGPIPE one only if
// every failure was a broken pipe.
func reportPipelineFailure(stages []pipelineStage, waitErrs []error) error {
	firstFail := -1
	for stageIdx := range stages {
		if waitErrs[stageIdx] == nil {
			continue
		}
		if firstFail == -1 {
			firstFail = stageIdx
		}
		if !isBrokenPipe(waitErrs[stageIdx]) {
			return stageError(stages[stageIdx].name, waitErrs[stageIdx], stages[stageIdx].err)
		}
	}
	if firstFail != -1 {
		return stageError(stages[firstFail].name, waitErrs[firstFail], stages[firstFail].err)
	}
	return nil
}

func stageError(name string, err error, stderr *cappedBuffer) error {
	return fmt.Errorf("pre-rewrite pipeline: %s failed: %w (stderr=%q)",
		name, err, strings.TrimSpace(stderr.String()))
}

// isBrokenPipe reports whether a process was terminated by SIGPIPE, which
// happens when it writes to a pipe whose downstream reader has already exited.
func isBrokenPipe(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGPIPE
}
