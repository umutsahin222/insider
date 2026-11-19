# How this was built with Claude Code

The assessment says the review focuses on architecture and on how Claude Code was steered. This
document covers the steering. Everything it describes can be checked in the repository:
[`CLAUDE.md`](../CLAUDE.md), [`.claude/`](../.claude), the [ADRs](adr/) and the commit history.

## In one paragraph

The developer made the product decisions and approved every phase. Claude Code (Opus) wrote the
code, the tests and the docs, inside rules written down in `CLAUDE.md` and enforced by a hook. The
work ran in eight phases from an approved plan, with Conventional Commits inside each phase and a
push only after the developer said yes. Each phase finished only after the work ran for real:
unit tests, integration tests against real Postgres and Redis, and then the whole system under
load in Docker. That last step found most of the bugs worth talking about, and each one led to a
fix commit or an ADR. At the end, `/code-review` and `/security-review` went over the whole
codebase. Claude verified the ten code-review findings one by one and fixed nine; the tenth is a
documented exception.

## 1. Setting up the session

**Plan first, in plan mode.** Before any code, Claude read the brief and proposed an architecture
and a phased plan. The developer settled the open questions:

- Postgres as both the queue and the store, with Redis for the rate limiter and events, instead
  of RabbitMQ or Kafka.
- Go installed locally, not only in Docker.
- All six bonus features.

The approved plan (phases 0–7: bootstrap, contract and model, API, worker, observability,
bonuses, packaging and CI, docs and reviews) became the backbone of the commit history.

**`CLAUDE.md` holds the standing rules**, so they do not have to be repeated in every prompt:

- Dependencies point inward: `domain` imports nothing from the project.
- Business rules stay out of handlers and SQL.
- Every status change is a conditional `UPDATE`, never read-then-write.
- Consumers define the interfaces.
- No global state, including Prometheus registries and the OpenTelemetry tracer provider; metric
  labels come only from closed sets.
- Only the approved dependencies, and ask before adding others.
- `make lint test-unit` must pass before every commit, and `-race` before a phase is declared
  done.
- The OpenAPI file changes in the same commit as the API.

The file grew as decisions were made. Phase 4, for example, added the rules about metrics and
tracing globals.

**ADRs carry the reasoning**, so it survives beyond a chat transcript. There are ten, kept short
(context, decision, consequences). When a later finding changed a decision, the ADR was revised
in place, saying what changed and why: ADR-002 (circuit breaker), ADR-005 (send deadline),
ADR-008 (asynchronous events), ADR-009 (template subset).

**Memory across sessions.** The work spanned several sessions and context compactions. Claude kept
a project memory file with what the code cannot tell a fresh session:

- what has been pushed;
- the agreed workflow;
- toolchain quirks (`make` only works from Git Bash);
- which other Docker containers on the machine must never be touched.

## 2. Guardrails

- **Hook:** after every `Edit` or `Write` of a `.go` file, a `PostToolUse` hook
  ([`.claude/hooks/gofmt.sh`](../.claude/hooks/gofmt.sh)) runs `gofmt -w` and `go vet` on that
  package and reports problems back to Claude straight away. It catches a broken intermediate
  state at the edit that caused it, not three files later.
- **Narrow permissions:** [`.claude/settings.json`](../.claude/settings.json) allows only what
  the work needs without a prompt: go build/test/vet, gofmt, make, docker compose, read-only git,
  golangci-lint, and one exact, pinned Redocly command. Auto mode stayed on. When its classifier
  blocked an ad-hoc download (an unpinned `npx` package, or a Docker image pulled just to try
  something), Claude did not retry or work around it. It added a pinned Makefile target, such as
  `make lint-api` with Redocly 1.25.11, plus a matching allow rule, and in the final README check
  it simply skipped the step.
- **Human gates:** every push needed the developer's yes. Claude also had standing instructions
  not to touch the developer's other containers, to keep personal data out of anything sent
  outside, and to do without admin rights.

## 3. The loop inside every phase

1. Implement one logical change, with tests in the same commit, table-driven where it fits.
2. `make lint test-unit`, then commit (Conventional Commits; the message explains why, not just
   what).
3. Integration tests (`make test`): testcontainers start real Postgres and Redis, under `-race`.
4. **Run the actual system and try to break it.** Start the stack, push load through it, kill
   things, and compare numbers from independent sources: provider stats against
   `notification_deliveries_total`, `/v1/stats` against Prometheus, Jaeger against logs.
5. Write down any non-obvious decision as an ADR.
6. Report to the developer in Turkish, push after approval, then watch CI (`gh run watch`) and
   fix what it finds.

## 4. Bugs found by verification

These are not things Claude got right the first time. They are the places where running the
system proved the first version wrong.

| What was wrong | How it surfaced | Fix |
|---|---|---|
| Retries scheduled for "now" were not claimable: Docker Desktop's VM clock lagged the host | Integration test of retry, then claim | The database clock is the only queue clock; stores take durations ([ADR-005](adr/005-lease-fencing-and-db-clock.md)) |
| A reused `Idempotency-Key` with a different body silently returned the original (the new message was lost); a retried batch duplicated its items | The developer asked "is anything missing, wrong, or not as intended so far?" and Claude audited its own work against the brief | Request fingerprints, a `batches` table with its own key, `422` on reuse ([ADR-006](adr/006-idempotency-fingerprints-and-batch-keys.md)) |
| With the breaker open mid-batch, workers still took rate-limit tokens that other replicas could have used | A unit test written for the new metrics | Release first, then take a token (in the metrics commit) |
| The circuit breaker paused healthy channels: "5 consecutive failures" happens by chance at a 30% failure rate | E2E load through the new `/v1/stats`: SMS p50 end-to-end latency was 65 s for a job that should take 6 s | Trip on failure ratio over a window; p50 fell to 4.9 s ([ADR-002](adr/002-delivery-and-retry.md)) |
| WebSocket streams would die at the server's 30 s `WriteTimeout` | Reasoning about the upgrade path, then a stream kept open past 30 s | Clear the read and write deadlines for upgraded connections |
| Prometheus saw one of three scaled workers | Smoke test against `--scale worker=3` | DNS service-discovery refresh from 30 s to 5 s |
| `smoke.sh` misread HTTP status codes | First run of the smoke test | Split body and status correctly |
| `go mod tidy` removed chi and gobreaker | Build failure | Only run tidy once the imports exist |
| A template example in the README failed (`missingkey=error` also applies inside `if`) | Running every README example against the stack | Corrected the example |

Not every check found a bug. Killing a worker with `-9` mid-batch, for example, showed the reaper
reclaiming all 36 orphaned rows and delivering them on attempt 2, as designed.

## 5. Review passes (phase 7)

### `/code-review` at high effort over `internal/` and `cmd/`

It ran in the background and returned ten findings. It had not checked them against the code, so
Claude checked each one before changing anything. Where possible, Claude wrote a test that fails
without the fix.

| # | Finding | Verdict | What changed |
|---|---|---|---|
| 1 | Rows of a claimed batch wait for rate-limit tokens one after another. A late row can wait past its lease, be reaped, be claimed elsewhere, and be sent twice. | Real | Per-row send deadline; the wait is bounded by it; a late row is released as `lease_expiring`. **E2E:** 40 SMS at 2/s with a 20 s lease produced 40 `lease_expiring` releases, 0 reaper reclaims and 0 double sends. |
| 2 | The WebSocket snapshot for a single notification was read *before* subscribing, so a transition in that gap was lost. | Real | Re-read after subscribing. The regression test fails without the fix. |
| 3 | User templates could loop forever without output (`{{range 100000}}{{range 100000}}{{end}}{{end}}`). | Real, and broader: also exponential recursion and `printf` width bombs | Allowlist of template syntax checked at creation; rendering is now linear ([ADR-009](adr/009-templates.md)). |
| 4 | Zero durations were accepted; `REAPER_INTERVAL=0` panics the worker. | Real | Every duration must be positive. |
| 5 | Outcome writes ignore shutdown cancellation and had no deadline, so a stalled database could hang the process on exit. | Real | 10 s store timeout. |
| 6 | In the half-open breaker state every loop took a shared token, then the breaker refused it. | Real | Only the loop making the single probe takes a token. |
| 7 | Status events were published synchronously, two Redis round trips per send. | Real | Asynchronous publisher with a bounded queue, order kept ([ADR-008](adr/008-status-events-and-websocket.md)). |
| 8 | Templates were re-parsed for every item of a batch. | Real (minor) | Parsed once per request. |
| 9 | The reaper's "attempts exhausted → failed" rule lives in SQL, against a `CLAUDE.md` rule. | Valid point, kept | It is a deliberate exception: reaping must be one set-based conditional `UPDATE`. Documented in ADR-005 and in the code. |
| 10 | A failed release lost `correlation_id` on shutdown and logged `notification_id` twice elsewhere. | Real | Releases log through the notification's context. |

The result was six `fix:` commits, each naming the review as its source.

### `/security-review` over the whole codebase

The command compares against `origin/HEAD`, which was not set, and on a fully pushed branch there
was nothing to compare anyway. To review the whole codebase, Claude temporarily pointed the local
`origin/HEAD` at the bootstrap commit, then restored it with `git remote set-head origin -a`. The
review found no high or medium issues. What it checked:

- SQL is fully parameterised, including the dynamic list filters.
- The template engine has no callable data.
- The WebSocket only accepts its own origin by default.
- Correlation IDs are validated before being forwarded as headers.
- Responses are JSON only.
- Logs carry no recipients or content.
- The container is distroless and runs as non-root.
- No CI workflow lets untrusted input reach an expression.

## 6. Tools Claude installed itself (no admin rights)

| Tool | How |
|---|---|
| gcc (WinLibs), for `-race` on Windows | winget, user scope; `go env -w CC=…` |
| `make` | mingw32-make copied to `~/bin/make.exe` |
| golangci-lint v2.15 | `go install`; older versions cannot read Go 1.27 export data |
| Redocly 1.25.11 | Pinned Docker image behind `make lint-api` |
| actionlint | `go install`; used to check every workflow edit |
| GitHub CLI 2.105 | winget, user scope; the developer logged in in the browser; used to watch CI runs |

Windows quirks were fixed once and recorded, not rediscovered each time. For example, the
testcontainers reaper needs a Docker socket override, now built into `make test`, and Git Bash
needs `MSYS_NO_PATHCONV` for docker volume paths.

## 7. Where the developer steered

- **Decisions:** storage and queue technology, scope (all bonuses), and the language rule: code
  and docs in English, conversation in Turkish.
- **Quality bar:** "We have no time pressure — don't fixate on the 12 hours. Let's do everything
  completely and correctly." Claude took that as permission to verify under load and to go back
  and fix earlier phases.
- **Audits on demand:** an open question ("is anything missing, wrong, or not as intended so
  far?") led to the idempotency fix, the most serious bug found before the reviews.
- **Gates:** every push, and choosing the "right way" when it mattered ("should we install gh? do
  it however is most correct").

## 8. What did not go smoothly

- **Shell quoting:** Python patch scripts embedded in Bash heredocs sometimes failed to parse.
  Claude switched to patch scripts written to a scratch folder, applied by a helper that requires
  every replacement to match exactly once. That also made edits fail loudly instead of silently.
- **Environment drift:** Docker Desktop was off at the start of a session, and Jaeger 2.x had
  removed the API the checks used. Both were noticed because the checks ran for real, not
  assumed.
- **Long sessions:** context compaction dropped detail. The memory file and the ADRs are why the
  later phases stayed consistent with the early ones.
- **First designs that were wrong:** the breaker policy, synchronous events, and the
  template-safety argument that relied on an output cap. Each was caught by load tests or review,
  not by users, and each correction is visible as a `fix:` commit with its reason.
