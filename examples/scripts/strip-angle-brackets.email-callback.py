# strip-angle-brackets.email-callback.py
#
# Usage:
#   gh history-rewrite-migration migrate \
#       --org <src> --repo <repo> --target-org <dst> \
#       --filter-repo-script ./examples/scripts/strip-angle-brackets.email-callback.py
#
# What this does
# --------------
# `git filter-repo`'s --email-callback receives the email portion of
# author/committer/tagger lines as a bytes object (NOT including the
# surrounding `<` `>` delimiters). On rare archives the email field
# itself contains stray angle brackets (e.g. b"<alice@example.com>")
# which then breaks the importer's identity parsing.
#
# This callback is the smallest useful demonstration of the
# email-callback contract: take in bytes, return bytes.
#
# The kind of this script is determined by its filename suffix
# (`.email-callback.py`) — see docs/callback-scripts.md.

return email.replace(b"<", b"")
