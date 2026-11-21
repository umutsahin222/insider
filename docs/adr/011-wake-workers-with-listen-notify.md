# ADR-011: Wake idle workers with LISTEN/NOTIFY

- Status: Accepted
- Date: 2026-11-21

## Context
Workers found new work only by polling: every claim loop (8 per channel per replica) queried the
queue every 200 ms when idle. That made idle cost and pickup latency two ends of one dial: on the
compose stack one idle worker produced about 140 database commits per second, and a new
notification still waited for the next poll of some loop. Lowering the interval would cut latency
and raise load further; raising it would do the opposite.

## Decision
- **The database announces claimable work.** Migration 000006 adds statement-level `AFTER INSERT`
  and `AFTER UPDATE` triggers on `notifications` that `pg_notify('notifications_ready', channel)`
  once per channel for rows that are `pending` and due now. That covers every way work becomes
  claimable at once: single and batch creates, manual retry, release, and reaped leases. NOTIFY is
  delivered on commit, so a worker never wakes before the rows are visible. Scheduled rows and
  retries that back off are not announced.
- **One listening session per worker process** (`Store.ListenForReadyWork`), on a connection taken
  out of the pool. If it breaks, it reconnects with backoff (1 s to 30 s) and then reports every
  channel once, because announcements made while disconnected are lost.
- **Idle loops wait on a per-channel broadcast** (`delivery.Signal`). A loop takes the signal's
  channel *before* it claims and waits on it after an empty claim, so an announcement that arrives
  during the claim is not missed.
- **Polling stays, slower:** `POLL_INTERVAL` defaults to 1 s instead of 200 ms. It picks up work
  falling due (scheduled times, backoffs) and keeps everything working if the listener is down;
  announcements are hints, never the only path to work.

Alternatives: NOTIFY from Go after each write needs every write path to remember it, while the
trigger also covers paths added later and manual fixes in SQL. Redis pub/sub would be a second
write after the commit that can fail on its own. A shorter poll interval keeps the trade-off this
decision removes.

## Consequences
- + Measured on the compose stack (one worker, mock provider, 100 notifications created 300 ms
  apart so the workers are idle before each): time from creation to the provider call fell from
  p50 41 ms / p90 115 ms to p50 15 ms / p90 31 ms; idle database commits fell from about 140 to
  33 per second.
- − NOTIFY takes a cluster-wide lock at commit, so notifying transactions serialise. Only creates,
  manual retries, releases and reaps notify (one NOTIFY per channel and statement; claims and
  outcomes do not), which is far from that limit here. Past several thousand single creates per
  second, clients should use the batch endpoint or the API should coalesce announcements.
- − An announcement wakes every idle loop of the channel; for a single row all but one find
  nothing. That costs one indexed `SKIP LOCKED` query per loop.
- − Triggers are invisible from Go code. This ADR, the migration and
  `listen_integration_test.go` (one case per path, plus a killed listening session) document them.
