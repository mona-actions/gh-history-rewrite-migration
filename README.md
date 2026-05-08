# gh-history-rewrite-migration

> An end-to-end orchestrator for GitHub repo migrations that need git history rewritten before import — large-file removal, callback-based rewrites, or both.

`gh-history-rewrite-migration` wraps the `export → rewrite → remap → import` pipeline behind a single `migrate` command, with two interactive confirmation gates protecting the destructive operations. Under the hood it talks to the GitHub Migrations REST API, drives `git filter-repo`, remaps metadata with `gh-commit-remap`, and imports the rewritten local archives into the target organization via `gh gei migrate-repo`.

---

## Installation

```bash
gh extension install mona-actions/gh-history-rewrite-migration
```

Verify:

```bash
gh history-rewrite-migration --help
```

---

## Prerequisites

| Tool / value | Why | How to provide |
| --- | --- | --- |
| `gh` CLI | Extension host | https://cli.github.com |
| `gh-gei` extension (≥ **v1.10.0**) | Imports the rewritten archive via the hidden `--git-archive-path` / `--metadata-archive-path` flags | `gh extension install github/gh-gei` |
| `git filter-repo` | Performs the actual history rewrite | https://github.com/newren/git-filter-repo (must be on `PATH`) |
| `tar` | Extracts and re-tars archives | Standard on macOS / Linux |
| `GH_SOURCE_PAT` | Source PAT, `admin:org` on the source org (read access to the repo, plus the migrations API) | Environment variable |
| `GH_PAT` | Target PAT, `admin:org` on the target GHEC org | Environment variable |

Run the preflight checker before your first migration:

```bash
gh history-rewrite-migration doctor
```

`doctor` verifies all of the above, plus work-dir writability and free disk space (extraction + rewrite + retar typically needs **3–4× the archive size**).

---

## Quick start

Migrate `acme/legacy-monorepo` from `GHEC` into `acme-cloud` on GHEC-EMU, stripping any file paths whose blobs exceed 400 MB along the way:

```bash
export GH_SOURCE_PAT=ghp_xxx_source
export GH_PAT=ghp_xxx_target

gh history-rewrite-migration migrate \
    --org acme \
    --repo legacy-monorepo \
    --target-org acme-cloud \
    --work-dir ./work-legacy-monorepo \
    --strip-large-files
```

You will be prompted twice:

1. **Gate 1 — pre-strip.** A table of candidate paths (with `MAX BLOB` and `CUMULATIVE` columns) is printed; confirm to rewrite history. Bypass non-interactively with `--yes`.
2. **Gate 2 — pre-import.** A post-rewrite summary is printed; confirm to push the rewritten archive into the target org. Bypass non-interactively with `--confirm`.

For CI / scripted use, pair `--non-interactive` with `--yes` and `--confirm` so the run fails fast instead of blocking on stdin:

```bash
gh history-rewrite-migration migrate \
    --org acme --repo legacy-monorepo --target-org acme-cloud \
    --strip-large-files \
    --non-interactive --yes --confirm
```

---

## Pipeline

```text
┌────────────┐     ┌────────────────┐     ┌───────────────────┐     ┌──────────────┐
│  Export    │ ──► │ Rewrite        │ ──► │ Remap             │ ──► │ Import       │
│ migrations │     │ git filter-repo│     │ gh-commit-remap   │     │ gh gei       │
└─────┬──────┘     └───────┬────────┘     └─────────┬─────────┘     └──────┬───────┘
      │                    │                        │                      │
      ▼                    ▼                        ▼                      ▼
raw git + metadata   rewritten git archive   metadata JSON SHAs      target repo on
archives in work-dir + commit-map            rewritten from map       GHEC
```

The end-to-end flow is:

1. **Export** from the source org with the GitHub Migrations REST API.
2. **Rewrite** the extracted bare repository with `git filter-repo` and copy its `commit-map` into the work directory.
3. **Remap** metadata JSON files with `gh-commit-remap`, replacing old commit SHAs with rewritten SHAs from the `commit-map`.
4. **Import** the final `git_archive.tar.gz` and `metadata_archive.tar.gz` with `gh gei migrate-repo --git-archive-path ... --metadata-archive-path ...`.

---

## Project structure

```text
.
├── cmd/                    # Cobra command definitions (root, export, rewrite, import, migrate, doctor)
├── internal/
│   ├── api/               # Authenticated GitHub API clients for GHES and GHEC
│   ├── atomicfs/          # Crash-safe file ops (write-tmp-then-rename) and sentinel tracking
│   ├── doctor/            # Preflight checks for tool dependencies and connectivity
│   ├── exporter/          # Orchestrates the export phase via the Migrations REST API
│   ├── filterrepo/        # Wraps the git filter-repo external tool
│   ├── importer/          # Wraps gh gei migrate-repo for archive import
│   ├── largefiles/        # Analyze → flag → cleanup workflow for oversized blobs
│   ├── migrate/           # End-to-end orchestrator (export → rewrite → remap → import)
│   ├── output/            # Structured console output helpers (tables, summaries)
│   ├── remap/             # Rewrites commit SHA references in metadata JSONs
│   ├── rewriter/          # Orchestrates the history-rewrite phase
│   └── workdir/           # Manages on-disk directory layout for a single migration
├── docs/                   # Extended documentation (large-files, callbacks, verification)
├── examples/               # Runnable callback script examples
└── main.go                 # Entry point
```

---

## Export modes

`--export-mode` controls how the source archives are produced:

| Mode | Behavior | Tradeoff |
| --- | --- | --- |
| `two` (default) | Runs two separate migration API calls: git-only and metadata-only. | Format-safe by construction because the local archives match the split archives `gh gei` itself produces. |
| `combined` | Runs one migration API call and splits the combined archive locally. | Reduces source-side migration load, but relies on the local splitter to recreate the split archive shape. |

Both modes normalize into the same downstream work-dir layout. The selected mode is persisted in `<work-dir>/.export-mode`; resuming with a different `--export-mode` is rejected so a partially completed work directory cannot be silently mixed across archive formats.

---

## Work directory layout

A completed v2 work directory contains:

```text
<work-dir>/
├── git_archive_raw.tar.gz       # raw git-only export
├── metadata_archive_raw.tar.gz  # raw metadata-only export
├── git_extracted/               # extracted git archive; contains repositories/<org>/<repo>.git/
├── metadata_extracted/          # extracted metadata archive; contains metadata JSONs
├── git_archive.tar.gz           # final rewritten git archive for gh gei import
├── metadata_archive.tar.gz      # final remapped metadata archive for gh gei import
├── commit-map                   # git filter-repo old-sha -> new-sha map
└── .export-mode                 # export mode used to create this work-dir
```

`git_extracted/` and `metadata_extracted/` may also contain sentinel files used for resumability. Treat the entire work directory as sensitive migration data.

---

## The two confirmation gates

| Gate | When | Skipped by | Forced-error in CI by |
| --- | --- | --- | --- |
| **Gate 1** — strip preview | After `git filter-repo --analyze`, before any history rewrite | `--yes` | `--non-interactive` (without `--yes`) |
| **Gate 2** — pre-import | After `rewrite` + `remap`, before `gh gei migrate-repo` | `--confirm` | `--non-interactive` (without `--confirm`) |

`--non-interactive` is a CI convenience: it converts any prompt that would otherwise block on stdin into an immediate error, so a misconfigured pipeline does not hang. Standalone `import` always requires `--confirm` when run without a TTY.

---

## Subcommands

| Command | Role |
| --- | --- |
| `migrate` | **Primary.** Runs `export → rewrite → remap → import` end-to-end with both confirmation gates. |
| `export` | Advanced. Downloads raw git and metadata archives from the source org. Honors `--export-mode two` and `--export-mode combined`. |
| `rewrite` | Advanced. Runs `git filter-repo` against the extracted bare repo (`--strip-large-files`, `--filter-repo-script`, `--filter-repo-flag`). |
| `import` | Advanced. Pushes a previously rewritten git+metadata archive pair into the target GHEC org via `gh gei migrate-repo`. |
| `doctor` | Preflight checks (binaries, versions, env vars, source reachability, disk space). |

Run `gh history-rewrite-migration <command> --help` for the full flag surface.

---

## Limitations

- **Single-repo only.** Multi-repo migration archives are rejected. Run the orchestrator once per repository.
- **Target is always GHEC (`github.com`).** `gh gei migrate-repo` does not support GHES targets, so this orchestrator does not expose a `--target-hostname` flag. Sources may be GHEC or GHES (set `--source-hostname` for GHES).
- **Upstream `gh-commit-remap` prefix list is incomplete.** Upstream's known SHA-bearing metadata prefixes do not currently include every file where commit SHAs may appear. This project extends the list in `internal/remap` via `SHABearingPrefixes`.
- **Upstream `gh-commit-remap` scans top-level metadata files only.** This project works around that by recursively discovering metadata roots with `FindMetadataDirs` before invoking the remapper.
- **`filter-repo` strips GPG signatures silently.** This is upstream behavior; signed-commit compliance teams should be aware. The post-rewrite summary surfaces whether signed commits were detected.
- **LFS pointer mappings are not rewritten.** LFS objects live outside the bare repo, so `filter-repo` cannot touch them. When `--strip-large-files` would touch an LFS-enabled archive, `--allow-lfs-rewrite` is required and the orchestrator surfaces a prominent warning.
- **Pushed forks downstream of rewritten history will diverge.** History rewriting changes every commit SHA from the strip point forward; consumers of the source repo must re-clone.

---

## Documentation

- [`docs/large-files.md`](docs/large-files.md) — `--strip-large-files` walkthrough, threshold tuning, the Gate 1 preview, recovery.
- [`docs/callback-scripts.md`](docs/callback-scripts.md) — the eight callback-script suffixes, validation rules, raw-flag passthrough.
- [`docs/manual-verification.md`](docs/manual-verification.md) — gei-import smoke test for validating archive compatibility beyond unit tests.
- [`examples/scripts/`](examples/scripts/) — runnable callback examples.

---

## Security

- **PATs are passed via environment variables** (`GH_SOURCE_PAT`, `GH_PAT`) and never via argv. Process listings (`ps`, `/proc`) will not leak credentials.
- **Callback script bodies are never logged** — only the script path and the upstream `filter-repo` stderr are surfaced on errors.
- **Do not commit your callback scripts to a public repo if they contain sensitive logic** (internal hostnames, employee email patterns, redaction rules). Treat them like any other piece of migration tooling.
- The work directory contains the extracted source repository and intermediate archives; clean it up (or store it in an encrypted volume) when done.

---

## License

MIT — see [LICENSE](LICENSE).
