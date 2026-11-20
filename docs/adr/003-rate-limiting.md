# ADR-003: Distributed per-channel rate limiting

- Status: Accepted (revised 2026-11-20: token bucket replaced by GCRA slot reservation)
- Date: 2026-11-18

## Context
Requirement: at most 100 messages/second per channel. Workers may run as multiple replicas,
so an in-process limiter (`x/time/rate`) would allow 100 × replicas.

## Decision
**GCRA (generic cell rate algorithm) with slot reservation, one key per channel in Redis**
(`ratelimit:{channel}`), as an atomic Lua script on the Redis server clock (`TIME`, in
microseconds), so replicas never disagree about elapsed time.

- The key holds the theoretical arrival time (TAT), the earliest start of the next free slot.
  A caller books `slot = max(TAT, now)` and moves TAT to `slot + interval`. Then it sleeps until
  its slot and calls the provider.
- Consecutive slots are therefore at least one interval apart, however many workers race for
  them, so **no second ever holds more than 100 sends.** Under continuous demand slots are back
  to back, so the limit is used almost in full.
- **The interval includes a jitter allowance:** `(1 s + 10 ms) / rate`, i.e. 10.1 ms. Slots are
  exact on the Redis clock, but each send reaches the provider a little after its slot (timer
  wake-up, scheduling, network), and that delay varies. With slots exactly 10 ms apart, slot 0
  and slot 100 are exactly 1 s apart, so a slightly late slot 0 already shows the provider 101
  requests within one second. With 10.1 ms, delays varying by up to 10 ms stay within the limit
  at the provider. The cost is 1 % of throughput (99/s).
- **No polling:** one Redis call per send, and no capacity is lost to late wake-ups.
- **Deadline-aware:** the caller passes how long it can wait (its lease-derived send deadline,
  ADR-005). A slot further away is not booked at all, so it stays available to others, and the
  worker hands the row back at once (`lease_expiring`).
- `RATE_LIMIT_BURST` (default 1) lets `burst − 1` extra sends through at once after an idle
  period, by letting a slot lag behind now. Each one adds to the most a 1-second window can
  hold, so the default keeps the strict cap.
- If Redis is unreachable, workers fail closed: they hand the work back rather than send
  without a limit.

**Why not the token bucket used first.** The first version was a token bucket with burst 1:
workers polled a Lua script that returned "allowed" or the time until the next token.
- It allowed up to `rate + burst` = 101 sends in a 1-second window.
- More visibly, it sustained only about 90/s (88–93 calls per second per channel, measured with
  1000 SMS + 1000 email at once). The bucket holds a single token, so whenever a polling waiter
  took it a millisecond late, that millisecond was lost: the wait is rounded to milliseconds,
  plus wake-up jitter and the Redis round trip.

The final audit measured this, and the reservation design removes both problems.

**Measured** with the mock provider recording arrivals per channel (`GET /stats`,
`peak_per_second`), the limit as the provider sees it:

| Run | Busiest second at the provider | Provider calls per second |
|---|---|---|
| 1000 SMS + 1000 email at once, 1 worker | 100 SMS, 100 email | 99–100 per channel; 1000 first attempts in 10.5 s |
| 300 SMS, 3 worker replicas | 100 | |

The first measurement of this design used database timestamps written *after* each provider call
and showed 103. Those timestamps include the mock's random 20–40 ms latency, so this was a
measurement error, but it led to the jitter allowance above. `scripts/smoke.sh` now checks the
provider-side peak on every CI run, with one worker and with three.

Each channel has its own worker pool and its own key, so a throttled or failing channel never
blocks another.

## Consequences
- + The exact limit holds across any number of worker replicas, and bursts are smoothed into
  evenly spaced sends instead of being dropped.
- + The limit is used almost in full (99/s rather than ~90/s): a burst of 1000 drains in about
  10.5 s.
- − A slot booked by a caller that then gives up (shutdown, cancelled context) goes unused. The
  cost is one 10.1 ms slot per abandoned call.
- − Redis is a dependency on the hot path; if it is unavailable, delivery pauses rather than
  exceeding the provider limit.
