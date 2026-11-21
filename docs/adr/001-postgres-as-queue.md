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
  well above 100 msg/s × 3 channels). Polling adds small latency (mitigated by short poll interval;
  since ADR-011, new work wakes workers through LISTEN/NOTIFY and polling only catches work
  falling due).
- − Strict priority ordering can starve `low` under sustained `high` load (acceptable; aging can be added).
- − Priority applies at claim time. A claimed row is never overtaken, so up to
  `WORKER_CONCURRENCY × CLAIM_BATCH_SIZE` rows per worker are committed ahead of a high-priority
  notification that arrives later. Batches therefore stay small (2 rows, about 160 ms of work per
  loop at 100/s shared by 8 loops); large batches only save claim queries, since the rate limit,
  not the claim, bounds throughput. *(Added 2026-11-21: with a batch of 10, the acceptance test
  measured the last of 50 high-priority notifications at position 159 behind an earlier
  low-priority backlog once LISTEN/NOTIFY woke every loop at the same moment.)*
- − A queue table churns: every claim, retry and outcome rewrites the row (MVCC), leaving dead
  tuples in the table and its partial indexes. At this project's volume default autovacuum keeps
  up; at sustained high volume the table needs per-table autovacuum settings (lower scale factor,
  higher cost limit) and finished rows should leave it, e.g. by time-partitioning and dropping old
  partitions (README, "Limits and next steps"). Long-running transactions on the primary would
  stall that cleanup, so analytics belong on a replica. *(Added 2026-11-21.)*
- Migration path: if volume outgrows this, keep Postgres as the state store and add an outbox relay
  to Kafka with per-priority topics; the `delivery` package is behind a `Queue` interface.
