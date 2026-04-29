# Manual remap workaround (temporary)

> **Status:** This page describes a stop-gap for the v1 release. It
> will be removed once `mona-actions/gh-commit-remap` ships its `pkg/`
> refactor and the orchestrator's `remap-real-impl` lands.

## What is broken

`migrate` runs `export → rewrite → remap → import`. The `remap` step
rewrites old → new SHAs inside the metadata JSONs (issues, PRs, issue
events) using the commit-map that `git filter-repo` emits.

The remap implementation lives in `mona-actions/gh-commit-remap`, but
that project does not yet expose a stable Go `pkg/` API. Until it does,
the orchestrator's `Remapper.Run()` aborts cleanly with
`ErrUpstreamPending` rather than guess at the contract. End-to-end
`migrate` therefore fails partway through with a clear error.

## What still works

- `export` runs end-to-end and downloads the combined archive.
- `rewrite` runs end-to-end. It produces:
  - the rewritten bare repo at `<work-dir>/extracted/.../<RepoName>.git/`,
  - `<work-dir>/cleanup.txt` (when `--strip-large-files` is used),
  - `<work-dir>/commit-map` (the SHA mapping `gh-commit-remap` consumes).
- `import` runs end-to-end **if you hand it the two archives that
  `gh-commit-remap` produced**.

## The workaround, end-to-end

Run the orchestrator up through `rewrite`, then invoke `gh-commit-remap`
yourself, then run `import` standalone pointing at the archives
`gh-commit-remap` wrote.

```bash
WORKDIR=./work-legacy
export GH_SOURCE_PAT=ghp_xxx_source
export GH_PAT=ghp_xxx_target

# 1. Export + rewrite (skip the broken remap step by stopping here).
gh history-rewrite-migration export \
    --org acme --repo legacy-monorepo \
    --work-dir "$WORKDIR"

gh history-rewrite-migration rewrite \
    --work-dir "$WORKDIR" \
    --strip-large-files \
    --yes

# 2. Run gh-commit-remap manually against the work-dir.
#    Refer to gh-commit-remap's own README for the exact invocation
#    your installed version expects. The inputs it needs are:
#      - the extracted archive tree under "$WORKDIR/extracted/"
#      - the commit-map at "$WORKDIR/commit-map"
#    Its outputs must be a (git_archive, metadata_archive) pair.
gh-commit-remap \
    --extracted-dir "$WORKDIR/extracted" \
    --commit-map   "$WORKDIR/commit-map" \
    --git-out      "$WORKDIR/git_archive.tar.gz" \
    --metadata-out "$WORKDIR/metadata_archive.tar.gz"

# 3. Import standalone, pointing at gh-commit-remap's outputs.
gh history-rewrite-migration import \
    --org acme --repo legacy-monorepo \
    --target-org acme-cloud \
    --target-repo legacy-monorepo \
    --work-dir "$WORKDIR" \
    --confirm
```

`import` reads `<work-dir>/git_archive.tar.gz` and
`<work-dir>/metadata_archive.tar.gz` by default — the same paths the
final orchestrated pipeline writes. As long as `gh-commit-remap` lands
its outputs at those filenames, no extra flags are needed; if it writes
elsewhere, copy or symlink the files into place before invoking
`import`.

## Why we are not auto-installing or scripting around `gh-commit-remap`

The current `gh-commit-remap` exposes only a `cmd/` entry point — its
internals (`archive.Extract`, `archive.ReTarTo`, `ProcessFiles`) are
not stable API. Two integration questions are open:

1. Does `archive.ReTarTo` produce **one** combined tarball or **two**
   split tarballs? `gh gei migrate-repo` wants two, so the
   orchestrator may need a small post-remap split step.
2. What is the canonical signature of `ProcessFiles` once `pkg/` is
   exported?

Rather than ship a brittle exec wrapper that we will throw away in two
weeks, the orchestrator surfaces `ErrUpstreamPending` and points users
here. Once `gh-commit-remap` publishes `pkg/`, the `Remapper` interface
swaps in the real implementation, this document is deleted, and
`migrate` works end-to-end.
