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
# Vet with the file's own build tag (e.g. //go:build integration or acceptance): otherwise a
# tagged file is never vetted, and a package made only of tagged files fails to load.
tags="$(sed -n '1,5s#^//go:build \([A-Za-z0-9_]*\)[[:space:]]*$#\1#p' "$file" | head -1)"
if ! vet="$(cd "$pkg_dir" && go vet ${tags:+-tags "$tags"} . 2>&1)"; then
  echo "go vet reported issues in $pkg_dir:" >&2
  echo "$vet" >&2
  exit 2
fi
exit 0
