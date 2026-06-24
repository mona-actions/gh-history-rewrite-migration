#!/usr/bin/env python3
#
# hello-world.pre-rewrite.py — the simplest possible --pre-rewrite-script.
#
# Reads the `git fast-export` stream on stdin, rewrites a single author/committer
# identity across all history, and writes the result to stdout. Edit the two
# variables below for your own case. Any language works — this one is Python;
# see repair-malformed-idents.pre-rewrite.pl for a real Perl example.

import re
import sys

# --- edit these two lines ----------------------------------------------------
OLD = b"Old Name <old@example.com>"
NEW = b"New Name <new@example.com>"
# -----------------------------------------------------------------------------

stdin = sys.stdin.buffer
stdout = sys.stdout.buffer

data_header = re.compile(rb"^data (\d+)\n\Z")
ident_line = re.compile(rb"^(?:author|committer|tagger) ")

while True:
    line = stdin.readline()
    if not line:
        break

    # Copy `data <N>` payloads (blobs, messages) through verbatim, byte for byte.
    m = data_header.match(line)
    if m:
        stdout.write(line)
        remaining = int(m.group(1))
        while remaining > 0:
            chunk = stdin.read(remaining)
            if not chunk:
                break  # truncated stream; let downstream error out
            stdout.write(chunk)
            remaining -= len(chunk)
        continue

    # Only rewrite the identity on author / committer / tagger lines.
    if ident_line.match(line):
        line = line.replace(OLD, NEW)

    stdout.write(line)
