#!/usr/bin/env perl
#
# repair-malformed-idents.pre-rewrite.pl
#
# Repairs malformed author/committer/tagger idents that crash filter-repo's
# parser (e.g. an Outlook "<mailto:...>" artifact). Idents with a recoverable
# email are rebuilt as a clean `Name <email>`; idents without one pass through
# unchanged. See docs/pre-rewrite-scripts.md for usage and the full contract.

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
