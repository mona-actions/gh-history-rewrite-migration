# Stripping large files from history (`--strip-large-files`)

GitHub's Migrations API rejects archives that contain blobs larger than
its size budget, and even archives that import successfully often carry
hundreds of megabytes of historical build artifacts that no one will
ever check out again. `--strip-large-files` is a built-in workflow that
automates the analyze → identify → strip flow inside the
orchestrator.

It is opt-in. By default, `migrate` and `rewrite` do not modify history
unless you ask them to.

---

## How candidates are selected

When `--strip-large-files` is active, the orchestrator runs
`git filter-repo --analyze` against the bare repo, then aggregates blobs
**by path**. A path is flagged when:

```text
max( max_blob_size_at_path, cumulative_size_at_path ) > threshold
```

Both dimensions matter:

- **`max_blob_size_at_path`** catches the obvious case — a single giant
  binary that was committed once.
- **`cumulative_size_at_path`** catches the more common case — a small
  artifact (say, `dist/bundle.js` at 9 MB) regenerated 200 times by CI,
  whose history adds up to nearly 2 GB.

The threshold defaults to **400 MiB**. Tune it with
`--large-file-threshold`:

```bash
gh history-rewrite-migration migrate \
    --org acme --repo legacy --target-org acme-cloud \
    --strip-large-files \
    --large-file-threshold 1G
```

Accepted suffixes: `K`, `M`, `G`, `Ki`, `Mi`, `Gi` (case-insensitive).
`0`, negative values, missing suffixes, and absurdly large values are
rejected with a clear error.

If the analysis finds zero paths above the threshold, the orchestrator
exits cleanly with a "no candidates above threshold" message and skips
the strip step entirely; it does not crash and it does not prompt.

---

## Gate 1: the strip preview

Before any rewrite happens, the orchestrator prints a confirmation
table:

```text
PATH                          MAX BLOB    CUMULATIVE     N    FIRST INTRODUCED
data/backup_1.bak             500.0 MiB     1.5 GiB      3    892d146 (Add database backup)
dist/bundle.js                  9.8 MiB     1.9 GiB    200    abc1234 (CI artifact)
vendor/sdk.zip                350.0 MiB   350.0 MiB      1    777aaaa (Vendor SDK 4.2)
```

| Column | Meaning |
| --- | --- |
| `PATH` | The repo-relative path that will be stripped (across all of history). |
| `MAX BLOB` | Largest single blob ever stored at this path. |
| `CUMULATIVE` | Sum of all blob sizes ever stored at this path (each historical version counts once). |
| `N` | How many distinct blob revisions exist at this path. |
| `FIRST INTRODUCED` | The first commit (short SHA + subject) where this path appeared. |

On a TTY you get an interactive `[y/N]` prompt. In CI / non-TTY
contexts:

- Pass `--yes` to bypass the prompt.
- Pass `--non-interactive` to fail fast (rather than block on stdin)
  if `--yes` is missing.

---

## What actually runs

The orchestrator generates `cleanup.txt` inside `<work-dir>/` —
one path per line — and persists it for auditability:

```bash
$ cat ./work-legacy/cleanup.txt
data/backup_1.bak
dist/bundle.js
vendor/sdk.zip
```

It then invokes a single `git filter-repo` call:

```bash
git filter-repo --force \
    --paths-from-file <work-dir>/cleanup.txt --invert-paths \
    [--commit-callback ...] [--email-callback ...] \
    [user --filter-repo-flag passthrough...]
```

When `--strip-large-files` is combined with `--filter-repo-script` or
`--filter-repo-flag`, **all operations are merged into a single
filter-repo invocation.** Multiple passes would rewrite SHAs multiple
times, breaking the commit-map handoff.

---

## Recovery and resume

`<work-dir>/` is the source of truth. The most common recovery flow:

```bash
# Strip refused mid-run? Inspect what was about to happen.
cat ./work-legacy/cleanup.txt

# Re-run idempotently.
gh history-rewrite-migration migrate \
    --org acme --repo legacy --target-org acme-cloud \
    --work-dir ./work-legacy \
    --strip-large-files \
    --resume
```

`--resume` is the explicit opt-in to re-use prior artifacts in
`<work-dir>/`. Without it, the orchestrator refuses to overwrite a
non-empty work directory. Re-running `rewrite` with **different** flags
than the first run is also refused — you would otherwise end up with a
half-rewritten archive matching neither flag set.

If you want a clean slate, delete the work directory.

---

## Warnings

> **GPG signatures are stripped.** `git filter-repo` does not preserve
> commit signatures. If your organization requires signed commits as a
> compliance gate, plan accordingly — re-signing post-migration is on
> the consumer.

> **LFS interaction.** `--strip-large-files` requires
> `--allow-lfs-rewrite` on LFS-enabled archives. LFS pointer files are
> tiny and will not trip the threshold, but if real binaries leaked
> into git history (LFS misconfig), they **will** be stripped. LFS
> objects under `git-lfs/` live outside the bare repo — `filter-repo`
> cannot touch them, so any payload referenced by a stripped path
> becomes an orphan that the importer may reject.

> **Forks and consumers will diverge.** Rewriting history changes every
> commit SHA from the strip point forward. Anyone who pushed work back
> to the source repo (or maintains a fork) must re-clone after
> migration; their existing branches will not fast-forward into the
> rewritten target.
