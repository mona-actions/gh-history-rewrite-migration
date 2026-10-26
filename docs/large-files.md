# Stripping large files from history (`--strip-large-files`)

GitHub rejects imports containing any single file larger than **400 MiB**.
`--strip-large-files` removes oversized blobs from history before import.
It is opt-in; without it, history is not modified.

> [!IMPORTANT]
> **Behavior change.** `--strip-large-files` used to run
> `git filter-repo --analyze` and strip whole paths. It now strips only
> blobs larger than the threshold. Add `--analyze` for the old behavior.

## Modes

| Flags | Behavior |
| --- | --- |
| `--strip-large-files` | **Size mode (default).** Removes only blob versions larger than the threshold. |
| `--strip-large-files --analyze` | **Path mode.** Runs `git filter-repo --analyze` and removes every flagged path from all of history. Slow on very large repos. |
| `--analyze` | **Report only.** Prints flagged paths; changes nothing. |

Size mode is the default because GitHub's limit is per file, and it skips
`--analyze`, which can take hours on very large repositories.

## Threshold

Defaults to **400 MiB**. Set it with `--large-file-threshold` (e.g. `1G`).

- Suffixes `K`, `M`, `G` (and `Ki`, `Mi`, `Gi`) are 1024-based; fractions like `1.5G` work.
- A blob is stripped only if it is **strictly larger** than the threshold.
- Zero, negative, or unparsable values are rejected.

## Size mode

1. A fast `git cat-file --batch-all-objects` scan counts blobs over the threshold.
   If there are none, the strip is skipped.
2. Oversized blobs are written to `<work-dir>/large-blobs.txt` (`<sha>\t<size>`)
   and shown in the [Gate 1 preview](#gate-1-preview).
3. After confirmation, filter-repo runs with `--strip-blobs-bigger-than <bytes>`.

Only oversized versions are removed: if `video.mp4` was committed at 50 MiB and
later at 600 MiB, only the 600 MiB version goes.

Caveats:

- The count is an **upper bound**; the scan includes unreachable blobs.
- The preview shows SHAs and sizes, not paths. Use `--analyze` to see paths.
- With `--pre-rewrite-script`, the scan runs before your scripts; filter-repo
  applies the threshold to the post-script data.

## Path mode (`--strip-large-files --analyze`)

Runs `git filter-repo --analyze` and flags a path when:

```text
max(max_blob_size_at_path, cumulative_size_at_path) > threshold
```

The cumulative rule catches small artifacts regenerated many times (e.g. a
9 MB `dist/bundle.js` committed 200 times). Flagged paths are removed from
**all** of history, including small versions. Paths are written to
`<work-dir>/cleanup.txt` and stripped with `--invert-paths --paths-from-file`.
If nothing is flagged, the strip is skipped without prompting.

## Report only (`--analyze`)

Prints the flagged-path table and writes the full report to
`<work-dir>/analysis/`. No rewrite, archive, commit-map, or prompt.
`rewrite` exits after the report; `migrate` exits `0` after export. Continue
with `--resume`, adding `--analyze` for path mode:

```bash
gh history-rewrite-migration migrate --org acme --repo legacy \
    --target-org acme-cloud --work-dir ./work-legacy --analyze

gh history-rewrite-migration migrate --org acme --repo legacy \
    --target-org acme-cloud --work-dir ./work-legacy \
    --strip-large-files --resume
```

Report-only mode rejects `--filter-repo-script`, `--filter-repo-flag`, and
`--pre-rewrite-script`.

## Gate 1 preview

Before rewriting, the orchestrator shows what it will strip and asks to confirm.

- **Size mode:** threshold, blob count (upper bound), largest blob, total size.
- **Path mode:** a table of `path`, `max-blob`, `cumulative`, and `reason`
  (`single blob`, `cumulative`, or `both`).

In CI, pass `--yes` to skip the prompt, or `--non-interactive` to fail instead
of waiting on stdin.

## Combined with other options

The strip, `--filter-repo-script`, and `--filter-repo-flag` run in **one**
filter-repo pass so commit SHAs are rewritten once. Conflicting passthrough
flags are rejected:

- Size mode: `--strip-blobs-bigger-than` (use `--large-file-threshold`).
- Path mode: `--path`, `--paths`, `--path-glob`, `--path-regex`,
  `--paths-from-file`, `--invert-paths`.
- Always: `--analyze` (use the tool's `--analyze` flag).

See [`callback-scripts.md`](callback-scripts.md#raw-passthrough---filter-repo-flag).

## Resume

`<work-dir>/` is the source of truth. Re-run with `--resume` to reuse it;
without it, a non-empty work dir is refused. Re-running with different flags
is also refused. Delete the work dir for a clean slate.

## Warnings

- **GPG signatures are lost.** filter-repo does not preserve commit signatures.
- **LFS.** Requires `--allow-lfs-rewrite` on LFS archives. Pointer files are
  tiny, but real binaries leaked into git history will be stripped, and LFS
  objects they referenced may be orphaned.
- **SHAs change.** Forks and existing clones must re-clone after migration.
