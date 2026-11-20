#!/usr/bin/env bash
# PreToolUse hook (Edit|Write|MultiEdit): applied migrations are immutable.
#
# CLAUDE.md says schema changes go into new files in migrations/ and an applied migration is
# never edited (databases that ran it would silently diverge). This hook turns that rule into
# a hard stop: editing a migrations/*.sql file that already exists in HEAD is denied; creating
# a new migration is fine.
set -u
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

hook_input
file="$(normalize_path "$(json_string file_path)")"
case "$file" in
*/migrations/*.sql | migrations/*.sql) ;;
*) exit 0 ;;
esac

name="${file##*/}"
repo="$(project_dir)"
if git -C "$repo" cat-file -e "HEAD:migrations/$name" 2>/dev/null; then
  permission deny "migrations/$name is already committed and may have been applied; never edit it. Add a new numbered migration (up and down) instead."
fi
exit 0
