# Evaluation: does the audit layer catch real violations?

- **Date:** 2026-11-20
- **Question:** the Claude Code audit layer in this repository (hooks, five auditor subagents, a
  verifier, see `docs/claude-code-toolkit.md`) is meant to catch violations of this repository's
  rules. Does it, and how many false alarms does it raise?
- **Method:** plant known violations in a realistic-looking change, run the layer blind, and
  compare its output with the list of what was planted.

## Setup

- A local branch `eval/planted-bugs` got one commit on top of `main` (`a557ad5`), titled
  "feat: delivery and API refinements": 9 files, +59/−8.
- It contains **8 planted violations**, each aimed at one rule family, and **3 decoys**: code
  that looks suspicious but is correct, to measure false alarms.
- The developer started `/audit main...HEAD`. The auditors and the verifier were given only the
  range and the file list; nothing told them the branch was planted. The planted diff is
  reproduced in full in Appendix B.
- The branch never left the machine. It had no upstream, the `guard-bash` hook denies pushing
  `eval/*`, and the `pre-push` git hook refuses `eval/*` refs. After the evaluation the branch was
  deleted locally, and `git ls-remote origin` confirms no `eval` ref on the remote.

| # | Planted violation | Location | Rule it breaks |
|---|---|---|---|
| P1 | Lease fence weakened to `attempts >= $2` in `MarkSent` | `internal/store/postgres/queue.go:52` | ADR-005: transitions out of `processing` fenced on `attempts = $n` |
| P2 | Package-level `promauto` counter on the global registry, label `key` | `internal/ratelimit/ratelimit.go:16` | CLAUDE.md: no global state; per-binary registries; closed label sets |
| P3 | Cancel cut-off ("due within 5 s") decided in the handler from a read | `internal/httpapi/notifications.go:245` | CLAUDE.md: business rules never in handlers; no read-then-write |
| P4 | Raw `r.URL.Path` as the HTTP metric `route` label | `internal/httpapi/middleware.go:54` | CLAUDE.md / ADR-007: labels from closed sets, route pattern only |
| P5 | New response field `age_seconds`, spec unchanged | `internal/httpapi/dto.go:85` | CLAUDE.md: `api/openapi.yaml` is the contract |
| P6 | README lists a non-existent integration test `TestBreaker_RecoversAfterOutage` | `README.md:392` | Documentation must match the code |
| P7 | Event publishing made blocking (drop path removed) | `internal/events/events.go:96` | ADR-008: publishing never stalls delivery |
| P8 | Provider `409` made retryable, with no test | `internal/delivery/policy.go:27` | CLAUDE.md: new logic ships with tests |

| # | Decoy (correct code) | Location |
|---|---|---|
| D1 | `Store.Reschedule`: conditional `UPDATE … WHERE status = 'pending'`, database clock | `internal/store/postgres/queue.go:74` |
| D2 | Package-level `const cancelCutoff` and sentinel `var errCancelTooLate` (allowed globals) | `internal/httpapi/notifications.go:345` |
| D3 | Heartbeat goroutine with a proper `ctx.Done()` stop path, joined by the WaitGroup | `internal/delivery/worker.go:164` |

## Results

The layer caught **8 of 8** planted violations, and **0 of 3** decoys drew a correctness false
alarm. The verifier confirmed all 20 de-duplicated findings and rejected none. Six severities were
lowered. In more detail:
- Raw output was 24 findings: architecture 6, concurrency 2, contract 3, claims 8, test
  adequacy 5.
- De-duplication merged four pairs.
- architecture-auditor reported all four mandatory checks with evidence: 22 tool calls, 106 s.

### Which layer caught what

| # | Auditor findings (all confirmed) | Deterministic layers | Existing tests |
|---|---|---|---|
| P1 | concurrency CONC-1, architecture ARCH-4, claims CLAIM-4 | — | Integration `TestFinishAttempt_Fencing` and `TestReaper` fail (run by the verifier) |
| P2 | architecture ARCH-2, claims CLAIM-8, test TEST-5 | — | none |
| P3 | architecture ARCH-3\*, contract CONTRACT-2, test TEST-1, claims CLAIM-6 | Stop hook `contract-drift` blocks: handlers changed without the spec | none |
| P4 | architecture ARCH-1, contract CONTRACT-3, claims CLAIM-3 | — | Unit `TestMetrics` fails |
| P5 | contract CONTRACT-1, architecture ARCH-6, claims CLAIM-7, test TEST-4 | Stop hook `contract-drift` blocks | none |
| P6 | claims CLAIM-1 | — | none (only the claims layer can see this) |
| P7 | concurrency CONC-2, architecture ARCH-5, claims CLAIM-2 | — | Unit `TestPublisher_NeverBlocksTheCaller` times out |
| P8 | test TEST-2, claims CLAIM-5 | — | none (`TestClassify` passes either way, which is the point) |

\* See "Contamination" below.

The value of the subagents is clearest where nothing else catches the problem:
- **P2, P6 and P8** are invisible to the test suite and to the hooks. Only the auditors found
  them.
- **P1** is caught by integration tests that need Docker and that a developer may skip, so the
  auditors catch it without running anything.
- Each planted violation was found by **2–4 independent auditors**. Losing one auditor would not
  have lost a finding.

### Hooks in the same evaluation
- **`guard-bash`:** `git push --force origin eval/planted-bugs` was denied: "Force, mirror and
  delete pushes rewrite shared history…".
- **`contract-drift`:** fed the planted working tree, the Stop hook returned `block`: "HTTP
  handlers changed (dto.go, notifications.go) but api/openapi.yaml did not".
- **`protect-migrations`:** editing an applied migration was refused when tested live during
  setup on `main`. It is covered by the hook test suite (`make test-hooks`, 53 cases, in CI).

### Decoys: honest accounting
- **D1 `Reschedule`: no correctness alarm, but one true finding.**
  - architecture-auditor explicitly reviewed it in its business-rule-leakage check and judged it
    correct (a conditional UPDATE on the database clock). concurrency-auditor did not flag it.
  - test-adequacy-auditor did flag it (TEST-3): new code with **no test and no caller**. The
    verifier confirmed it.
  - That finding is right. CLAUDE.md requires new logic to ship with tests, and
    `internal/store/postgres/CLAUDE.md` requires an integration test for this package. The decoy
    was meant to probe false alarms about *correctness*, but as written it broke the testing
    rule.
  - So TEST-3 is a flaw in the decoy's design, not a false positive of the auditor. A clean
    decoy would have shipped with its test.
- **D2 package-level const and sentinel:** architecture-auditor's global-state scan opened it and
  judged it an allowed immutable sentinel. No alarm.
- **D3 heartbeat goroutine:** no auditor flagged it. No alarm.

### Bonus: the verifier found bugs nobody planted
- **P3 has a second bug.** `ScheduledAt.Sub(now) < 5s` is also true for every *past* schedule.
  So a pending notification whose scheduled time has passed (backlog, retry backoff) could
  never be cancelled individually. The verifier raised this while checking ARCH-3. It was not
  intended when P3 was written.
- **P5 has a second bug.** `age_seconds` uses `time.Since` and ignores the API's injected clock,
  so it is non-deterministic in tests (raised by ARCH-6).
- **Overstated claims corrected.** The verifier pushed back with evidence:
  - the blocking publisher is a long stall, not a deadlock;
  - an unserved global metric does not change what `/metrics` lists (half of CLAIM-8 rejected);
  - an additive field breaks no client, because the schema has no
    `additionalProperties: false`.

## Limits of this evaluation

- **Contamination (ARCH-3).** The hardened `architecture-auditor` prompt contained two hints at
  P3: its example finding described almost exactly a cancel cut-off decided in the handler, and
  its check list named "can be cancelled if…" as an example of an eligibility rule. Both were
  written *after* the violations were planted.
  - P3 was caught independently by three other auditors (contract, test adequacy, claims) and by
    the Stop hook, so the overall result does not depend on ARCH-3. architecture-auditor's P3
    catch should still be discounted.
  - Both hints have since been replaced with examples unrelated to any planted item.
- **Same rule families.** The other auditors' examples were written *before* planting, but each
  illustrates its own rule family, and the planted violations were chosen to exercise exactly
  those families. This evaluation shows that the layer catches the classes of problems it was
  built for. It does not show that it generalises to unseen kinds of bugs.
- **One run, one author.**
  - LLM output varies between runs.
  - The same model family wrote the rules, the agents, the planted bugs and the audit. A second,
    independently written set of planted bugs would be a stronger test.
- **Diff-mode blind spot.** In diff mode, architecture-auditor reported the raw path on the
  changed line `middleware.go:54` but did not re-raise the *pre-existing* problem on the same
  line: the raw HTTP method used as a label, ARCH-1 of `2026-11-20-architecture-recheck.md`. Diff
  audits do not replace periodic `/audit all` runs.
- **Thin passes.** concurrency-auditor used 3 tool calls in diff mode and still caught both of
  its targets, but only architecture-auditor is held to a `checks_executed` coverage contract so
  far. Extending that contract to every auditor is the obvious next step.

## Appendix A: the `/audit main...HEAD` result

| ID (merged) | Severity after verification | Location | Maps to |
|---|---|---|---|
| CONC-1 (+ARCH-4) | high | `queue.go:52` | P1 |
| CONC-2 (+ARCH-5) | high | `events.go:96` | P7 |
| ARCH-1 (+CONTRACT-3) | high | `middleware.go:54` | P4 |
| CLAIM-1 | high | `README.md:392` | P6 |
| ARCH-2 | medium | `ratelimit.go:16` | P2 |
| ARCH-3 | medium | `notifications.go:245` | P3 (+ bonus: past schedules) |
| CONTRACT-1 (+ARCH-6) | medium | `dto.go:85` | P5 (+ bonus: injected clock) |
| CONTRACT-2 | medium | `notifications.go:245` | P3 |
| TEST-1 | medium | `notifications.go:245` | P3 |
| TEST-2 | medium | `policy.go:27` | P8 |
| TEST-3 | medium | `queue.go:74` | D1: true finding, flawed decoy |
| CLAIM-2 | medium | ADR-008:19 | P7 |
| CLAIM-3 | medium | README:313, ADR-007 | P4 |
| CLAIM-4 | medium | ADR-005:20, README | P1 |
| CLAIM-6 | medium | README:66, `openapi.yaml:200` | P3 |
| TEST-4 | low | `dto.go:85` | P5 |
| TEST-5 | low | `ratelimit.go:91` | P2 |
| CLAIM-5 | low | README:284, ADR-002 | P8 |
| CLAIM-7 | low | `openapi.yaml:734` | P5 |
| CLAIM-8 | low | README:426, ADR-007:15 | P2 |

| Agent | Model | Tool calls | Duration | Raw findings |
|---|---|---|---|---|
| architecture-auditor | sonnet, effort high | 22 | 106 s | 6 |
| concurrency-auditor | sonnet | 3 | 15 s | 2 |
| contract-auditor | sonnet | 4 | 39 s | 3 |
| claims-auditor | sonnet | 4 | 86 s | 8 |
| test-adequacy-auditor | sonnet | 8 | 241 s | 5 |
| finding-verifier | opus | 37 | 441 s | 20 verified (20 confirmed, 0 rejected) |

## Appendix B: the planted diff

<details>
<summary>git diff main...eval/planted-bugs (9 files, +59/−8)</summary>

```diff
diff --git a/README.md b/README.md
@@ -389,6 +389,7 @@ make migrate-up   # migrations through the compose migrate service
 Integration tests cover what unit tests cannot:
 - 10 concurrent claimers over 300 rows with no double claims;
 - lease fencing against a zombie worker;
+- circuit breaker recovery after a full provider outage (`TestBreaker_RecoversAfterOutage`);
 - idempotent creates with 20 concurrent callers;
diff --git a/internal/delivery/policy.go b/internal/delivery/policy.go
@@ -24,7 +24,7 @@ func Classify(reply domain.ProviderReply, err error) domain.AttemptOutcome {
-	case s == http.StatusRequestTimeout, s == http.StatusTooManyRequests, s >= 500:
+	case s == http.StatusRequestTimeout, s == http.StatusConflict, s == http.StatusTooManyRequests, s >= 500:
 		return domain.OutcomeRetryableError
diff --git a/internal/delivery/worker.go b/internal/delivery/worker.go
@@ -161,6 +161,18 @@ func (w *Worker) Run(ctx context.Context) {
 	for range w.cfg.Concurrency {
 		wg.Go(func() { w.loop(ctx) })
 	}
+	heartbeat := time.NewTicker(time.Minute)
+	defer heartbeat.Stop()
+	wg.Go(func() {
+		for {
+			select {
+			case <-ctx.Done():
+				return
+			case <-heartbeat.C:
+				w.log.Debug("worker heartbeat", "breaker", w.breaker.State().String())
+			}
+		}
+	})
 	wg.Wait()
diff --git a/internal/events/events.go b/internal/events/events.go
@@ -93,11 +93,7 @@ func (p *Publisher) Publish(_ context.Context, evs ...domain.StatusEvent) {
 	for _, ev := range evs {
-		select {
-		case p.queue <- ev:
-		default:
-			p.dropped.Add(1)
-		}
+		p.queue <- ev
 	}
diff --git a/internal/httpapi/dto.go b/internal/httpapi/dto.go
@@ -82,6 +82,7 @@ type notificationResponse struct {
 	SentAt            *time.Time `json:"sent_at"`
+	AgeSeconds        float64    `json:"age_seconds"`
 }
@@ -105,6 +106,7 @@ func toNotificationResponse(n domain.Notification) notificationResponse {
 		SentAt:            utcPtr(n.SentAt),
+		AgeSeconds:        time.Since(n.CreatedAt).Seconds(),
 	}
diff --git a/internal/httpapi/middleware.go b/internal/httpapi/middleware.go
@@ -51,7 +51,7 @@ func (a *api) observe(next http.Handler) http.Handler {
-		a.metrics.ObserveRequest(r.Method, route, status, elapsed)
+		a.metrics.ObserveRequest(r.Method, r.URL.Path, status, elapsed)
diff --git a/internal/httpapi/notifications.go b/internal/httpapi/notifications.go
@@ -242,6 +242,15 @@ func (a *api) cancelNotification(w http.ResponseWriter, r *http.Request) {
+	current, err := a.store.GetNotification(r.Context(), id)
+	if err != nil {
+		a.writeError(w, r, err)
+		return
+	}
+	if current.ScheduledAt != nil && current.ScheduledAt.Sub(a.now()) < cancelCutoff {
+		a.writeError(w, r, errCancelTooLate)
+		return
+	}
 	n, err := a.store.CancelNotification(r.Context(), id)
@@ -332,3 +341,8 @@ func pathUUID(r *http.Request) (uuid.UUID, bool) {
+
+// cancelCutoff is how close to its scheduled time a notification can still be cancelled.
+const cancelCutoff = 5 * time.Second
+
+var errCancelTooLate = domain.Conflict("notification is due within 5 seconds and can no longer be cancelled")
diff --git a/internal/ratelimit/ratelimit.go b/internal/ratelimit/ratelimit.go
@@ -8,9 +8,16 @@ import (
+	"github.com/prometheus/client_golang/prometheus"
+	"github.com/prometheus/client_golang/prometheus/promauto"
 	"github.com/redis/go-redis/v9"
 )
 
+var reservations = promauto.NewCounterVec(prometheus.CounterOpts{
+	Name: "ratelimit_reservations_total",
+	Help: "Rate-limit slots booked, by key.",
+}, []string{"key"})
+
@@ -81,6 +88,9 @@ func (l *Limiter) Reserve(ctx context.Context, key string, maxWait time.Duration
+	if res[0] == 1 {
+		reservations.WithLabelValues(key).Inc()
+	}
 	return res[0] == 1, time.Duration(res[1]) * time.Microsecond, nil
diff --git a/internal/store/postgres/queue.go b/internal/store/postgres/queue.go
@@ -48,7 +49,7 @@ func (s *Store) MarkSent(ctx context.Context, n domain.Notification, providerMes
-		WHERE id = $1 AND status = 'processing' AND attempts = $2`,
+		WHERE id = $1 AND status = 'processing' AND attempts >= $2`,
@@ -70,6 +71,21 @@ func (s *Store) MarkFailed(ctx context.Context, n domain.Notification, a domain.
+// Reschedule moves a pending notification's next attempt to delay from now.
+func (s *Store) Reschedule(ctx context.Context, id uuid.UUID, delay time.Duration) error {
+	tag, err := s.pool.Exec(ctx, `
+		UPDATE notifications SET next_attempt_at = now() + $2 * interval '1 millisecond', updated_at = now()
+		WHERE id = $1 AND status = 'pending'`,
+		id, delay.Milliseconds())
+	if err != nil {
+		return fmt.Errorf("rescheduling notification: %w", err)
+	}
+	if tag.RowsAffected() == 0 {
+		return domain.Conflict("only pending notifications can be rescheduled")
+	}
+	return nil
+}
```

(Unchanged context lines trimmed; the `uuid` import added to `queue.go` is omitted.)
</details>

This evaluation changed no product code; the planted branch was deleted and never pushed.
