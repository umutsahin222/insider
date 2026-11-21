// Package ratelimit implements a distributed rate limiter on Redis (ADR-003): GCRA with slot
// reservation, shared by every worker replica.
package ratelimit

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// reserveSlot books the next send slot for a key (GCRA, the generic cell rate algorithm).
//
// The key holds the theoretical arrival time (TAT): the earliest moment the next slot may
// start. A caller gets slot = max(TAT, now - tolerance), and TAT moves to slot + interval, so
// consecutive slots are at least one interval apart however many callers race for them.
// Callers sleep until their slot instead of polling, so no capacity is lost to late wake-ups:
// under continuous demand slots are back to back and the full rate is used.
//
// With tolerance 0 (burst 1) no 1-second window ever holds more than rate slots; a tolerance of
// (burst-1) intervals lets that many extra requests through at once after an idle period.
// A slot more than max_wait away is not booked at all, so a caller that could not use it (its
// deadline would pass first) does not take capacity from others.
//
// Times are integer microseconds on the Redis server clock (TIME), so replicas never disagree
// about elapsed time. Returns {booked (1|0), wait_us until the slot starts}.
var reserveSlot = redis.NewScript(`
local interval  = tonumber(ARGV[1])
local tolerance = tonumber(ARGV[2])
local max_wait  = tonumber(ARGV[3])

local t   = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])

local tat  = tonumber(redis.call('GET', KEYS[1])) or 0
local slot = math.max(tat, now - tolerance)
local wait = math.max(0, slot - now)
if wait > max_wait then
  return {0, wait}
end

local next_tat = slot + interval
redis.call('SET', KEYS[1], string.format('%.0f', next_tat), 'PX', math.ceil((next_tat - now) / 1000) + 1000)
return {1, wait}
`)

// maxReservation bounds how far ahead Wait books a slot when ctx has no deadline.
const maxReservation = time.Minute

// Limiter spaces events per key across every process that shares the same Redis.
type Limiter struct {
	rdb       redis.Scripter
	interval  int64 // microseconds between slots
	tolerance int64 // microseconds a slot may lag behind now: (burst-1) intervals
}

// New returns a limiter allowing at most rate events in any second; burst > 1 lets that many
// through at once after an idle period (each extra one adds to the most a second can hold).
//
// margin stretches each second's worth of slots by that much. Slots are exact on the Redis
// clock, but a send reaches the provider a little after its slot (timer wake-up, scheduling,
// network), and that delay varies. With 100 slots exactly 10 ms apart, slot 0 and slot 100 are
// exactly 1 s apart, so a slightly late slot 0 already puts 101 requests inside one second at
// the provider. Spacing slots (1 s + margin) / rate apart keeps every second at the provider
// within the limit for delays that vary by up to margin, at the cost of margin / (1 s + margin)
// of throughput (ADR-003).
func New(rdb redis.Scripter, rate float64, burst int, margin time.Duration) *Limiter {
	interval := int64(math.Ceil(float64((time.Second + max(margin, 0)).Microseconds()) / rate))
	return &Limiter{rdb: rdb, interval: interval, tolerance: int64(max(burst-1, 0)) * interval}
}

// Reserve books the next slot for key unless it starts more than maxWait from now. It returns
// whether a slot was booked and how long until it starts (the caller must not act before).
func (l *Limiter) Reserve(ctx context.Context, key string, maxWait time.Duration) (bool, time.Duration, error) {
	res, err := reserveSlot.Run(ctx, l.rdb, []string{"ratelimit:" + key},
		l.interval, l.tolerance, max(maxWait.Microseconds(), 0)).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("reserving rate-limit slot: %w", err)
	}
	return res[0] == 1, time.Duration(res[1]) * time.Microsecond, nil
}

// Allow books a slot only if one is available right now.
func (l *Limiter) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	return l.Reserve(ctx, key, 0)
}

// Wait books the next slot for key and sleeps until it starts. If the slot would start after
// ctx's deadline, nothing is booked and the error wraps context.DeadlineExceeded at once, so
// the caller can give the work back instead of waiting in vain. Redis errors are returned
// immediately: the caller decides how to back off (fail closed, see ADR-003). A slot booked
// by a caller whose ctx is cancelled while it sleeps goes unused.
func (l *Limiter) Wait(ctx context.Context, key string) error {
	maxWait := maxReservation
	if deadline, ok := ctx.Deadline(); ok {
		maxWait = min(maxWait, time.Until(deadline))
	}
	booked, wait, err := l.Reserve(ctx, key, maxWait)
	if err != nil {
		return err
	}
	if !booked {
		return fmt.Errorf("next rate-limit slot for %s is %s away: %w", key, wait.Round(time.Millisecond), context.DeadlineExceeded)
	}
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
