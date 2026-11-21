# internal/store/postgres

Postgres is the source of truth and the work queue (ADR-001). Invariants for this package:

- **Every status change is one conditional UPDATE** (`WHERE status = <expected>`), never a read
  followed by a write.
- **Transitions out of `processing` are fenced on the attempt number:**
  `WHERE id = $1 AND status = 'processing' AND attempts = $2`. Zero rows affected means a newer
  attempt owns the row: return `domain.ErrLeaseLost`, never retry the write (ADR-005).
- **The database clock is the only queue clock.** Methods take durations
  (`Claim(lease)`, `ScheduleRetry(delay)`, `Release(delay)`) and compute `now() + …` in SQL.
  Never pass `time.Now()` from Go into queue timing.
- `Release` hands the attempt back (`attempts - 1`). A reaped attempt counts.
- The reaper's give-up rule (`attempts >= max_attempts`) is the one piece of retry policy allowed
  in SQL. It is a documented exception (ADR-005); do not add others. Business rules belong in
  `internal/domain` or `internal/delivery`.
- Triggers on `notifications` (migration 000006) `NOTIFY notifications_ready` when rows become
  pending and due now; `ListenForReadyWork` relays them to the workers (ADR-011). A new way of
  making work claimable needs no Go change, but add a case to `listen_integration_test.go`.
- Queries are parameterised (`$n`) only. Dynamic SQL may join fixed fragments, never values.
- Schema changes go into a **new** migration (`migrations/`); a hook blocks edits to committed ones.
- Changes here need an integration test (`//go:build integration`, testcontainers).
  Concurrency claims (SKIP LOCKED, fencing, idempotency races) are tested with real goroutines.
