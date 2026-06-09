# Custom callback scripts (`--filter-repo-script`, `--filter-repo-flag`)

`git filter-repo` exposes eight Python callback hooks for surgically
rewriting commits, blobs, refs, tags, filenames, and email addresses.
The orchestrator surfaces all eight as user-supplied scripts via
`--filter-repo-script`, plus a `--filter-repo-flag` passthrough for any
other filter-repo flag you need.

Both flags are repeatable and compose with `--strip-large-files` —
when all three are in play, the orchestrator builds a **single**
filter-repo invocation so commit SHAs are rewritten exactly once.

> **Note:** strip and your callbacks run in the *same* pass, so a
> `--blob-callback` / `--filename-callback` still sees blobs that strip
> will remove — it cannot assume an earlier pass already deleted them.

> **Repo won't even parse?** Callbacks run *inside* filter-repo's parse
> loop, so they cannot fix history that crashes the parser (e.g. a
> malformed author ident). For that, use a pre-parse stream filter —
> see [`docs/pre-rewrite-scripts.md`](pre-rewrite-scripts.md).

---

## How a script's "kind" is decided

The callback kind is detected from the **filename suffix**, fail-closed.
There is no `--callback-kind` flag.

| Filename suffix | filter-repo flag | What you receive in your callback |
| --- | --- | --- |
| `*.commit-callback.py` | `--commit-callback` | The `commit` object (parents, file_changes, message, author, …). |
| `*.email-callback.py` | `--email-callback` | An `email` byte-string (e.g. `b"alice@old.example.com"`). |
| `*.blob-callback.py` | `--blob-callback` | The `blob` object (raw blob data). |
| `*.filename-callback.py` | `--filename-callback` | A `filename` byte-string (path in the tree). |
| `*.message-callback.py` | `--message-callback` | A commit/tag `message` byte-string. |
| `*.refname-callback.py` | `--refname-callback` | A fully-qualified `refname` byte-string. |
| `*.tag-callback.py` | `--tag-callback` | The `tag` object (annotated tags only). |
| `*.reset-callback.py` | `--reset-callback` | The `reset` object (branch resets in the stream). |

**Validation rules** (enforced by the orchestrator before filter-repo
ever runs):

- A filename whose suffix is not in the table above → **hard error**.
  No silent default, no "best guess." Rename the file.
- Two scripts of the same kind in one invocation → **hard error**.
  filter-repo accepts only one body per callback flag; if you need
  multiple transforms of the same kind, merge them into one Python
  function.
- Script bodies are passed to filter-repo as files via the
  appropriate `--<kind>-callback <path>` flag. The orchestrator never
  `eval`s your script. Python syntax errors and missing imports
  surface with the script path so you can locate them quickly; script
  *contents* are never logged.

See [`examples/scripts/`](../examples/scripts/) for runnable examples.

---

## Example: strip a null-pointer submodule

A common cause of failed imports is a submodule whose `.gitmodules`
entry was removed but whose tree entry (a "gitlink" / commit pointer)
was left dangling in history. The example
[`examples/scripts/null-submodule.commit-callback.py`](../examples/scripts/null-submodule.commit-callback.py)
removes every gitlink at a fixed path:

```bash
gh history-rewrite-migration migrate \
    --org acme --repo legacy --target-org acme-cloud \
    --filter-repo-script ./examples/scripts/null-submodule.commit-callback.py
```

Adjust the constant inside the script if the submodule path differs.

## Example: strip stray angle brackets from author emails

Some import paths trip on author lines whose email field contains
literal `<`/`>` characters. The example
[`examples/scripts/strip-angle-brackets.email-callback.py`](../examples/scripts/strip-angle-brackets.email-callback.py)
applies a one-line `email.replace(b"<", b"")` and is the smallest
useful demonstration of the email-callback contract:

```bash
gh history-rewrite-migration migrate \
    --org acme --repo legacy --target-org acme-cloud \
    --filter-repo-script ./examples/scripts/strip-angle-brackets.email-callback.py
```

---

## Raw passthrough: `--filter-repo-flag`

Repeatable. Each value is one argv token (with or without `=value`):

```bash
gh history-rewrite-migration rewrite \
    --filter-repo-flag --refs=refs/heads/main \
    --filter-repo-flag --partial \
    --filter-repo-flag --replace-message=replacements.txt
```

The orchestrator parses `git filter-repo --help` at runtime to build a
"did you mean…?" hint via Levenshtein distance, but **unknown flags are
still passed through** — newer `filter-repo` versions with flags the
orchestrator has not seen still work. `filter-repo` itself remains the
authority on what is valid.

### Always-blocked flags

These flags are reserved by the orchestrator and rejected unconditionally
when supplied via `--filter-repo-flag`:

| Flag | Why it is blocked |
| --- | --- |
| `--force` | The orchestrator sets this internally on every rewrite. |
| `--analyze` | Used internally by `--strip-large-files`. |
| `--dry-run` | A rewrite that does not rewrite is a silent no-op surprise. |
| `--debug` | Conflicts with the orchestrator's verbose output. |

### Path-selection family — blocked only with `--strip-large-files`

When `--strip-large-files` is active, the following are also rejected:

```text
--invert-paths   --paths-from-file   --path
--paths          --path-glob         --path-regex
```

The orchestrator generates `--paths-from-file <work-dir>/cleanup.txt
--invert-paths` itself. A user-supplied `--invert-paths` would silently
flip the meaning of `cleanup.txt` (stripping everything **except** the
listed paths), and a user-supplied `--path*` family would re-scope the
strip in ways that are nearly impossible to debug after the fact.

If you need both a large-file strip **and** custom path filtering:

- prefer a callback script (e.g., a `commit-callback` that drops
  unwanted file_changes), or
- run two separate `rewrite` invocations into separate work-dirs and
  merge the results manually.
