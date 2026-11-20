# ADR-003: Distributed per-channel rate limiting

- Status: Accepted
- Date: 2026-11-18

## Context
Requirement: at most 100 messages/second per channel. Workers may run as multiple replicas,
so an in-process limiter (`x/time/rate`) would allow 100 × replicas.

## Decision
A token bucket per channel stored in Redis (`ratelimit:{channel}`), refilled at 100 tokens/s,
implemented as an atomic Lua script returning either "allowed" or the wait time until the next
token. The script reads the Redis server clock (`TIME`), so replicas never disagree about elapsed
time. Workers acquire a token right before calling the provider and sleep for the returned wait
time when denied. Rate and burst are configurable (`RATE_LIMIT_PER_SECOND`, `RATE_LIMIT_BURST`).

**Burst defaults to 1.** Any 1-second window can admit at most `rate + burst` messages, so a burst
of 100 would allow ~200 in the first second after an idle period — violating the provider limit.
With burst 1 sends are evenly spaced (~10 ms apart). Measured end to end with 1000 SMS + 300 email
queued at once: max 95 SMS and 94 email in any 1-second window, both channels in parallel.

**Sustained rate is ~90/s, not 100/s.** Measured again in the final audit with 1000 SMS + 1000
email at once: 88–93 calls per second per channel during the bulk phase, busiest sliding second 94.
With burst 1 the bucket holds at most one token, so when a waiter takes it a millisecond late
(ms-rounded wait, wake-up jitter, the Redis round trip) that millisecond is not recovered. A
reservation-based limiter (GCRA: each call atomically books the next 10 ms slot and sleeps until
it) would use the full 100/s and cap every sliding second at exactly 100 instead of `rate + burst`
(101), at the cost of a different Lua script and of slots wasted by callers that give up.

Each channel has its own worker pool, so a throttled or failing channel never blocks another.

## Consequences
- + Correct limit across any number of worker replicas; burst traffic is smoothed, not dropped.
- − Redis becomes a dependency on the hot path; if Redis is unavailable workers back off
  (fail closed) rather than exceeding the provider limit.
