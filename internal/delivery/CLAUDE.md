# internal/delivery

The worker loop: claim → rate-limit slot → circuit breaker → send → record (ADR-002, -003, -005).
Invariants for this package:

- **Never start a send the lease might not cover.** Each claimed row has a send deadline
  (`sendDeadline`: claim time + lease − send timeout − `StoreTimeout`). The rate-limit wait is
  bounded by it; a row that cannot start in time is released as `lease_expiring`.
- **No provider call without a rate-limit slot.** `Limiter.Wait` errors wrapping
  `context.DeadlineExceeded` mean "slot too late" (release as `lease_expiring`). Other errors
  mean the limiter is down: fail closed and release.
- **Breaker:** while open, nothing is claimed. Claimed rows are released without consuming an
  attempt. In half-open, only the loop holding `probing` books a slot.
- **Wake-ups:** an idle loop takes `wake.C()` *before* claiming and waits on it after an empty
  claim (ADR-011). Taking it after the claim would lose an announcement made during the claim.
  Polling stays as the fallback; never make a wake-up the only path to work.
- Outcome writes run detached from shutdown (`context.WithoutCancel`), always with
  `StoreTimeout`. Any new I/O must honour a context with a deadline.
- Every recorded outcome publishes a status event (best-effort, never blocking). A lost lease
  publishes nothing: the newer attempt owns the outcome.
- Metrics and release reasons come from the closed sets in `metrics.go`. Never label by ID.
- Log through the notification's context (`notificationContext`) so `correlation_id` and
  `notification_id` are attached exactly once.
- Behaviour changes need a unit test against the fakes in `worker_test.go` / `lease_test.go`,
  and the test must fail without the change.
