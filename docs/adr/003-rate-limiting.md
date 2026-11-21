# ADR-003: Distributed per-channel rate limiting

- Status: Accepted (revised 2026-11-20: token bucket replaced by GCRA slot reservation;
  revised 2026-11-21: provider-side margin widened to 50 ms and made configurable)
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
- **The interval includes a margin:** `(1 s + RATE_LIMIT_MARGIN) / rate`, by default
  `(1 s + 50 ms) / 100` = 10.5 ms. Slots are exact on the Redis clock, but each send reaches the
  provider a little after its slot (timer wake-up, scheduling, network), and that delay varies.
  With slots exactly 10 ms apart, slot 0 and slot 100 are exactly 1 s apart, so a slightly late
  slot 0 already shows the provider 101 requests within one second. With a margin of *m*, delays
  that vary by up to *m* stay within the limit at the provider. The cost is
  *m* / (1 s + *m*) of throughput: 4.8 % (95/s) at 50 ms.
  *Revised 2026-11-21:* the margin was 10 ms (99/s). On the same stack and code where it had
  always measured exactly 100, a later session measured 101–102 in seven of eight channel runs,
  i.e. delays there varied by more than 20 ms; the previous commit's build showed the same, so
  the host, not a code change, had become noisier. 100 had been the limit's edge all along. The
  requirement is a hard maximum and throughput is not, so the default now tolerates 50 ms;
  a dedicated host with tighter timing can lower `RATE_LIMIT_MARGIN`.
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
| 1000 SMS + 1000 email at once, 1 worker, 10 ms margin (2026-11-20) | 100 SMS, 100 email | 99–100 per channel; 1000 first attempts in 10.5 s |
| 300 SMS, 3 worker replicas, 10 ms margin (2026-11-20) | 100 | |
| Same 1000 + 1000, 10 ms margin, noisier host (2026-11-21) | 101–102 in 7 of 8 channel runs | |
| Same 1000 + 1000, quiet host, 4 runs per margin (2026-11-21) | 10 ms: 100 · 25 ms: 98–99 · 50 ms: 96 | |

The first measurement of this design used database timestamps written *after* each provider call
and showed 103. Those timestamps include the mock's random 20–40 ms latency, so this was a
measurement error, but it led to the first margin. `scripts/smoke.sh` and the acceptance suite
check the provider-side peak on every CI run, with one worker and with three.

Each channel has its own worker pool and its own key, so a throttled or failing channel never
blocks another.

## Consequences
- + The exact limit holds across any number of worker replicas, and bursts are smoothed into
  evenly spaced sends instead of being dropped.
- + Most of the limit is used (95/s rather than ~90/s with the token bucket): a burst of 1000
  first attempts needs 1000 slots of 10.5 ms, about 10.5 s.
- − 5 % of the provider's allowance is kept as a safety margin by default.
- − A slot booked by a caller that then gives up (shutdown, cancelled context) goes unused. The
  cost is one slot (10.5 ms) per abandoned call.
- − Redis is a dependency on the hot path; if it is unavailable, delivery pauses rather than
  exceeding the provider limit.
