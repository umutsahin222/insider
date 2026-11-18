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

**Circuit breaker per channel** (gobreaker): opens after consecutive failures; while open the
channel's workers stop claiming, so attempts are not burned against a dead provider.

**Exhausted attempts** → `failed` with `last_error`. Failed notifications are the dead-letter set:
queryable via the list API and re-queueable with `POST /v1/notifications/{id}/retry`.

Every attempt is recorded in `delivery_attempts` (status code, error, duration) for auditability.

**Duplicate protection:** state transitions are conditional on `status='processing'`; the provider
receives `X-Idempotency-Key: <notification id>`; leases make a crashed worker's work reclaimable.

## Consequences
- Delivery is at-least-once; exactly-once requires provider-side dedup via the idempotency key.
- Full jitter avoids thundering herds after an outage.
