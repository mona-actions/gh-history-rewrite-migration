# gh-history-rewrite-migration

> An end-to-end orchestrator for GitHub repo migrations that need git history rewritten before import — large-file removal, callback-based rewrites, or both.

> **Status: v1 — single-repo migrations.** Multi-repo support and full remap automation are pending the upstream `mona-actions/gh-commit-remap` `pkg/` release. The orchestrator currently aborts cleanly at the remap step with `ErrUpstreamPending`; see [Limitations](#limitations) and [`docs/manual-remap.md`](docs/manual-remap.md) for the supported workaround.

`gh-history-rewrite-migration` wraps the `export → rewrite → remap → import`
pipeline behind a single `migrate` command, with two interactive
confirmation gates protecting the destructive operations. Under the hood
it talks to the GitHub Migrations REST API, drives `git filter-repo`,
hands off to `gh-commit-remap`, and pushes the rewritten archive into the
target organization via `gh gei migrate-repo`.

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

`doctor` verifies all of the above, plus work-dir writability and free
disk space (extraction + rewrite + retar typically needs **3–4× the
archive size**).

---

## Quick start

Migrate `acme/legacy-monorepo` from `github.com` into `acme-cloud` on
GHEC, stripping any file paths whose blobs exceed 400 MB along the way:

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

1. **Gate 1 — pre-strip.** A table of candidate paths (with `MAX BLOB` and
   `CUMULATIVE` columns) is printed; confirm to rewrite history. Bypass
   non-interactively with `--yes`.
2. **Gate 2 — pre-import.** A post-rewrite summary is printed; confirm
   to push the rewritten archive into the target org. Bypass
   non-interactively with `--confirm`.

For CI / scripted use, pair `--non-interactive` with `--yes` and
`--confirm` so the run fails fast instead of blocking on stdin:

```bash
gh history-rewrite-migration migrate \
    --org acme --repo legacy-monorepo --target-org acme-cloud \
    --strip-large-files \
    --non-interactive --yes --confirm
```

---

## The two confirmation gates

| Gate | When | Skipped by | Forced-error in CI by |
| --- | --- | --- | --- |
| **Gate 1** — strip preview | After `git filter-repo --analyze`, before any history rewrite | `--yes` | `--non-interactive` (without `--yes`) |
| **Gate 2** — pre-import | After `rewrite` + `remap`, before `gh gei migrate-repo` | `--confirm` | `--non-interactive` (without `--confirm`) |

`--non-interactive` is a CI convenience: it converts any prompt that
would otherwise block on stdin into an immediate error, so a misconfigured
pipeline does not hang. Standalone `import` always requires `--confirm`
when run without a TTY.

---

## Subcommands

| Command | Role |
| --- | --- |
| `migrate` | **Primary.** Runs `export → rewrite → remap → import` end-to-end with both confirmation gates. |
| `export` | Advanced. Downloads a combined migration archive from the source org via the REST migrations API and extracts it in place. |
| `rewrite` | Advanced. Runs `git filter-repo` against the extracted bare repo (`--strip-large-files`, `--filter-repo-script`, `--filter-repo-flag`). |
| `import` | Advanced. Pushes a previously rewritten git+metadata archive pair into the target GHEC org via `gh gei migrate-repo`. |
| `doctor` | Preflight checks (binaries, versions, env vars, source reachability, disk space). |

Run `gh history-rewrite-migration <command> --help` for the full flag
surface.

---

## Architecture

```text
   ┌──────────────────┐                       source org / GHEC or GHES
   │ GH_SOURCE_PAT    │                       │
   └─────────┬────────┘                       ▼
             │                ┌──────────────────────────────┐
             │   1. export    │ POST /orgs/{org}/migrations  │
             ▼   ──────────►  │ poll → 302 archive download  │
   ┌──────────────────┐       └────────────────┬─────────────┘
   │  <work-dir>/     │                        │
   │  archive.tar.gz  │ ◄──────────────────────┘
   └─────────┬────────┘
             │  2. extract
             ▼
   ┌──────────────────┐
   │ extracted/       │   <RepoName>.git/  + metadata JSONs (+ git-lfs/)
   └─────────┬────────┘
             │  3. rewrite      git filter-repo --analyze
             │                  + cleanup.txt (strip)
             │                  + user *.commit-callback.py / *.email-callback.py / …
             │                  + raw --filter-repo-flag passthrough
             ▼                  → <bare>/filter-repo/commit-map
   ┌──────────────────┐
   │ commit-map       │ copied to <work-dir>/commit-map
   └─────────┬────────┘
             │  4. remap         gh-commit-remap rewrites SHAs
             │                   inside the metadata JSONs and
             │                   re-tars two archives
             ▼
   ┌──────────────────────┐    ┌───────────────────────────┐
   │ git_archive.tar.gz   │    │ metadata_archive.tar.gz   │
   └──────────┬───────────┘    └─────────────┬─────────────┘
              │  5. import (Gate 2)          │
              └───────┬──────────────────────┘
                      ▼
        gh gei migrate-repo \
            --github-target-org <target>  --target-repo <repo> \
            --git-archive-path … --metadata-archive-path …
                      │
                      ▼
                target org on GHEC (github.com)
```

Notes:

- The migrations API delivers a **single** combined tarball; the split
  into `git_archive.tar.gz` + `metadata_archive.tar.gz` happens during
  remap so the importer can use `gh gei`'s hidden local-archive flags.
- `filter-repo` only ever touches `<RepoName>.git/`. Metadata JSONs are
  rewritten by `gh-commit-remap` using the `commit-map` filter-repo emits.
- The bare repo is discovered (the unique `*.git` directory under
  `extracted/`); the GitHub-emitted prefix-dir scheme is not assumed.

---

## Limitations

- **Single-repo only in v1.** Multi-repo archives are rejected. Run the
  orchestrator once per repo; multi-repo orchestration is on the roadmap.
- **Target is always GHEC (`github.com`).** `gh gei migrate-repo` does
  not support GHES targets, so this orchestrator does not expose a
  `--target-hostname` flag. Sources may be GHEC or GHES (set
  `--source-hostname` for GHES).
- **`gh-commit-remap` real implementation pending upstream `pkg/`
  release.** The orchestrator currently aborts cleanly at the remap step
  with `ErrUpstreamPending`. A documented stop-gap exists for users who
  need to ship today — see [`docs/manual-remap.md`](docs/manual-remap.md).
- **`filter-repo` strips GPG signatures silently.** This is upstream
  behavior; signed-commit compliance teams should be aware. The
  post-rewrite summary surfaces whether signed commits were detected.
- **LFS pointer mappings are not rewritten.** LFS objects live under
  `git-lfs/` outside the bare repo, so `filter-repo` cannot touch them.
  When `--strip-large-files` would touch an LFS-enabled archive,
  `--allow-lfs-rewrite` is required and the orchestrator surfaces a
  prominent warning.
- **Pushed forks downstream of rewritten history will diverge.** History
  rewriting changes every commit SHA from the strip point forward;
  consumers of the source repo must re-clone.

---

## Documentation

- [`docs/large-files.md`](docs/large-files.md) — `--strip-large-files`
  walkthrough, threshold tuning, the Gate 1 preview, recovery.
- [`docs/callback-scripts.md`](docs/callback-scripts.md) — the eight
  callback-script suffixes, validation rules, raw-flag passthrough.
- [`docs/manual-remap.md`](docs/manual-remap.md) — temporary stop-gap
  for the upstream `gh-commit-remap` blocker.
- [`examples/scripts/`](examples/scripts/) — runnable callback examples.

---

## Security

- **PATs are passed via environment variables** (`GH_SOURCE_PAT`,
  `GH_PAT`) and never via argv. Process listings (`ps`, `/proc`) will
  not leak credentials.
- **Callback script bodies are never logged** — only the script path
  and the upstream `filter-repo` stderr are surfaced on errors.
- **Do not commit your callback scripts to a public repo if they
  contain sensitive logic** (internal hostnames, employee email
  patterns, redaction rules). Treat them like any other piece of
  migration tooling.
- The work directory contains the extracted source repository and
  intermediate archives; clean it up (or store it in an encrypted
  volume) when done.

---

## License

MIT — see [LICENSE](LICENSE).
