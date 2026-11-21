# Requirements traceability

Every requirement of the assessment specification, the acceptance test that checks it against
the running system (`make acceptance`, black-box: public API, mock provider stats, Prometheus,
Jaeger, container logs), and where the design is recorded. Unit and integration tests
(`make test`) cover the same behaviour from the inside.

| ID | Requirement (assessment specification) | Acceptance test | Design |
|---|---|---|---|
| API-1 | Create notification requests with recipient, channel, content and priority | `TestREQ_API1_CreateWithRecipientChannelContentPriority` | ADR-004 |
| API-2 | Batch creation, up to 1000 notifications per request | `TestREQ_API2_BatchUpTo1000`, `TestREQ_API2_BatchIsAllOrNothing` | ADR-004 |
| API-3 | Query notification status by ID or batch ID | `TestREQ_API3_QueryByIDAndBatchID` | ADR-004 |
| API-4 | Cancel pending notifications | `TestREQ_API4_CancelPending` | ADR-001 (conditional UPDATE) |
| API-5 | List notifications with filtering (status, channel, date range) and pagination | `TestREQ_API5_ListFilterAndPaginate` | ADR-004 (keyset pagination) |
| ENG-1 | Process notifications asynchronously via queue workers | `TestREQ_ENG2_RateLimit100PerSecondPerChannel` (2000 delivered by workers), `TestREQ_DEL_RetriesAreAudited` | ADR-001, ADR-005 |
| ENG-2 | Rate limiting: at most 100 messages per second per channel | `TestREQ_ENG2_RateLimit100PerSecondPerChannel` (busiest second measured at the provider, two channels at once); `scripts/smoke.sh` repeats it with three workers in CI | ADR-003 |
| ENG-3 | Priority queue support (high, normal, low) | `TestREQ_ENG3_PriorityQueue` | ADR-001 |
| ENG-4 | Content validation (character limits, required fields) | `TestREQ_ENG4_ContentValidation` | ADR-004 |
| ENG-5 | Idempotency support to prevent duplicate sends | `TestREQ_ENG5_Idempotency` | ADR-006, ADR-005 (fencing, `X-Idempotency-Key`) |
| DEL | Delivery and retry logic (design left to the candidate) | `TestREQ_DEL_RetriesAreAudited`, `TestREQ_BONUS_DeadLetterAndManualRetry` | ADR-002, ADR-005 |
| OBS-1 | Real-time metrics endpoint: queue depth, success/failure rates, latency | `TestREQ_OBS1_Metrics` | ADR-007 |
| OBS-2 | Structured logging with correlation IDs | `TestREQ_OBS2_StructuredLogsWithCorrelationIDs` | ADR-007 |
| OBS-3 | Health check endpoint | `TestREQ_OBS3_HealthChecks` | ADR-010 |
| PROV | External provider integration: `{to, channel, content}`, `202` with `messageId` | every delivery test: the mock provider decodes `{to, channel, content}`, rejects requests without `to` or `content` with `400`, and answers `202` with a `messageId` that is stored; verified live against webhook.site (README) | ADR-002, ADR-010 |
| BONUS-FAIL | Failure handling | `TestREQ_BONUS_DeadLetterAndManualRetry`, `TestREQ_DEL_RetriesAreAudited` | ADR-002, ADR-005 |
| BONUS-SCHED | Scheduled notifications | `TestREQ_BONUS_ScheduledNotifications` | ADR-001 |
| BONUS-TPL | Template system with variable substitution | `TestREQ_BONUS_Templates` | ADR-009 |
| BONUS-WS | Real-time status updates via WebSocket | `TestREQ_BONUS_WebSocket`, `TestREQ_BONUS_WebSocketResume` | ADR-008, ADR-012 |
| BONUS-TRACE | Distributed tracing | `TestREQ_BONUS_DistributedTracing` | ADR-007 |
| BONUS-CI | GitHub Actions CI/CD: automated testing and linting | `.github/workflows/ci.yml` (lint, tests with `-race`, compose e2e with smoke and this suite) | ADR-010 |
| DELIV-1 | Source code with clean commit history | Conventional Commits, one logical change each | `docs/ai-workflow.md` |
| DELIV-2 | README: setup, architecture overview, API examples | `README.md` (every curl example was run against the stack) | — |
| DELIV-3 | One-command setup (`docker-compose up`) | CI e2e job starts the stack from scratch | ADR-010 |
| DELIV-4 | API documentation (OpenAPI) | `TestREQ_DELIV_APIDocumentation`; `make lint-api` in CI | ADR-004 |
| DELIV-5 | Versioned database migrations | `migrations/` (golang-migrate); edits to applied migrations are blocked by a hook | ADR-001 |
| DELIV-6 | Test suite runnable with a single command | `make test` (unit + integration), `make acceptance` | ADR-010 |

Run `/spec-check` in Claude Code to run this suite and get every failure mapped back to this
table, with a verdict on whether the product or the test expectation is wrong.
