#!/usr/bin/env bash
# Tests for the Claude Code hooks in .claude/hooks and the git pre-push hook: each hook gets the
# JSON payload Claude Code would send and must answer with the expected decision.
# Portable: bash, git, sed, grep (and go, if installed, for the vet case). Runs in Windows Git
# Bash and on Linux CI:  bash scripts/test-hooks.sh
set -u
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOKS="$REPO/.claude/hooks"
pass=0
fail=0

esc() { sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

# check NAME EXPECT OUTPUT — EXPECT is deny, ask, block, context or none (no decision).
check() {
  local name="$1" expect="$2" out="$3" ok=""
  case "$expect" in
  deny | ask) printf '%s' "$out" | grep -q "\"permissionDecision\":\"$expect\"" && ok=1 ;;
  block) printf '%s' "$out" | grep -q '"decision":"block"' && ok=1 ;;
  context) printf '%s' "$out" | grep -q '"hookEventName":"SessionStart","additionalContext":"' && ok=1 ;;
  none) [ -z "$out" ] && ok=1 ;;
  esac
  if [ -n "$ok" ]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    printf 'FAIL %-62s want %-7s got: %s\n' "$name" "$expect" "${out:-<no output>}"
  fi
}

bash_cmd() { # EXPECT COMMAND
  local payload
  payload="{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"$(printf '%s' "$2" | esc)\",\"description\":\"x\"}}"
  check "guard-bash: $2" "$1" "$(printf '%s' "$payload" | CLAUDE_PROJECT_DIR="$REPO" bash "$HOOKS/guard-bash.sh")"
}

edit_file() { # EXPECT PATH
  local payload
  payload="{\"tool_name\":\"Edit\",\"tool_input\":{\"file_path\":\"$(printf '%s' "$2" | esc)\",\"old_string\":\"a\",\"new_string\":\"b\"}}"
  check "protect-migrations: $2" "$1" "$(printf '%s' "$payload" | CLAUDE_PROJECT_DIR="$REPO" bash "$HOOKS/protect-migrations.sh")"
}

# --- guard-bash -------------------------------------------------------------------------
bash_cmd deny 'git push --force origin main'
bash_cmd deny 'git push -f'
bash_cmd deny 'git push origin +main'
bash_cmd deny 'git push --force-with-lease origin main'
bash_cmd deny 'git -C /some/repo push --delete origin feature'
bash_cmd deny 'git push origin eval/planted-bugs'
bash_cmd deny 'git push -u origin eval/x:eval/x'
bash_cmd deny 'git reset --hard HEAD~1'
bash_cmd deny 'echo done; git reset --hard'
bash_cmd deny 'git clean -fdx'
bash_cmd deny 'docker system prune -af'
bash_cmd deny 'docker volume prune -f'
bash_cmd deny 'docker container prune'
bash_cmd deny 'docker rm -f some_other_database'
bash_cmd deny 'docker stop -t 5 legacy_web'
bash_cmd deny 'docker container kill insider-notify-api-1 other_app'
bash_cmd deny 'rm -rf /'
bash_cmd deny 'rm -rf ~'
bash_cmd deny 'rm -rf ..'
bash_cmd deny 'cd bin && rm -rf ../../elsewhere'
bash_cmd deny 'rm -r -f /etc/nginx'
bash_cmd deny 'rm -rf C:\Windows\Temp2'
bash_cmd ask 'git push origin main'
bash_cmd ask 'git push'
bash_cmd ask 'make lint test-unit && git push -u origin feature/x'
bash_cmd none 'go test ./...'
bash_cmd none 'git status --short'
bash_cmd none 'git reset --soft HEAD~1'
bash_cmd none 'git log --oneline | head -5'
bash_cmd none 'docker compose down -v'
bash_cmd none 'docker ps --format "{{.Names}}"'
bash_cmd none 'docker stop insider-notify-api-1'
bash_cmd none 'docker rm -f insider-notify-worker-2 insider-notify-worker-3'
bash_cmd none 'rm -rf bin'
bash_cmd none 'rm -rf ./coverage /tmp/scratch-dir'
bash_cmd none "rm -rf $REPO/bin"
bash_cmd none 'rm notes.txt'

# --- protect-migrations -----------------------------------------------------------------
edit_file deny "$REPO/migrations/000001_create_notifications.up.sql"
edit_file deny 'migrations/000002_create_delivery_attempts.down.sql'
edit_file deny 'C:\work\insider\migrations\000005_add_stats_indexes.up.sql'
edit_file none "$REPO/migrations/000099_add_something_new.up.sql"
edit_file none "$REPO/migrations/embed.go"
edit_file none "$REPO/README.md"

# --- contract-drift (in a throwaway repository) -----------------------------------------
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
(
  cd "$tmp" && git init -q && git config core.autocrlf false && mkdir -p internal/httpapi api &&
    echo 'package httpapi' >internal/httpapi/router.go &&
    echo 'openapi: 3.1.0' >api/openapi.yaml &&
    git add . && git -c user.email=t@example.com -c user.name=t commit -qm init
)
stop() { printf '{"hook_event_name":"Stop","stop_hook_active":%s}' "$1" | CLAUDE_PROJECT_DIR="$tmp" bash "$HOOKS/contract-drift.sh"; }
check "contract-drift: clean tree" none "$(stop false)"
echo '// new route' >>"$tmp/internal/httpapi/router.go"
check "contract-drift: handler changed, spec not" block "$(stop false)"
check "contract-drift: already sent back once (stop_hook_active)" none "$(stop true)"
echo '# documented' >>"$tmp/api/openapi.yaml"
check "contract-drift: handler and spec changed" none "$(stop false)"
git -C "$tmp" checkout -q -- .
echo 'package httpapi' >"$tmp/internal/httpapi/router_test.go"
echo 'package httpapi' >"$tmp/internal/httpapi/middleware.go"
check "contract-drift: only tests and middleware changed" none "$(stop false)"
if command -v go >/dev/null 2>&1; then
  rm -f "$tmp/internal/httpapi/"*_test.go "$tmp/internal/httpapi/middleware.go"
  printf 'module example.com/x\n\ngo 1.21\n' >"$tmp/go.mod"
  printf 'package x\n\nimport "fmt"\n\nfunc F() { fmt.Printf("%%d", "not a number") }\n' >"$tmp/x.go"
  check "contract-drift: go vet failure" block "$(stop false)"
fi

# --- session-context --------------------------------------------------------------------
check "session-context: reports repository state" context "$(printf '{}' | CLAUDE_PROJECT_DIR="$REPO" bash "$HOOKS/session-context.sh")"

# --- gofmt (non-Go files pass through silently) ------------------------------------------
check "gofmt: non-Go file ignored" none "$(printf '{"tool_input":{"file_path":"README.md"}}' | bash "$HOOKS/gofmt.sh")"

# --- git pre-push ---------------------------------------------------------------------------
prepush() { printf '%s\n' "$1" | bash "$REPO/scripts/git-hooks/pre-push" 2>/dev/null && echo allowed || echo refused; }
[ "$(prepush 'refs/heads/eval/planted-bugs 1111 refs/heads/eval/planted-bugs 0000')" = refused ] && pass=$((pass + 1)) || { fail=$((fail + 1)); echo "FAIL pre-push: eval branch must be refused"; }
[ "$(prepush 'refs/heads/main 1111 refs/heads/main 2222')" = allowed ] && pass=$((pass + 1)) || { fail=$((fail + 1)); echo "FAIL pre-push: main must be allowed"; }

echo "hooks: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
