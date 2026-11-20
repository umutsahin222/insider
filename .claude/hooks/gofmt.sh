#!/usr/bin/env bash
# PostToolUse hook: gofmt + go vet the Go file Claude just edited/wrote.
# Receives the tool call as JSON on stdin.
set -u
export PATH="$PATH:/c/Program Files/Go/bin:/usr/local/go/bin"

input="$(cat)"
file="$(printf '%s' "$input" | grep -o '"file_path"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*:[[:space:]]*"\(.*\)"/\1/' | sed 's/\\\\/\//g')"

case "$file" in
  *.go) ;;
  *) exit 0 ;;
esac

if ! out="$(gofmt -l -w "$file" 2>&1)"; then
  echo "gofmt failed for $file: $out" >&2
  exit 2   # surface the error back to Claude
fi

pkg_dir="$(dirname "$file")"
if ! vet="$(cd "$pkg_dir" && go vet . 2>&1)"; then
  echo "go vet reported issues in $pkg_dir:" >&2
  echo "$vet" >&2
  exit 2
fi
exit 0
