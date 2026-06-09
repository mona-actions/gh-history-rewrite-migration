# Pre-rewrite stream scripts (`--pre-rewrite-script`)

Some repositories cannot even be *parsed* by `git filter-repo`. A commit whose
author or committer line is malformed — for example an Outlook "mailto:"
hyperlink left embedded in the ident:

```text
Pat Doe <pat.doe@example.com <mailto:pat.doe@example.com> 1452345014 +0530
```

crashes `git fast-import` (which `filter-repo` drives) with
`fatal: Missing > in ident string` (or `Invalid raw date`, or a `badEmail`
on `git fsck`) **before any callback runs**.

`git filter-repo`'s callback hooks — the eight
[`--filter-repo-script`](callback-scripts.md) kinds (`commit`, `email`, `blob`,
`filename`, `message`, `refname`, `tag`, and `reset`) — cannot fix this. They
execute *inside* filter-repo's parse loop, after the ident line has already been
parsed into an object, so a stream that won't parse never reaches them.

`--pre-rewrite-script` fills that gap: it is a user-supplied filter for the
**pre-parse stage**, applied to the raw `git fast-export` byte stream *before*
filter-repo sees it.

---

## How it runs

When one or more `--pre-rewrite-script <path>` are supplied, the rewrite stage
feeds filter-repo from a stream instead of running it directly against the bare
repo:

```text
LC_ALL=C git fast-export --all --signed-tags=strip \
    --show-original-ids --reencode=no --use-done-feature \
  | <script1> [ | <script2> ... ] \
  | git filter-repo --stdin --force [strip / callback / passthrough args]
```

- **Repeatable.** Scripts are chained in the order given; each reads the
  previous stage's stdout.
- **Composes into one pass.** Strip (`--strip-large-files`), callbacks
  (`--filter-repo-script`) and raw flags (`--filter-repo-flag`) are appended to
  the *same* `filter-repo --stdin` invocation, so the whole rewrite still
  produces a **single** `commit-map` (original → final SHA). The downstream
  metadata remap depends on that single map, which is why `--show-original-ids`
  is always on.

```bash
gh history-rewrite-migration migrate \
    --org acme --repo legacy --target-org acme-cloud \
    --pre-rewrite-script ./examples/scripts/repair-malformed-idents.pre-rewrite.pl
```

It is also available on the advanced `rewrite` subcommand.

---

## Hello World: the smallest possible script

Before the production example, here is the whole idea in one tiny script. A
pre-rewrite script is just a filter — \*\*bytes in on stdin, bytes out on
stdout\*\* — and the simplest useful one rewrites a single author identity across
all of history:

```perl
#!/usr/bin/env perl
use strict; use warnings;
binmode STDIN; binmode STDOUT;

my $OLD = 'Old Name <old@example.com>';
my $NEW = 'New Name <new@example.com>';

while (defined(my $line = <STDIN>)) {
    # Copy `data <N>` payloads (blobs, messages) through untouched.
    if ($line =~ /^data (\d+)\n\z/) {
        print $line;
        my $remaining = $1;
        while ($remaining > 0) {
            my $got = read(STDIN, my $chunk, $remaining) or last;
            print $chunk;
            $remaining -= $got;
        }
        next;
    }
    # Only touch identity lines.
    $line =~ s/\Q$OLD\E/$NEW/ if $line =~ /^(?:author|committer|tagger) /;
    print $line;
}
```

That is the runnable
[`examples/scripts/hello-world.pre-rewrite.pl`](../examples/scripts/hello-world.pre-rewrite.pl).
Edit the two variables, then:

```bash
gh history-rewrite-migration migrate \
    --org acme --repo legacy --target-org acme-cloud \
    --pre-rewrite-script ./examples/scripts/hello-world.pre-rewrite.pl
```

Even this minimal script already follows the two non-negotiable rules
(byte-oriented I/O, and copying `data <N>` payloads verbatim). The next section
explains those rules in full; the production example after it builds on exactly
this shape.

---

## The stream contract

A pre-rewrite script is a filter: **bytes from stdin → bytes to stdout**. It
must emit a valid `git fast-import` stream. Three rules keep it correct:

1. **Operate on bytes, never on decoded text.** A fast-export stream carries
   arbitrary bytes — non-UTF-8 author names, file paths, and blob contents.
   Decoding as UTF-8 (or letting your locale do it) corrupts history. In Perl
   that means `binmode STDIN; binmode STDOUT;` and no `use utf8`; in Python,
   read/write `sys.stdin.buffer` / `sys.stdout.buffer`.

2. **Respect `data <N>` framing.** Blob contents, commit messages and tag
   messages are emitted as length-prefixed blocks:

```text
  data 77
   <exactly 77 raw bytes, which may contain anything>
```

   You must copy those `N` bytes through verbatim. If you treat the stream as
   plain lines, a line *inside a file* that happens to start with `author`  (or
   any token you rewrite) will be mangled — the classic symptom is filter-repo
   later dying with `Could not parse line: 'b'ob''` because a blob got
   corrupted. Read the `N` after `data` , copy that many bytes, then resume
   line processing.

3. **Only touch what you mean to.** Anchor edits to the specific command lines
   you care about — for ident repair that is lines matching
   `^(author|committer|tagger) <ident> <unix-timestamp> <tz-offset>$`. Pass
   **every other line through unchanged**: `feature`, `mark`, `from`, `merge`,
   `reset`, `commit`, `tag`, `original-oid`, `done`, `M`/`D`/`R`/`C`, and so on.

### Preserve `original-oid` — it is a hard requirement

`git fast-export --show-original-ids` emits an `original-oid <sha>` line before
each object. Those lines are what let filter-repo build the original → final
`commit-map` that the metadata remap consumes. \*\*Do not drop, reorder, or
rewrite `original-oid` lines, and do not synthesize new commits.\*\* A script that
mangles them will silently break the remap: metadata that references the
original SHA will fail to map to the rewritten one.

### Fail closed

If your script encounters an ident (or anything else) it cannot confidently
repair, **pass it through unchanged** rather than inventing data. A fabricated
identity is worse than a loud failure: let filter-repo / `git fsck` surface the
bad object so a human can decide. The shipped example does exactly this — it
only rewrites idents from which a real email is recoverable.

### `LC_ALL=C`

The orchestrator sets `LC_ALL=C` for the export and your scripts so byte-level
matching is locale-independent. If you test your script by hand, run it under
`LC_ALL=C` too — without it, tools like `sed`/`perl` may refuse non-UTF-8 bytes
(`RE error: illegal byte sequence`) and silently no-op.

---

## Security model

Pre-rewrite scripts run operator-supplied code, exactly like callback scripts.
The orchestrator wires them defensively:

- **No shell, ever.** The pipeline is built from separate child processes
  connected by in-process pipes — there is no `sh -c` and no command-string
  concatenation, so shell metacharacters in a path or filename cannot inject.
- **Executed via their shebang.** Each script is run as an argv array
  (`exec` of the absolute path), so the kernel honors its `#!/usr/bin/env perl`
  (or `python3`, etc.) line. The orchestrator never guesses an interpreter or
  builds an interpreter command string. Scripts must therefore be \*\*regular,
  executable files with a shebang\*\*; this is validated up front, before any
  process starts.
- **Sanitized environment.** Your script receives a minimal allow-listed
  environment (`PATH`, `HOME`, `LC_ALL`, `LANG`) — the migration PATs
  (`GH_PAT`, `GH_SOURCE_PAT`) and any cloud credentials in the parent
  environment are **not** forwarded.
- **Every stage's exit code is checked.** If any stage fails, the orchestrator
  reports that stage's error and stderr — not a downstream "broken pipe" that
  would mask the real cause.
- **Stream contents are never logged.** Idents are PII; only script paths and
  stage names appear in logs, consistent with callback-body redaction.

As with callback scripts: **only pass scripts you trust**, and don't commit
scripts containing sensitive internal logic to a public repo.

---

## Example: repair malformed author/committer idents

[`examples/scripts/repair-malformed-idents.pre-rewrite.pl`](../examples/scripts/repair-malformed-idents.pre-rewrite.pl)
is a complete, runnable implementation of the contract above. It:

- reads the stream byte-for-byte (`binmode`) and copies `data <N>` payloads
  verbatim;
- rewrites only `author` / `committer` / `tagger` lines;
- unwraps Outlook `<mailto:…>` artifacts, extracts the recoverable email, and
  rebuilds a clean `Name <email>`;
- **fails closed** — idents with no recoverable email pass through untouched.

```bash
gh history-rewrite-migration migrate \
    --org acme --repo legacy --target-org acme-cloud \
    --pre-rewrite-script ./examples/scripts/repair-malformed-idents.pre-rewrite.pl
```

If your malformation differs (a different wrapper, or a site policy that
*does* want a synthesized `<user>@<corp>` fallback), copy the example and adjust
the `repair_ident` routine — keeping the byte-orientation, `data <N>` framing,
and `original-oid` preservation intact.

---

## See also

- [`docs/callback-scripts.md`](callback-scripts.md) — the in-parse-loop
  callbacks (`--filter-repo-script`) for rewrites that *do* parse.
- [`docs/large-files.md`](large-files.md) — `--strip-large-files`, which
  composes into the same single filter-repo pass.
