# ADR-003: Distributed per-channel rate limiting

- Status: Accepted
- Date: 2026-11-18

## Context
Requirement: at most 100 messages/second per channel. Workers may run as multiple replicas,
so an in-process limiter (`x/time/rate`) would allow 100 × replicas.

## Decision
A token bucket per channel stored in Redis (`ratelimit:{channel}`), refilled at 100 tokens/s with
a burst of 100, implemented as an atomic Lua script returning either "allowed" or the wait time
until the next token. Workers acquire a token right before calling the provider and sleep for the
returned wait time when denied. The rate and burst are configurable per channel.

Each channel has its own worker pool, so a throttled or failing channel never blocks another.

## Consequences
- + Correct limit across any number of worker replicas; burst traffic is smoothed, not dropped.
- − Redis becomes a dependency on the hot path; if Redis is unavailable workers back off
  (fail closed) rather than exceeding the provider limit.
