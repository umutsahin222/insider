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
documented exception. A final audit then checked every line of the brief against the running
system. It found two real bugs, a related timeout gap, and three mistakes in Claude's own test
expectations. Finally, those one-off checks became a repeatable layer: hooks, six auditor
subagents with a verifier, and skills (section 7, [`claude-code-toolkit.md`](claude-code-toolkit.md)).
The layer found real issues on `main` and caught 8 of 8 planted violations in a blind evaluation.
A second round (section 8) closed gaps and made the system event-driven end to end, and its
verification again found more than the code changes did.

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

## 6. Final audit against the brief

After the reviews, the developer asked: "Does it do everything the assessment specification asks, completely and
correctly? Is anything missing or wrong?" Claude went through the brief line by line, first in
the code, then on the running stack. It wrote an acceptance script with one check per requirement
(69 checks, nearly all of them HTTP calls), plus SQL over `delivery_attempts` to measure rate
limits and priority order. Examples:

- 1000-item batches, and 1001 rejected;
- validation limits counted in characters, including Turkish letters;
- idempotent replay and key reuse;
- keyset pagination over 1000 rows with no duplicates or gaps;
- cancel and its conflicts;
- 2000 notifications across two channels, with 100/s checked per sliding second;
- priority under load;
- scheduled delivery;
- correlation IDs in API and worker logs;
- one Jaeger trace across API, worker and provider;
- `/v1/stats`;
- a WebSocket stream (snapshot, then live events);
- dead letter, then manual retry.

The first run passed 65 of 69. Each of the four failures was examined before anything changed:

| Failure | Verdict |
|---|---|
| An empty request reported only `channel`, because recipient and content checks depended on a valid channel | **Real.** ADR-004 and the README promise every error at once. Missing fields are now reported whatever the channel. |
| A malformed ID in the path returned `404`, the script expected `400` | **Script was wrong.** `404` is a deliberate choice, documented in the code: a malformed ID cannot name any resource. |
| 999 of 1000 SMS were sent | **Script was wrong.** The missing one failed five real injected errors (500, 429, 429, 500, 429) and was dead-lettered as designed. That happens with probability 10⁻⁵ per message, so about 2% of 2000-message runs. The check now accepts a dead letter only if all five attempts were genuine failures. |
| The last high-priority message was sent after 233 of 400 lows | **Script was wrong.** That message was a retry waiting out its backoff. By first attempts, the 50 highs took positions 1–97 (average 36), against an average of 249 for lows queued earlier. The check now measures first attempts. |

The audit also turned up three problems that were not in the script:

- **The README claimed something untested:** it listed "Redis reconnects" among the integration
  tests, but reconnection had only been checked by hand. Claude wrote the missing test (kill the
  hub's pub/sub connection in Redis, expect subscribers dropped and the hub resubscribed). Its first
  version failed. The hub was not at fault: the previous test's hub still held its subscription,
  so the kill hit the wrong connection.
- **Hub shutdown hung (a real bug):** that leftover subscription lived on for 30 s, because
  go-redis reads pub/sub messages without watching the context. So on API shutdown the hub never
  sent WebSocket clients the promised 1013 close. A new test, which fails without the fix, pins
  the behaviour, and `Hub.Run` now closes the subscription when its context ends.
- **Context deadlines were ignored on Redis calls (the same class of bug):** go-redis does not
  apply them to socket I/O by default (`ContextTimeoutEnabled`), so the publisher's documented
  1 s timeout was really the library's 5 s. Both binaries now enable it.

With the fixes deployed, the full run passed 69 of 69. Stopping the API with a stream open now
closed it with 1013 about 1.5 s after the stop command.

The audit also measured something the earlier tests had hidden: the sustained send rate was about
90/s per channel, not 100/s. That is a property of a burst-1 token bucket, which also allowed 101
sends in a 1-second window in the worst case. Claude proposed replacing it with GCRA slot
reservation, and the developer approved, together with a live webhook.site test and a Swagger UI
page. What followed:

- **GCRA limiter:** each send books the next slot atomically in Redis and sleeps until it, with no
  polling. The first measurement showed 103 in one second. Claude traced that to measuring at the
  wrong point: the database timestamps include the mock's random latency. Measured by send start
  the peak was 101, which exposed a real edge: with slots exactly 10 ms apart, timing jitter puts
  101 requests inside one second at the provider. So slots are now (1 s + 10 ms) / 100 apart. To
  measure where it matters, the mock provider now records arrivals per channel. Result: the
  provider's busiest second is 100 with one worker and with three, at 99–100 calls per second.
  The smoke test asserts it in CI.
- **webhook.site:** an SMS, an email and a push sent through a real URL (created and deleted via
  webhook.site's API) arrived in exactly the brief's format with all three headers, got `202`,
  and stored the configured `messageId`. This also proved HTTPS from the distroless image.
- **API docs:** the running API serves its contract at `/openapi.yaml` and renders it with
  Swagger UI at `/docs`.

## 7. Making the audit repeatable

The reviews and the final audit had worked, but each had been a one-off run by hand. The
developer asked for more: the job ad stresses Claude Code skills, and the setup itself should show
them. Claude proposed a plan in plan mode, and the developer approved it with four corrections:
- no machine-specific names in the repository;
- "assessment specification", not the document's file format;
- scripts that run in both Git Bash and Linux CI;
- the evaluation branch must never be pushed.

The result is described in [`claude-code-toolkit.md`](claude-code-toolkit.md):
- hooks that enforce the hard rules, tested in CI;
- directory-scoped `CLAUDE.md` files;
- six read-only auditor subagents and a verifier;
- the `/audit`, `/ship`, `/spec-check` and `/adr` skills;
- an optional PR workflow;
- the acceptance run ported to Go, one test per requirement.

Before writing any agent, hook or workflow file, Claude had a subagent check the current
documentation: frontmatter fields, hook JSON formats, `claude-code-action` inputs.

Then the layer was used and measured, and every run is kept in [`docs/audits/`](audits/):

1. **`/audit all` on `main`** (started by the developer).
   - Eleven raw findings; the verifier confirmed 6, all minor (undocumented 413/403/426,
     incomplete `required` lists, the correlation-ID pattern, two documentation claims), and
     rejected 5 with reasons.
   - All six were fixed with tests.
   - For one fix that the auditor could not revert-check, a missing regression test was added.
     It was mutation-checked: it fails when the deadline is computed per row, while the old test
     still passes.
2. **A weak spot, found by reading the run.**
   - architecture-auditor had "covered" the whole repository in three tool calls. The developer
     asked to harden it.
   - It got four mandatory checks and a `checks_executed` coverage contract, which `/audit` now
     enforces. An empty result without proof of coverage is reported as incomplete, not clean.
   - The first re-run in the same session still returned the old shape: agent definitions only
     load at session start, so a restart was needed.
   - After the restart it took 25 calls instead of 3 and found a real bug the first run had
     missed. The raw HTTP method was a metric label, giving unbounded series. That was fixed,
     and its test fails without the fix.
3. **A blind planted-bug evaluation.**
   - Eight violations and three decoys went into one innocuous-looking commit on a local branch.
     The auditors were told nothing about it.
   - All eight were caught, each by two to four auditors. No decoy drew a correctness alarm. The
     verifier confirmed all 20 findings and found two bugs nobody had planted.
   - One decoy drew a true finding (new code without a test). That is recorded as a flaw in the
     decoy's design, not as an auditor error.
   - While writing the report, Claude noticed that the hardened agent's prompt contained an
     example close to one planted violation. It disclosed this in the report as contamination
     and replaced the example. The violation had been caught by three other auditors anyway.

What this shows about working with Claude Code:
- Instructions alone produce plausible but shallow work. Coverage has to be **asked for and
  checked**, as with `checks_executed`.
- A second, skeptical agent filters over-reporting, and its corrections are worth reading. It
  lowered six severities and rejected half of one claim.
- An evaluation is only as honest as its write-up of its own limits.

## 8. A second round: gap analysis, then architecture

With the project complete, the developer asked for a gap analysis, and whether the queue design
should change to a message broker. Claude recommended against a rewrite. The provider limit, not
the queue, bounds throughput, and the Postgres queue's guarantees were already proven by tests.
Instead it proposed to close the gaps and strengthen the event-driven side. The developer
approved, with one rule: every change is justified on its own merits in the repository.

The plan had three parts:
- small gaps: SMS length by encoding, a dead-letter alert, earlier batch rejection, docs;
- `LISTEN/NOTIFY` wake-ups ([ADR-011](adr/011-wake-workers-with-listen-notify.md));
- a transactional outbox feeding a Redis stream, with resumable WebSocket streams
  ([ADR-012](adr/012-status-event-outbox-and-stream.md)).

The changes were small to write. The verification around them found more:

| What was wrong | How it surfaced | Fix |
|---|---|---|
| SMS length was counted in characters whatever the alphabet, so a text with Turkish letters passed as one SMS but needs up to three UCS-2 segments | Reviewing the validation rules | GSM-7/UCS-2 segment counting, `SMS_MAX_SEGMENTS` ([ADR-004](adr/004-api-conventions.md)) |
| Alert rules were never checked before Prometheus loaded them | Adding the dead-letter alert | `promtool` unit tests in CI (`make lint-alerts`); a broken threshold was shown to fail them |
| After the wake-ups, the acceptance priority test failed: the last of 50 high-priority notifications came 159th behind a low-priority backlog | `make acceptance` | Waking all loops at once exposed head-of-line blocking from 10-row claim batches. Batches of 2: last high at 72 ([ADR-001](adr/001-postgres-as-queue.md)) |
| The provider saw 101–102 requests in one second | The same acceptance run | Isolated step by step: triggers dropped, old poll interval, then a build of the previous commit. All showed it, so the host had grown noisier and 100 had always been the edge. Margin made configurable, four values measured, 50 ms chosen ([ADR-003](adr/003-rate-limiting.md)) |
| go-redis silently retried a failed `XREAD` on a new connection, even to a restarted server, so the event hub never noticed a gap | Integration test that trims the stream and kills the connection | The hub owns a client with retries disabled ([ADR-012](adr/012-status-event-outbox-and-stream.md)) |
| Snapshot events carried the API's clock, stream events the database's, so clients could not order them | Recording real WebSocket output for the README | Snapshots carry the row's `updated_at` |
| A test Claude wrote for "a lost lease records no event" failed | The test itself | The scenario was wrong: a worker never writes after its own release. The test now uses the real path (lease expiry, reaper, re-claim) |
| `make migrate-up/down` failed in Git Bash | Verifying the new migrations up, down and up | `MSYS_NO_PATHCONV` in both targets |

Every new test was also run against the code with its fix removed, to show it fails there: the
wake-up, the replay de-duplication, the batch check and the alert threshold. Measured gains are
in the ADRs:
- new work reaches the provider in p50 15 ms instead of 41 ms;
- idle database commits fell from about 140 to 33 per second;
- a 1000-event burst leaves the outbox within 0.2 s;
- a Redis restart ends WebSocket streams with 1013 and recovers. Both were verified on the
  running stack.

The guard hook also earned its place. During a stress test it refused `docker rm` and
`docker kill` for containers outside this project's namespace. The stress containers were
recreated inside that namespace instead of working around the rule.

## 9. Tools Claude installed itself (no admin rights)

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

## 10. Where the developer steered

- **Decisions:** storage and queue technology, scope (all bonuses), and the language rule: code
  and docs in English, conversation in Turkish.
- **Quality bar:** "We have no time pressure — don't fixate on the 12 hours. Let's do everything
  completely and correctly." Claude took that as permission to verify under load and to go back
  and fix earlier phases.
- **Audits on demand:** an open question ("is anything missing, wrong, or not as intended so
  far?") led to the idempotency fix, the most serious bug found before the reviews.
- **Gates:** every push, and choosing the "right way" when it mattered ("should we install gh? do
  it however is most correct").

## 11. What did not go smoothly

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
