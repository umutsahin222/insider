# ADR-001: PostgreSQL as source of truth and work queue

- Status: Accepted
- Date: 2026-11-18

## Context
Notifications must be durable, queryable (status, filters, pagination), cancellable while pending,
prioritised (high/normal/low), schedulable for the future and retried with backoff.
A separate broker (RabbitMQ/Kafka) would require keeping the DB and the broker consistent
(dual write → transactional outbox + relay) and would still need a DB for status queries.
Kafka has no native priority or delayed delivery; RabbitMQ needs DLX/TTL tricks for delays and
cannot cancel an already-published message.

## Decision
The `notifications` table is the queue. Workers claim work with:

```sql
UPDATE notifications SET status='processing', locked_until=now()+$lease, attempts=attempts+1
WHERE id IN (
  SELECT id FROM notifications
  WHERE status='pending' AND channel=$1 AND next_attempt_at <= now()
  ORDER BY priority, next_attempt_at
  LIMIT $2 FOR UPDATE SKIP LOCKED)
RETURNING ...;
```

- Priority, scheduling and retry backoff are all expressed through `priority` + `next_attempt_at`.
- A partial index `(channel, priority, next_attempt_at) WHERE status='pending'` keeps claims O(log n).
- Cancel is a conditional `UPDATE ... WHERE status='pending'`.
- A lease (`locked_until`) plus a reaper returns work from crashed workers → at-least-once delivery;
  the notification ID is sent to the provider as an idempotency key.

## Consequences
- + Single source of truth, transactional create (incl. 1000-item batches), no outbox.
- + Simple ops: Postgres + Redis only.
- − Throughput ceiling is the DB's write capacity (~low thousands of claims/s on modest hardware —
  well above 100 msg/s × 3 channels). Polling adds small latency (mitigated by short poll interval).
- − Strict priority ordering can starve `low` under sustained `high` load (acceptable; aging can be added).
- Migration path: if volume outgrows this, keep Postgres as the state store and add an outbox relay
  to Kafka with per-priority topics; the `delivery` package is behind a `Queue` interface.
