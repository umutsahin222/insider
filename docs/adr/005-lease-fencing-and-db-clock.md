# ADR-005: Lease fencing and a single queue clock

- Status: Accepted
- Date: 2026-11-18

## Context
ADR-001 gives each claim a lease and lets a reaper return expired leases to the queue. Two
failure modes remained:

1. **Zombie workers.** A worker stalls (GC pause, network partition) past its lease, the reaper
   re-queues the row, a second worker claims it, and then the first worker wakes up and writes
   `sent`/`failed`/`pending` over the second worker's state. `WHERE status = 'processing'` does
   not catch this — the row *is* processing, just for someone else.
2. **Clock skew.** `next_attempt_at` was computed with the API/worker clock but compared with
   Postgres `now()` in the claim query. Found by the integration tests: Docker Desktop's VM
   clock lagged the host, so a retry scheduled for "now" was not claimable yet.

## Decision
- **`attempts` is the fencing token.** The claim increments it and returns the new value; every
  transition out of `processing` is `WHERE id = $1 AND status = 'processing' AND attempts = $2`.
  A stale worker's write matches nothing and gets `domain.ErrLeaseLost`. Its `delivery_attempts`
  row is still committed — the provider call did happen and belongs in the audit trail.
- **The database clock is the only queue clock.** Stores take *durations* (`ScheduleRetry(delay)`,
  `Release(delay)`, `Claim(lease)`) and compute `now() + delay` in SQL; inserts set
  `next_attempt_at = GREATEST(scheduled_at, now())`.
- **Release does not consume an attempt.** Work handed back because the circuit breaker is open,
  the rate limiter is unreachable, or the worker is shutting down decrements `attempts` again.
- **Lease sizing is validated at startup:** `LEASE_DURATION > PROVIDER_TIMEOUT × CLAIM_BATCH_SIZE`,
  so a healthy worker can never lose a lease while working through its batch.

## Consequences
- + Duplicate *state changes* are impossible; duplicate *sends* remain possible only after a real
  stall/crash (at-least-once), mitigated by `X-Idempotency-Key` (ADR-002).
- + Retries and schedules are immune to host clock drift.
- − A reaped attempt counts against `max_attempts` even though its outcome is unknown; we prefer
  that to retrying a possibly-delivered message forever.
- Verified end to end: killing a worker with `-9` mid-batch left 36 rows in `processing`; the
  restarted worker's reaper reclaimed all 36 and they were delivered on attempt 2.
