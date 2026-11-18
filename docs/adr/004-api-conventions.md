# ADR-004: API and data-model conventions

- Status: Accepted
- Date: 2026-11-18

## Context
Several small choices shape the contract (`api/openapi.yaml`) and schema (`migrations/`) and are
hard to change once clients depend on them.

## Decision
- **IDs are UUIDv7**, generated in the application. They are time-ordered, so inserts into the
  primary-key B-tree stay append-mostly under burst load, and the ID is known before the INSERT
  (useful for logs and batch responses).
- **Errors are RFC 9457 Problem Details** with an `errors[]` list of `{field, message}` and the
  `correlation_id`. Validation reports every invalid field at once, not only the first.
  `400` = malformed request, `422` = semantically invalid, `409` = state conflict / duplicate.
- **Batches are all-or-nothing:** validated as a whole, inserted in a single transaction; item
  errors are addressed as `items[i].field`. Partial acceptance would force clients to reconcile
  which items were queued.
- **Idempotency:** single create uses the `Idempotency-Key` header; a replay returns the original
  notification with `200` + `Idempotent-Replayed: true`. Batch items carry `idempotency_key`; a key
  that already exists rejects the batch with `409`. Keys are unique globally (partial unique index).
- **Scheduling:** `scheduled_at` only sets the initial `next_attempt_at`; values in the past mean
  "now", values more than 30 days ahead are rejected.
- **Content limits** are counted in characters (runes) and configurable: SMS 160 (one GSM-7
  segment; multipart is out of scope), email 10 000, push 256.
- **Pagination** is keyset over `(created_at DESC, id DESC)` with an opaque cursor — stable under
  concurrent inserts and O(log n) per page, unlike OFFSET.
- The DB enforces invariants too (enum types, CHECK constraints such as "sent ⇒ sent_at set"), as a
  backstop for the rules in `internal/domain`.

## Consequences
- Idempotency keys are not scoped per client; a multi-tenant version would add a tenant column to
  the unique index.
- ~~The idempotency check does not compare payloads; reusing a key with a different body returns
  the original notification.~~ Superseded by ADR-006: requests are fingerprinted, a reused key with
  a different body is rejected with `422`, and batches accept their own `Idempotency-Key`.
