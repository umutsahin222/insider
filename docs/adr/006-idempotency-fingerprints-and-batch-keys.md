# ADR-006: Idempotency fingerprints and batch-level keys

- Status: Accepted (amends the idempotency bullet of ADR-004)
- Date: 2026-11-19

## Context
ADR-004 made create requests idempotent via the `Idempotency-Key` header, but a self-review
against the brief ("idempotency support to prevent duplicate sends") found two gaps:

1. **Key reuse went undetected.** A second request with the same key but a *different* body got
   the original notification back with `200`. The client believes its new message was queued;
   it never will be. That is silent data loss, not idempotency.
2. **Batches could not be retried safely.** A client that times out on `POST /batch` and retries
   either creates up to 1000 duplicates (items without keys) or gets a `409` for the whole batch
   (items with keys) — never the batch it actually created.

## Decision
- **Every create request is fingerprinted** in `internal/domain`: SHA-256 over the fields that
  decide what is delivered (channel, recipient, content, priority, scheduled_at, template),
  after normalisation (omitted priority ≡ `normal`, timestamps in UTC at microsecond precision).
  Fields are length-prefixed so boundaries are unambiguous, and the hash is versioned
  (`notification/v1`). Correlation ID and the key itself are excluded. Stored in
  `notifications.request_hash`.
- **Replay vs. reuse** is a domain rule (`domain.VerifyReplay`): same key + same fingerprint →
  `200` with `Idempotent-Replayed: true`; same key + different fingerprint → `422` "Idempotency key
  reused" (the status the IETF `Idempotency-Key` header draft recommends). Rows created before the
  column existed have no fingerprint and are accepted as replays.
- **Batches are first-class rows** (`batches`: id, idempotency_key, request_hash, size) with a
  foreign key from `notifications.batch_id`. `POST /batch` accepts an `Idempotency-Key` header;
  the batch fingerprint covers every item's fingerprint and item key, in order. The batch row is
  inserted first with `ON CONFLICT DO NOTHING` in the same transaction as the items, so
  concurrent retries serialise on the unique index; a replay returns the original `batch_id` and
  `notification_ids` in request order (UUIDv7 IDs are generated in item order).
- Item-level `idempotency_key` keeps its ADR-004 meaning: an existing item key rejects the whole
  batch with `409`.

## Consequences
- + A retried request is answered with exactly what the first one created; a mistaken key reuse
  fails loudly instead of dropping a message.
- + Race-free without locks or read-then-write: both checks resolve through unique indexes.
- − The fingerprint definition is now part of the contract: adding a field that affects delivery
  (e.g. template variables in phase 5) must go into the hash, bumping the version prefix.
- − Keys are still global and never expire; a production system would scope them per client and
  purge them after a retention window (e.g. 24 h).
