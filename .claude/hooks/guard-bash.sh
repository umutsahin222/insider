#!/usr/bin/env bash
# PreToolUse hook (Bash): deterministic guardrails for shell commands Claude runs here.
#
#   deny  force pushes, pushes of eval/* branches, git reset --hard, git clean -f
#   deny  docker prunes, and docker rm/stop/kill of containers outside this compose project
#   deny  rm -r with root, home, parent-directory or out-of-repo targets
#   ask   any other git push: publishing needs the developer's yes
#
# Everything else gets no decision (the normal permission rules apply). Rules are generic on
# purpose; machine-specific exceptions belong in the untracked .claude/settings.local.json.
set -u
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

hook_input
cmd="$(json_string command)"
[ -n "$cmd" ] || exit 0

COMPOSE_PROJECT="insider-notify" # docker-compose.yml: name
repo="$(project_dir)"

# Each simple command is checked on its own: split on && || ; | and newlines.
segments="$(printf '%s\n' "$cmd" | sed -E 's/(&&|\|\||;|\|)/\n/g')"

ask_push=""
while IFS= read -r seg; do
  # Trim, and drop a leading "sudo " or "command ".
  seg="$(printf '%s' "$seg" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//; s/^(sudo|command)[[:space:]]+//')"
  [ -n "$seg" ] || continue
  read -r -a w <<<"$seg"

  case "${w[0]}" in
  git)
    # Skip global options such as -C <dir> or -c k=v to find the subcommand.
    i=1
    while [ $i -lt ${#w[@]} ]; do
      case "${w[$i]}" in
      -C | -c) i=$((i + 2)) ;;
      -*) i=$((i + 1)) ;;
      *) break ;;
      esac
    done
    sub="${w[$i]:-}"
    rest=" ${w[*]:$((i + 1))} "
    case "$sub" in
    push)
      if printf '%s' "$rest" | grep -Eq '[[:space:]](-f|--force|--force-with-lease(=[^[:space:]]*)?|--mirror|--delete|-d)[[:space:]]|[[:space:]]\+[^[:space:]]+'; then
        permission deny "Force, mirror and delete pushes rewrite shared history and are not allowed from Claude Code in this repository."
      fi
      if printf '%s' "$rest" | grep -Eq '(^|[[:space:]:/])eval/'; then
        permission deny "eval/* branches hold deliberately broken code for evaluations and must never leave this machine."
      fi
      ask_push="yes"
      ;;
    reset)
      printf '%s' "$rest" | grep -Eq '[[:space:]]--hard[[:space:]]' &&
        permission deny "git reset --hard discards uncommitted work; use git stash or a new commit instead."
      ;;
    clean)
      printf '%s' "$rest" | grep -Eq '[[:space:]](-[a-zA-Z]*f[a-zA-Z]*|--force)[[:space:]]' &&
        permission deny "git clean -f deletes untracked files irreversibly; remove specific files instead."
      ;;
    esac
    ;;
  docker)
    sub="${w[1]:-}"
    [ "$sub" = container ] && sub="${w[2]:-}" && w=("${w[@]:1}")
    case "$sub" in
    prune)
      permission deny "docker container prune removes containers that may not belong to this project."
      ;;
    system | volume | image | network | builder)
      [ "${w[2]:-}" = prune ] &&
        permission deny "docker $sub prune affects every project on this machine; use 'docker compose down -v' for this stack."
      ;;
    rm | stop | kill)
      # Targets are the non-flag arguments (flags taking a value: -t/--time, -s/--signal).
      j=2
      while [ $j -lt ${#w[@]} ]; do
        t="${w[$j]}"
        case "$t" in
        -t | --time | -s | --signal) j=$((j + 2)); continue ;;
        -*) j=$((j + 1)); continue ;;
        esac
        case "$t" in
        "$COMPOSE_PROJECT"-*) ;;
        *) permission deny "docker $sub may only target this compose project's containers ($COMPOSE_PROJECT-*); use 'docker compose $sub' instead of naming '$t'." ;;
        esac
        j=$((j + 1))
      done
      ;;
    esac
    ;;
  rm)
    recursive=""
    targets=()
    for t in "${w[@]:1}"; do
      case "$t" in
      --recursive) recursive=yes ;;
      --*) ;;
      -*) printf '%s' "$t" | grep -q '[rR]' && recursive=yes ;;
      *) targets+=("$t") ;;
      esac
    done
    [ -n "$recursive" ] || continue
    for t in "${targets[@]}"; do
      p="$(normalize_path "$t")"
      p="${p%/}"
      case "$p" in
      "" | / | /\* | "~" | "~/"* | '$HOME'* | '${HOME}'* | . | .. | ../* | \*)
        permission deny "rm -r on '$t' would delete outside the repository or the repository itself."
        ;;
      /*)
        case "$p" in
        "$repo"/* | /tmp/* | */[Tt]emp/* | */tmp/*) ;;
        *) permission deny "rm -r on '$t' targets a path outside the repository and temp directories." ;;
        esac
        ;;
      esac
    done
    ;;
  esac
done <<<"$segments"

if [ -n "$ask_push" ]; then
  permission ask "Pushing publishes commits to the shared repository; the developer approves every push."
fi
exit 0
