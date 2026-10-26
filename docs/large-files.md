# Stripping large files from history (`--strip-large-files`)

GitHub rejects imports that contain any single file larger than
**400 MiB**, and even archives that import successfully often carry
hundreds of megabytes of historical build artifacts that no one will
ever check out again. `--strip-large-files` is a built-in workflow that
finds oversized blobs and removes them from history inside the
orchestrator.

It is opt-in. By default, `migrate` and `rewrite` do not modify history
unless you ask them to.

> [!IMPORTANT]
> **Behavior change.** Earlier versions of `--strip-large-files` always
> ran `git filter-repo --analyze` and stripped whole paths across
> history. `--strip-large-files` now strips only the individual blobs
> that are larger than the threshold. To keep the earlier path-based
> behavior, add `--analyze`.

---

## Choose a mode

| Flags | Behavior |
| --- | --- |
| (none) | No history changes. |
| `--strip-large-files` | **Size mode (default).** Scans blob sizes, then removes only the blob versions larger than the threshold. Smaller versions of the same path are kept. |
| `--strip-large-files --analyze` | **Path mode (previous behavior).** Runs `git filter-repo --analyze` and removes every flagged path across all of history. Slower on very large repositories. |
| `--analyze` | **Report only.** Runs `git filter-repo --analyze` and prints the flagged paths. Makes no changes to history. |

Size mode is the default because GitHub's import limit applies to each
file, so removing the oversized blobs is enough. It also avoids
`git filter-repo --analyze`, which can be very slow. On one repository
with 368k commits, 2.36M trees, 1.27M blobs (245 GiB), and 90k refs,
`--analyze` took about 68 minutes on a workstation, and ran for more than
3.5 hours on a GitHub Actions larger runner before the runner was
stopped.

---

## Set the threshold

The threshold defaults to **400 MiB**. Tune it with
`--large-file-threshold`:

```bash
gh history-rewrite-migration migrate \
    --org acme --repo legacy --target-org acme-cloud \
    --strip-large-files \
    --large-file-threshold 1G
```

- Accepted suffixes: `K`, `M`, `G`, `Ki`, `Mi`, `Gi` (case-insensitive).
  All suffixes are 1024-based, so `1G` and `1Gi` both mean 1,073,741,824
  bytes.
- Fractional values such as `1.5G` are accepted. The orchestrator
  converts the threshold to an exact byte count before it passes the
  value to `git filter-repo`, which only accepts whole numbers.
- A blob is stripped only if it is **strictly larger** than the
  threshold.
- `0`, negative values, missing numbers, and values that overflow are
  rejected with an error.

---

## Size mode (default)

With `--strip-large-files` alone, the orchestrator:

1. Runs a quick pre-scan with
   `git cat-file --batch-all-objects --batch-check` and counts blobs
   larger than the threshold.
2. If no blob is larger than the threshold, skips the strip. No rewrite
   runs unless you also passed other rewrite options, such as
   `--filter-repo-script`, `--filter-repo-flag`, or
   `--pre-rewrite-script`.
3. Otherwise, writes the oversized blobs to
   `<work-dir>/large-blobs.txt` (one `<sha>\t<size>` line per blob) for
   auditing, and shows the [Gate 1 summary](#gate-1-the-strip-preview).
4. After you confirm, runs one `git filter-repo` pass with
   `--strip-blobs-bigger-than <bytes>`.

Only the oversized blob versions are removed. If `assets/video.mp4` was
committed once at 50 MiB and later at 600 MiB, only the 600 MiB version
is stripped.

Keep these limits of the size-mode preview in mind:

- **The count is an upper bound.** The pre-scan includes unreachable
  blobs that filter-repo will never see, so Gate 1 reports "up to N
  blobs." In the worst case, a rewrite runs and strips nothing.
- **It shows blob SHAs and sizes, not paths.** If you need to know which
  paths are affected before you rewrite, run with `--analyze` first.
- **Pre-rewrite scripts run after the scan.** With
  `--pre-rewrite-script`, the pre-scan measures blob sizes before your
  scripts run. filter-repo applies the threshold to the blob data it
  receives after the scripts.

---

## Path mode (`--strip-large-files --analyze`)

Path mode is the previous default. The orchestrator runs
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

Every flagged path is removed from **all** of history, including
versions that are smaller than the threshold. Use path mode when you
want to drop artifacts like `dist/bundle.js` entirely, not just the
versions that exceed GitHub's per-file limit.

If the analysis finds no paths above the threshold, the orchestrator
logs that there is nothing to strip and skips the strip step; it does
not fail and it does not prompt.

---

## Report only (`--analyze`)

`--analyze` without `--strip-large-files` runs `git filter-repo
--analyze`, prints the flagged-path table, and prints the location of
the full report in `<work-dir>/analysis/`. It makes no changes: there is
no rewritten archive, no `commit-map`, and no Gate 1 prompt.

- With `rewrite`, the command exits after printing the report.
- With `migrate`, the orchestrator exports, analyzes, prints the report,
  and exits with status `0` before the rewrite and import steps. To
  continue, re-run with `--resume --strip-large-files`, adding
  `--analyze` if you want path mode.
- `--analyze` alone is rejected when combined with
  `--filter-repo-script`, `--filter-repo-flag`, or
  `--pre-rewrite-script`, because those options would require a
  rewrite.

```bash
# Inspect, then strip with the same work directory.
gh history-rewrite-migration migrate \
    --org acme --repo legacy --target-org acme-cloud \
    --work-dir ./work-legacy \
    --analyze

gh history-rewrite-migration migrate \
    --org acme --repo legacy --target-org acme-cloud \
    --work-dir ./work-legacy \
    --strip-large-files \
    --resume
```

Analysis reports are written to `<work-dir>/analysis/`, not inside the
bare repo. You can re-run the analysis, and the report is not included
in the migrated archive.

---

## Gate 1: the strip preview

Before any rewrite happens, the orchestrator shows what it is about to
strip and asks for confirmation.

**Size mode** prints a summary:

| Field | Meaning |
| --- | --- |
| Threshold | The `--large-file-threshold` value. |
| Blobs over threshold | Number of blobs larger than the threshold (an upper bound). |
| Largest blob | Size of the largest blob over the threshold. |
| Total | Combined size of the blobs over the threshold. |

It then prompts:

```text
Strip up to N blobs larger than X from history? This rewrites all commits.
```

**Path mode** prints a table of flagged paths:

```text
path                  max-blob     cumulative   reason
data/backup_1.bak     500.0 MiB    1.5 GiB      both
dist/bundle.js        9.8 MiB      1.9 GiB      cumulative
vendor/sdk.zip        450.0 MiB    450.0 MiB    single blob
```

| Column | Meaning |
| --- | --- |
| `path` | The repo-relative path that will be stripped (across all of history). |
| `max-blob` | Largest single blob ever stored at this path. |
| `cumulative` | Sum of all blob sizes ever stored at this path (each historical version counts once). |
| `reason` | Why the path was flagged: `single blob`, `cumulative`, or `both`. |

It then prompts:

```text
Strip these N paths from history? This rewrites all commits.
```

On a TTY you get an interactive `[y/N]` prompt. In CI / non-TTY
contexts:

- Pass `--yes` to bypass the prompt.
- Pass `--non-interactive` to fail fast (rather than block on stdin)
  if `--yes` is missing.

---

## What actually runs

**Size mode** writes `<work-dir>/large-blobs.txt` for auditing:

```bash
$ cat ./work-legacy/large-blobs.txt
a1f3c9e27b4d6058e1c2f7a9b3d5e8c0f4a6b2d1	524288000
9f2c4d1a7e0b5c3f6a8d2e4b1c7f0a9d8e6b5c4a	1610612736
```

It then invokes a single `git filter-repo` call:

```bash
git filter-repo --force \
    --strip-blobs-bigger-than <threshold-bytes> \
    [--commit-callback ...] [--email-callback ...] \
    [user --filter-repo-flag passthrough...]
```

**Path mode** writes `<work-dir>/cleanup.txt`, one path per line:

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

Some passthrough flags are restricted so they cannot conflict with the
strip:

- In size mode, `--filter-repo-flag --strip-blobs-bigger-than` is
  rejected. Use `--large-file-threshold` instead. Path-selection flags
  such as `--path` and `--invert-paths` are allowed.
- In path mode, the path-selection flags (`--path`, `--paths`,
  `--path-glob`, `--path-regex`, `--paths-from-file`, `--invert-paths`)
  are rejected.
- `--filter-repo-flag --analyze` is always rejected. Use the tool's
  `--analyze` flag.

See [`docs/callback-scripts.md`](callback-scripts.md#raw-passthrough---filter-repo-flag)
for the full list.

---

## Recovery and resume

`<work-dir>/` is the source of truth. The most common recovery flow:

```bash
# Strip refused mid-run? Inspect what was about to happen.
cat ./work-legacy/large-blobs.txt   # size mode
cat ./work-legacy/cleanup.txt       # path mode

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
> cannot touch them, so any payload referenced by a stripped blob or
> path becomes an orphan that the importer may reject.

> **Forks and consumers will diverge.** Rewriting history changes every
> commit SHA from the strip point forward. Anyone who pushed work back
> to the source repo (or maintains a fork) must re-clone after
> migration; their existing branches will not fast-forward into the
> rewritten target.
