#!/usr/bin/env bash
# Shared helpers for the Claude Code hooks in this directory.
#
# Portable on purpose: bash, sed, grep and git only (no jq, python or bc), so the hooks behave
# the same in Windows Git Bash and on Linux CI. Hooks receive the tool call as one JSON object
# on stdin; these helpers read string fields from it without a JSON parser, which is enough for
# the flat fields hooks need (tool_input.command, tool_input.file_path, stop_hook_active).

# hook_input reads stdin once into HOOK_INPUT, with newlines folded so fields can be matched on
# one line (newlines inside JSON strings are already escaped as \n).
hook_input() {
  HOOK_INPUT="$(tr '\n' ' ')"
}

# json_string FIELD prints the unescaped value of the last string field named FIELD.
json_string() {
  printf '%s' "$HOOK_INPUT" |
    sed -nE 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"(([^"\\]|\\.)*)".*/\1/p' |
    sed -e 's/\\"/"/g' -e 's/\\\\/\\/g'
}

# json_true FIELD succeeds if FIELD is the JSON literal true.
json_true() {
  printf '%s' "$HOOK_INPUT" | grep -Eq '"'"$1"'"[[:space:]]*:[[:space:]]*true'
}

# json_escape makes stdin safe inside a JSON string.
json_escape() {
  sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr '\n' ' '
}

# normalize_path turns Windows paths (C:\a\b, C:/a/b) into Git Bash form (/c/a/b) so paths
# from either platform compare equal.
normalize_path() {
  printf '%s' "$1" | sed -E -e 's#\\#/#g' -e 's#^([A-Za-z]):/#/\L\1/#' -e 's#/+#/#g'
}

# project_dir is the repository root, normalized.
project_dir() {
  normalize_path "${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
}

# permission DECISION REASON answers a PreToolUse hook: allow, ask or deny with a reason that
# Claude (and the developer, for ask) sees.
permission() {
  local reason
  reason="$(printf '%s' "$2" | json_escape)"
  printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"%s","permissionDecisionReason":"%s"}}\n' "$1" "$reason"
  exit 0
}
