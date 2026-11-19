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
  and `> PROVIDER_TIMEOUT + 10 s` (the store-write timeout), as a sizing hint.
- **A worker never starts a send its lease might not cover.** Sizing alone is not enough: the
  rows of a claimed batch wait for rate-limit tokens one after another, and with many replicas
  sharing a 100/s bucket (or a low `RATE_LIMIT_PER_SECOND`) a late row can wait past its lease,
  be reaped and claimed elsewhere, and then be sent by both workers. Found by the phase 7 code
  review. Each row therefore has a send deadline, `claim time + lease − send timeout − store
  timeout`, measured on the worker's monotonic clock from just *before* the claim (so it is never
  later than the lease start on the database). The rate-limit wait is bounded by it; a row that
  reaches it is released (`lease_expiring`, attempt handed back, due at once) instead of sent.
  Outcome writes run detached from shutdown but with their own 10 s timeout, so a stalled
  database cannot hold the process open either.
- **The reaper's give-up rule is the one piece of retry policy in SQL**
  (`CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending'`). It is a deliberate
  exception to "no business rules in SQL": reaping must be one set-based conditional UPDATE, and
  moving the decision to Go would mean reading rows and writing them back. The rule mirrors the
  worker's (`attempts < max_attempts` retries) and is covered by the reaper integration tests.

## Consequences
- + Duplicate *state changes* are impossible; duplicate *sends* remain possible only after a real
  stall/crash (at-least-once), mitigated by `X-Idempotency-Key` (ADR-002).
- + Retries and schedules are immune to host clock drift.
- − A reaped attempt counts against `max_attempts` even though its outcome is unknown; we prefer
  that to retrying a possibly-delivered message forever.
- Verified end to end: killing a worker with `-9` mid-batch left 36 rows in `processing`; the
  restarted worker's reaper reclaimed all 36 and they were delivered on attempt 2.
