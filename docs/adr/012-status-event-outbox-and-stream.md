# ADR-012: Status events through a transactional outbox and a Redis stream

- Status: Accepted (replaces the event transport of ADR-008; its WebSocket contract stays)
- Date: 2026-11-21

## Context
ADR-008 carried status events over Redis pub/sub: workers and the API published each event
after their own database write. That had two weaknesses.

- **A dual write.** A transition could commit while its event was lost: a crash between the
  commit and the publish, Redis down, or the bounded publish queue full. Pub/sub also keeps
  nothing, so every Redis hiccup had to close every WebSocket stream (1013) to force a resync.
- **Every write path had to remember to publish:** claim, outcome and release in the worker,
  the reaper, cancel, batch cancel and retry in the API. Creation was never published at all.

The work queue needs no outbox, because the row is the job (ADR-001). Events are different:
they leave the database for other consumers, and only an outbox makes that reliable.

ADR-008 had rejected Redis Streams because they need retention and per-client cursors. With an
outbox the trade-off changes. A cursor turns out to be only the `event_id` the client already
holds, retention is one `MAXLEN`, and a stream is what lets the hub survive a reconnect without
dropping everyone.

## Decision
- **The outbox is written by the statement that changes the status.** Migration 000007 adds
  statement-level triggers on `notifications`. They insert one `status_event_outbox` row per
  created notification and per status change, mirroring the row after the change (status,
  attempts, `last_error`, `updated_at`). An event exists if and only if the change committed;
  no write path can forget one, and a fenced UPDATE that lost its lease changes nothing and
  records nothing. The triggers capture changes and decide nothing, so no business rule moves
  into SQL. Writing the outbox in each store method instead would mean nine call sites to keep
  right, and a missed one would be a silent gap.
- **One relay at a time, elected by an advisory lock.** Every worker runs `events.Relay`.
  `pg_try_advisory_lock` on a dedicated session picks the leader, and its reads and deletes run
  on that same session, so a leader whose session died cannot act. Each round it reads up to
  500 events oldest first, appends them to the stream `notification.events` in one pipeline
  (`XADD MAXLEN ~ EVENT_STREAM_MAXLEN`, default 100 000), then deletes exactly those ids. It
  never deletes a range, because a transaction that commits late can hold a lower id. Events of
  one notification stay in order: its transitions serialise on the row lock, so a later event
  always gets a higher id and becomes visible after the earlier one. Delivery is at least
  once; a crash between `XADD` and the delete repeats events.
- **A Redis stream, not pub/sub and not Kafka.** A stream keeps entries, so readers resume
  after a disconnect and clients can catch up. Redis is already a dependency. A log broker
  such as Kafka is the next step at higher volume, and needs only a different relay target.
- **The hub reads with `XREAD` from its last position.** After a broken connection it resumes,
  and subscribers stay connected. It ends subscriptions (1013) only on a gap:
  - Redis restarted (its `run_id` changed), so it may have lost acknowledged entries;
  - the stream was trimmed past the hub's position.

  In both cases the hub re-positions at the stream's end. It opens its own client with go-redis
  retries disabled. Otherwise go-redis retries a failed `XREAD` on a new connection, even to a
  restarted server, and the hub never sees the failure that makes it check for a gap. An
  integration test found this.
- **WebSocket clients can resume.** Every event carries `event_id`, its stream entry ID. On
  reconnect with `?after=<event_id>` the API replays from the stream, but only if it can prove
  completeness:
  - the position lies within the hub's uninterrupted history on the current Redis server;
  - the entry itself has not been trimmed;
  - at most 10 000 entries follow it.

  Otherwise the client gets a snapshot as before. Subscribing first still leaves no gap, and
  replayed events that the hub also delivers live are skipped by event ID.

## Consequences
- + No status event is lost while Postgres works. A Redis outage delays events, and they are
  relayed in order when Redis returns. `notification_outbox_oldest_age_seconds` and the
  `StatusEventsNotRelayed` alert show when that happens.
- + A Redis connection blip no longer disconnects WebSocket clients. A reconnecting client gets
  only what it missed.
- + Workers and the API publish nothing. The publisher, its bounded queue and every publish call
  are gone.
- + Creation is an event too, so the stream is a complete lifecycle log. Other consumers
  (customer webhooks, analytics) can read it with consumer groups without touching delivery.
- − Every transition also inserts an outbox row and later deletes it: about three per
  notification (created, processing, sent), roughly 900 rows per second at the provider limit.
  The table stays small, but autovacuum has to keep up.
- − An event reaches clients up to the relay's 100 ms poll interval later than a direct publish
  did.
- − If Redis restarts without persistence, resumable history is lost and clients fall back to
  snapshots: correct, but heavier.
- Measured on the compose stack (one worker):
  - A 1000-item batch's 1000 events left the outbox within 0.1–0.2 s of the commit (three runs).
  - Creating a 1000-item batch took 57–112 ms with or without the outbox trigger (five runs
    each), so the difference is within noise.
  - The acceptance suite's resume test reconnects mid-batch and receives what it missed, then
    the rest live, without a snapshot (45 events in one run).
