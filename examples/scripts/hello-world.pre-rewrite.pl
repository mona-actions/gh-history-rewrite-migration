#!/usr/bin/env perl
#
# hello-world.pre-rewrite.pl — the simplest possible --pre-rewrite-script.
#
# A pre-rewrite script is just a filter: it reads the raw `git fast-export`
# byte stream on stdin and writes a (possibly edited) stream to stdout, *before*
# `git filter-repo` parses it. The orchestrator runs it as:
#
#   git fast-export ... | <this script> | git filter-repo --stdin ...
#
# This example does ONE easy-to-follow thing: rewrite a single author/committer
# identity across all history — change "Old Name <old@example.com>" into
# "New Name <new@example.com>". Edit the two variables below for your own case.
#
# It still respects the two rules every pre-rewrite script must follow:
#   1. read/write raw bytes (binmode), never decoded text; and
#   2. copy `data <N>` payloads (blobs, commit messages) through untouched, so a
#      line *inside a file* that starts with "author " is never mistaken for an
#      identity line.
#
# For the real-world malformed-ident repair — and the full set of correctness
# guarantees — see repair-malformed-idents.pre-rewrite.pl in this folder.

use strict;
use warnings;

binmode STDIN;
binmode STDOUT;

# --- edit these two lines ----------------------------------------------------
my $OLD = 'Old Name <old@example.com>';
my $NEW = 'New Name <new@example.com>';
# -----------------------------------------------------------------------------

while (defined(my $line = <STDIN>)) {
    # `data <N>` introduces N raw bytes (a blob or a commit/tag message). Copy
    # them through verbatim so we never accidentally edit file contents.
    if ($line =~ /^data (\d+)\n\z/) {
        print $line;
        my $remaining = $1;
        while ($remaining > 0) {
            my $got = read(STDIN, my $chunk, $remaining);
            last unless $got;    # truncated stream; let downstream error out
            print $chunk;
            $remaining -= $got;
        }
        next;
    }

    # Only rewrite the identity on author / committer / tagger lines.
    if ($line =~ /^(?:author|committer|tagger) /) {
        $line =~ s/\Q$OLD\E/$NEW/;
    }

    print $line;
}
