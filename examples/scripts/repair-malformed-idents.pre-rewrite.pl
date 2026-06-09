#!/usr/bin/env perl
#
# repair-malformed-idents.pre-rewrite.pl
#
# Usage:
#   gh history-rewrite-migration migrate \
#       --org <src> --repo <repo> --target-org <dst> \
#       --pre-rewrite-script ./examples/scripts/repair-malformed-idents.pre-rewrite.pl
#
# What this does
# --------------
# This is a *pre-rewrite* script: unlike the eight `--filter-repo-script`
# callbacks (which run *inside* git filter-repo's parse loop), a pre-rewrite
# script filters the raw `git fast-export` byte stream *before* filter-repo
# parses it. The orchestrator runs it as:
#
#   LC_ALL=C git fast-export --all --signed-tags=strip \
#       --show-original-ids --reencode=no --use-done-feature \
#     | <this script> \
#     | git filter-repo --stdin --force [...]
#
# It exists because some commits carry malformed author/committer idents that
# crash filter-repo's parser (and git fast-import) *before* any callback can
# run, e.g. an Outlook "mailto:" hyperlink artifact embedded in the ident:
#
#   Pat Doe <pat.doe@example.com <mailto:pat.doe@example.com> 1452345014 +0530
#                                     ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
#   -> fatal: Missing > in ident string   (git fsck: badEmail)
#
# A valid raw ident line is:
#
#   <role> <name> <email-in-angle-brackets> <unix-timestamp> <tz-offset>
#   e.g.  author Pat Doe <pat.doe@example.com> 1452345014 +0530
#
# This script rebuilds a clean `Name <email>` for author/committer/tagger
# lines whose ident contains a recoverable email address.
#
# Why a stream filter (and not a callback)
# ----------------------------------------
# filter-repo parses the fast-export stream into objects, runs callbacks, then
# re-serializes. The malformed ident breaks that *parse* step, so the callback
# never sees it. Fixing the raw stream first is the only place the repair can
# happen.
#
# Correctness guarantees this script upholds
# ------------------------------------------
#   * BYTE-ORIENTED. binmode on both handles; no UTF-8 decoding. fast-export
#     streams contain arbitrary bytes (paths, names, blob/message payloads),
#     so decoding as text would corrupt history.
#   * RESPECTS `data <N>` FRAMING. Blob contents, commit messages and tag
#     messages are emitted as length-prefixed `data <N>\n<N bytes>` blocks.
#     We copy those N bytes through verbatim so a line *inside* a file that
#     happens to start with "author " / "committer " is never mistaken for an
#     ident command. (A naive line filter corrupted blobs in exactly this way.)
#   * ONLY TOUCHES ident command lines (`author `/`committer `/`tagger `).
#   * FAILS CLOSED. If an ident has no recoverable email, the line is passed
#     through UNCHANGED rather than fabricating an address. filter-repo / fsck
#     then surface it loudly instead of silently inventing an identity.
#   * PRESERVES STRUCTURE. Every other line (feature/mark/from/merge/reset/
#     tag/commit/original-oid/done/...) is passed through untouched, so the
#     commit-map's original->final SHA mapping stays intact for the downstream
#     metadata remap.
#
# Adjust to taste: this repairs the known Outlook "<mailto:...>" shape and any
# ident from which a single email is recoverable. It deliberately does NOT
# synthesize emails for idents with none (some sites want a "<user>@<corp>"
# fallback — add it here if that matches your policy, but keep it explicit).

use strict;
use warnings;

binmode STDIN;
binmode STDOUT;

# Matches one valid trailing "raw date": "<seconds> <+|-HHMM>". Git timestamps
# are integers (allow a leading '-' for the rare pre-1970 case).
my $DATE = qr/-?\d+ [+-]\d{4}/;

# A conservative email token. ASCII-only on purpose: we operate on raw bytes,
# and broadening this risks matching arbitrary high bytes in mangled names.
my $EMAIL = qr/[A-Za-z0-9._%+\-]+\@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}/;

while (defined(my $line = <STDIN>)) {
    # Length-prefixed payload: copy `data <N>` header, then exactly N raw bytes.
    if ($line =~ /^data (\d+)\n\z/) {
        print $line;
        my $remaining = $1;
        while ($remaining > 0) {
            my $chunk;
            my $got = read(STDIN, $chunk, $remaining);
            last unless $got;    # truncated stream; let downstream error out
            print $chunk;
            $remaining -= $got;
        }
        next;
    }

    # Ident command lines: author / committer / tagger.
    if ($line =~ /^(author|committer|tagger) (.*) ($DATE)\r?\n\z/) {
        my ($role, $ident, $date) = ($1, $2, $3);
        if (defined(my $fixed = repair_ident($ident))) {
            print "$role $fixed $date\n";
            next;
        }
        # Unrepairable ident: fall through and emit the original line verbatim.
    }

    print $line;
}

# repair_ident: given the ident blob between "<role> " and the trailing date,
# return a clean "Name <email>" string, or undef if no email is recoverable.
sub repair_ident {
    my ($ident) = @_;

    # Unwrap Outlook "mailto:" hyperlinks: <mailto:foo@bar> -> <foo@bar>.
    $ident =~ s/<mailto:([^>]*)>/<$1>/g;

    my @emails = ($ident =~ /($EMAIL)/g);
    return undef unless @emails;    # nothing to anchor on; do not fabricate.

    # If the same address was duplicated by the artifact, the last occurrence
    # is the canonical one.
    my $email = $emails[-1];

    # Derive the display name: strip every <...> group, any bare email tokens,
    # stray angle brackets, then collapse whitespace.
    my $name = $ident;
    $name =~ s/<[^>]*>//g;
    $name =~ s/$EMAIL//g;
    $name =~ s/[<>]//g;
    $name =~ s/^\s+//;
    $name =~ s/\s+$//;
    $name =~ s/\s+/ /g;
    $name = $email if $name eq '';

    return "$name <$email>";
}
