# null-submodule.commit-callback.py
#
# Usage:
#   gh history-rewrite-migration migrate \
#       --org <src> --repo <repo> --target-org <dst> \
#       --filter-repo-script ./examples/scripts/null-submodule.commit-callback.py
#
# What this does
# --------------
# Removes every "gitlink" tree entry (mode 160000 — i.e., a submodule
# pointer to another repo's commit) at the path SUBMODULE_PATH from
# every commit in history. This is the standard fix for a "null-pointer
# submodule": the .gitmodules entry was removed long ago, but the tree
# still references a phantom submodule commit, and the importer rejects
# it.
#
# The kind of this script is determined by its filename suffix
# (`.commit-callback.py`) — see docs/callback-scripts.md.
#
# Adjust SUBMODULE_PATH below for your repository.

SUBMODULE_PATH = b"CommonComponentsSpecification"

# `commit` is a filter-repo Commit object. `commit.file_changes` is a
# list of FileChange objects with attributes:
#   .type      — b"M" (modify), b"D" (delete), b"R" (rename), ...
#   .filename  — bytes; the path in the tree
#   .mode      — bytes; e.g. b"100644" (regular file), b"160000" (gitlink)
#   .blob_id   — bytes; the blob SHA (or null for gitlinks/deletes)
#
# We drop any entry whose path matches SUBMODULE_PATH AND whose mode is
# the gitlink mode. Filtering by mode (not just by path) is defensive:
# if a real file ever lived at the same path, we leave it alone.

new_changes = []
for change in commit.file_changes:
    is_submodule_path = change.filename == SUBMODULE_PATH
    is_gitlink = change.mode == b"160000"
    if is_submodule_path and is_gitlink:
        # Drop this change entirely — equivalent to "this submodule
        # entry was never here".
        continue
    new_changes.append(change)

commit.file_changes = new_changes
