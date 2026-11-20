#!/usr/bin/env bash
# Stop hook: before Claude finishes a turn, catch two kinds of drift in uncommitted work.
#
#   1. The HTTP layer changed but the contract did not: CLAUDE.md requires api/openapi.yaml to
#      change in the same commit as any API change. Claude is sent back once to update the
#      spec or to state why the contract is unaffected (stop_hook_active lets it stop then).
#   2. Go code changed and `go vet ./...` fails.
set -u
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

hook_input
json_true stop_hook_active && exit 0 # already sent back once this turn

repo="$(project_dir)"
cd "$repo" 2>/dev/null || exit 0
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || exit 0

# Uncommitted changes: modified, staged and untracked files.
changed="$(git status --porcelain --untracked-files=all | sed -E 's/^.. //; s/.* -> //')"
[ -n "$changed" ] || exit 0

block() {
  printf '{"decision":"block","reason":"%s"}\n' "$(printf '%s' "$1" | json_escape)"
  exit 0
}

api_changed="$(printf '%s\n' "$changed" | grep -E '^internal/httpapi/[^/]+\.go$' | grep -vE '_test\.go$' | grep -v '^internal/httpapi/middleware\.go$' || true)"
if [ -n "$api_changed" ] && ! printf '%s\n' "$changed" | grep -qx 'api/openapi.yaml'; then
  block "HTTP handlers changed ($(printf '%s' "$api_changed" | tr '\n' ' ')) but api/openapi.yaml did not. Update the contract and run make lint-api, or state explicitly why the contract is unaffected."
fi

if [ -f go.mod ] && printf '%s\n' "$changed" | grep -qE '\.go$'; then
  if ! out="$(go vet ./... 2>&1)"; then
    block "go vet fails on the uncommitted changes: $(printf '%s' "$out" | head -n 15)"
  fi
fi
exit 0
