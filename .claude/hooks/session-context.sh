#!/usr/bin/env bash
# SessionStart hook: give Claude the state it would otherwise discover by trial and error —
# branch, unpushed and uncommitted work, and whether the Docker daemon (needed by make test,
# make up and the acceptance suite) is reachable.
set -u
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

repo="$(project_dir)"
cd "$repo" 2>/dev/null || exit 0
notes=()

if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null)"
  line="Branch $branch"
  if counts="$(git rev-list --left-right --count '@{upstream}...HEAD' 2>/dev/null)"; then
    behind="${counts%%[[:space:]]*}"
    ahead="${counts##*[[:space:]]}"
    line="$line: $ahead commit(s) not pushed, $behind behind upstream"
  fi
  dirty="$(git status --porcelain | grep -c . || true)"
  notes+=("$line; $dirty uncommitted file(s).")
fi

if command -v docker >/dev/null 2>&1; then
  if command -v timeout >/dev/null 2>&1; then probe=(timeout 5 docker info); else probe=(docker info); fi
  if "${probe[@]}" >/dev/null 2>&1; then
    notes+=("Docker daemon reachable.")
  else
    notes+=("Docker daemon NOT reachable: make test, make up and make acceptance need it (start Docker first).")
  fi
fi

context="$(printf '%s ' "${notes[@]}" | json_escape)"
printf '{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"%s"}}\n' "$context"
