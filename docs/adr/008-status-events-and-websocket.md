# ADR-008: Status events over Redis pub/sub, streamed via WebSocket

- Status: Accepted
- Date: 2026-11-19

## Context
Clients want to watch a notification or a batch change state in real time. State changes happen
in worker processes (and some in the API: cancel, retry), while clients connect to any API
replica. Something has to carry events between processes, and the WebSocket contract has to stay
correct when that transport hiccups.

## Decision
- **Transport: Redis pub/sub**, one channel `notification.events`, JSON `StatusEvent`s. Redis is
  already a dependency (rate limiter); pub/sub adds no storage and no consumer bookkeeping.
- **Producers:** workers publish `processing`, `sent`, `pending` (retry scheduled, or work
  released with the attempt handed back) and `failed`; the reaper publishes what it reclaimed;
  the API publishes cancellations and manual retries. No event when a lease was lost — the newer
  attempt owns that notification's outcome. Publishing is **best-effort and asynchronous**:
  `Publish` only enqueues (bounded queue of 8192; drops are counted in a log line when full), and
  one goroutine per process sends events in pipelined batches, in publish order, with a 1 s
  timeout per round trip. A slow or unreachable Redis therefore never stalls a delivery or an API
  request (the phase 7 code review found the earlier synchronous publish added two blocking round
  trips per send). Shutdown flushes the queue for up to 2 s.
- **One subscription per API process** (`events.Hub`), fanned out in memory to WebSocket
  connections by notification or batch ID. The hub never blocks on a subscriber: one whose buffer
  is full is dropped.
- **Events are hints; the database is the truth.** A WebSocket session therefore:
  1. subscribes to the hub,
  2. sends a snapshot of the current state read from Postgres,
  3. forwards live events.
  Subscribing *before* the snapshot leaves no gap — a racing event is delivered twice at worst,
  and events are ordered by `attempts`.
- **Every way of missing events ends the stream explicitly.** A dropped slow subscriber, a lost
  Redis connection (the hub drops *all* subscribers, then resubscribes) and API shutdown all close
  the socket with **1013 Try Again Later**; the client reconnects and gets a fresh snapshot. A
  client never sits on a silently incomplete stream.
- The upgraded connection clears the HTTP server's read/write deadlines (the API's 30 s
  `WriteTimeout` would otherwise cut every stream) and pings every 30 s.

## Alternatives considered
- **Postgres `LISTEN/NOTIFY`:** no extra infrastructure, but each API replica needs a dedicated
  connection, payloads are limited to 8 KB, and NOTIFY inside the hot delivery transaction adds
  lock contention on the notify queue at our message rate.
- **Redis Streams / Kafka:** replayable, so reconnecting clients could resume from an offset. That
  needs retention and per-client cursors; the snapshot-on-connect design gets the same correctness
  with far less machinery at this scale.
- **Per-notification channels:** fewer wasted messages per API process, but a batch watcher would
  need 1000 subscriptions; a single channel with in-process filtering is simpler and cheap.

## Consequences
- + Real-time updates across any number of worker and API replicas, with an explicit contract
  for loss (1013 → resnapshot) instead of an implicit one.
- + Verified end to end: a stream outlived the 30 s server timeout; restarting Redis closed open
  streams with 1013, the hub resubscribed within 1 s and reconnecting clients saw the full flow.
- − Every API process receives every event (≈3 per notification); at very high volume, shard the
  channel (e.g. by batch ID hash) or move to Streams.
- − Events published while an API process is disconnected from Redis are lost for its clients;
  they recover through the snapshot on reconnect, not through replay.
