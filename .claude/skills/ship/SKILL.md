---
name: ship
description: Definition of done for a change in this repository - runs every check (lint, unit, integration with -race, OpenAPI lint, hook tests, smoke and acceptance when the stack is up), audits the diff, commits with Conventional Commits, and hands the push to the developer.
argument-hint: "[short description of the change]"
disable-model-invocation: true
---

# /ship

Change: `$ARGUMENTS`

Working tree:
!`git status --short`

Run the steps in order. If a check fails, find and fix the cause, then rerun from that check.
Never skip a check, weaken a test, or add `--no-verify`. Report what each step did.

## 1. Checks
1. `make lint test-unit`: go vet, golangci-lint, unit tests with `-race`.
2. `make test`: unit and integration tests with `-race` (testcontainers; needs Docker). If
   Docker is down, say so and stop: a phase is not done without `-race` (CLAUDE.md).
3. `make lint-api`, if `api/openapi.yaml` or `internal/httpapi` changed.
4. `make test-hooks`, if `.claude/hooks` or `scripts/git-hooks` changed.
5. If `docker compose ps` shows the stack running: rebuild with `make up`, then run
   `make smoke` and `make acceptance`.

## 2. Audit
Run the `/audit` skill on the diff. Fix confirmed findings that belong to this change. List
the others for the developer instead of fixing them silently.

## 3. Documentation
- Did behaviour, config, endpoints or limits change? Then update `README.md`,
  `api/openapi.yaml` and the relevant ADR in the same commit.
- Is this a non-obvious decision? Record it with `/adr`.

## 4. Commit
- One logical change per commit, Conventional Commits (`feat:`, `fix:`, `test:`, `docs:`,
  `chore:`, `ci:`, `build:`), with a body that explains **why** and how it was verified.
- End with the attribution trailer the session specifies.
- Show the commit list (`git log --oneline origin/main..HEAD`).

## 5. Push and CI
**Do not push.** Pushing is the developer's decision (a hook asks for it as well). Tell them
the exact command (`git push origin <branch>`). Once they confirm, watch the run:
`gh run watch <id> --exit-status`. If CI fails, read `gh run view <id> --log-failed`, fix the
cause in a new commit, and go back to step 1.
