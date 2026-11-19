# ADR-002: Delivery and retry strategy

- Status: Accepted
- Date: 2026-11-18

## Context
The provider (webhook.site / mock) may time out, return 5xx, throttle with 429, or reject a request
with 4xx. Retrying everything wastes capacity and can hammer a degraded provider; retrying nothing
loses messages during transient outages.

## Decision
**Classification** of each attempt:

| Outcome | Action |
|---|---|
| 202 Accepted | `sent`, store provider `messageId` |
| timeout / network error / 5xx | retryable |
| 429 | retryable, honour `Retry-After` if present |
| other 4xx | permanent → `failed` immediately |

**Backoff:** `delay = rand(0, min(base·2^(attempt-1), max))` (full jitter), base = 2s, max = 5m,
`max_attempts` = 5 (configurable, stored per notification). Next attempt is scheduled by setting
`next_attempt_at`, so retries re-enter the same priority queue instead of blocking a worker.

**Circuit breaker per channel** (gobreaker): only retryable failures count. It opens when, within a
10 s window, at least 20 calls were made and ≥ 50 % failed, or after 10 consecutive failures (a
hard outage at low traffic). While open (30 s) the channel's workers stop claiming and hand claimed
work back without using an attempt, so attempts are not burned against a dead provider.

*Revised 2026-11-19:* the first version opened after 5 consecutive failures. The phase 4 metrics
showed why that is wrong: against a provider failing 30 % of the time at random, a streak of 5 is
expected a few times per thousand calls; each trip paused a working channel for 30 s, and SMS
end-to-end p50 latency rose to 65 s. A ratio over a minimum sample separates "degraded" from
"down"; the consecutive rule (now 10, ≈ 6·10⁻⁶ at 30 %) only catches total outages. All thresholds
are configurable (`BREAKER_*`).

**Exhausted attempts** → `failed` with `last_error`. Failed notifications are the dead-letter set:
queryable via the list API and re-queueable with `POST /v1/notifications/{id}/retry`. A manual
retry raises `max_attempts` to `attempts + MAX_ATTEMPTS` instead of resetting `attempts` to 0:
`attempts` is the lease fencing token (ADR-005) and part of the `delivery_attempts` key, so a
reset would let a stale worker's write match a new attempt and collide with the audit rows.
Attempt numbers simply continue (6, 7, …).

Every attempt is recorded in `delivery_attempts` (status code, error, duration) for auditability.

**Duplicate protection:** state transitions are conditional on `status='processing'`; the provider
receives `X-Idempotency-Key: <notification id>`; leases make a crashed worker's work reclaimable.

## Consequences
- Delivery is at-least-once; exactly-once requires provider-side dedup via the idempotency key.
- Full jitter avoids thundering herds after an outage.
