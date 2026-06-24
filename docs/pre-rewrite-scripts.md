# Pre-rewrite stream scripts (`--pre-rewrite-script`)

## Why it exists

Some repositories can't even be *parsed* by `git filter-repo`. A commit whose author or committer line is malformed — e.g. an Outlook `mailto:` link left embedded in the ident:

```text
Pat Doe <pat.doe@example.com <mailto:pat.doe@example.com> 1452345014 +0530
```

crashes `git fast-import` (which filter-repo drives) with `fatal: missing > in ident string` *before any callback runs*. filter-repo's normal callbacks (`--filter-repo-script`) can't help — they execute inside the parse loop, after the bad line has already been parsed, so a stream that won't parse never reaches them.

`--pre-rewrite-script` fills that gap: a filter applied to the raw `git fast-export` byte stream *before* filter-repo sees it.

## When to use it

Reach for it only when filter-repo fails to **parse** the history — malformed idents, bad dates, or other corruption that `git fsck` flags as an error and that crashes `fast-import`. For rewrites on history that already parses (rename an author, strip a path), use the in-loop [`--filter-repo-script`](callback-scripts.md) callbacks instead.

## How to run it

Pass `--pre-rewrite-script <path>` (repeatable) to `migrate` or `rewrite`:

```bash
gh history-rewrite-migration migrate \
    --org acme --repo legacy --target-org acme-cloud \
    --pre-rewrite-script ./examples/scripts/repair-malformed-idents.pre-rewrite.pl
```

Scripts are chained in order and feed a single `filter-repo --stdin` pass, so strip/callback/flag options still compose into one `commit-map`.

## Writing a script

Don't start from scratch. Copy the runnable [`examples/scripts/repair-malformed-idents.pre-rewrite.pl`](../examples/scripts/repair-malformed-idents.pre-rewrite.pl) and edit its `repair_ident` routine for your case. It already handles the tricky parts of the stream format correctly; it unwraps `<mailto:…>` artifacts, rebuilds a clean `Name <email>`, and leaves any ident it can't confidently fix untouched.

Scripts can be in any language — they're run directly via their shebang. The Perl example above is the real-world repair; [`examples/scripts/hello-world.pre-rewrite.py`](../examples/scripts/hello-world.pre-rewrite.py) is a minimal Python template showing just the stream skeleton.

Your script reads the export on stdin and writes the fixed version to stdout. A few things will silently corrupt history if you get them wrong, which is why starting from the example matters:

- It edits the raw bytes, not decoded text, so non-English names and binary file contents survive intact.
- It only changes `author`/`committer`/`tagger` lines and copies everything else through unchanged — including file contents and the bookkeeping lines Git uses to track which old commit became which new one.
- When it can't recover a real email from a broken ident, it leaves the line alone rather than inventing a fake one.

Scripts run with no shell, via their shebang, in a sanitized environment (`PATH`, `HOME`, `LANG`, `TMPDIR`, plus a forced `LC_ALL=C`); the migration PATs are never forwarded. **Only pass scripts you trust.**

## See also

- [`docs/callback-scripts.md`](callback-scripts.md) — in-loop callbacks for history that parses.
- [`docs/large-files.md`](large-files.md) — `--strip-large-files`, same single pass.
