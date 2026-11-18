// Package ratelimit implements a distributed token bucket on Redis (ADR-003).
package ratelimit

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucket refills `rate` tokens per second up to `burst` and takes one token if available.
// It uses the Redis server clock (TIME), so all worker replicas share one notion of time.
// Returns {allowed (1|0), wait_ms until a token is available}.
var tokenBucket = redis.NewScript(`
local key   = KEYS[1]
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])

local t   = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local state  = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts     = tonumber(state[2])
if tokens == nil then
  tokens = burst
  ts = now
end

tokens = math.min(burst, tokens + math.max(0, now - ts) * rate / 1000)

local allowed = 0
local wait = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  wait = math.ceil((1 - tokens) * 1000 / rate)
end

redis.call('HSET', key, 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', key, math.ceil(burst * 1000 / rate) + 1000)
return {allowed, wait}
`)

// Limiter is a per-key token bucket shared by every process using the same Redis.
type Limiter struct {
	rdb   redis.Scripter
	rate  float64
	burst int
}

// New returns a limiter allowing rate events per second with the given burst.
func New(rdb redis.Scripter, rate float64, burst int) *Limiter {
	return &Limiter{rdb: rdb, rate: rate, burst: burst}
}

// Allow tries to take a token for key. If denied, it returns how long until one is available.
func (l *Limiter) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	res, err := tokenBucket.Run(ctx, l.rdb, []string{"ratelimit:" + key}, l.rate, l.burst).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("running token bucket: %w", err)
	}
	return res[0] == 1, time.Duration(res[1]) * time.Millisecond, nil
}

// Wait blocks until a token for key is taken or ctx is done. Redis errors are returned
// immediately: the caller decides how to back off (fail closed, see ADR-003).
func (l *Limiter) Wait(ctx context.Context, key string) error {
	for {
		ok, wait, err := l.Allow(ctx, key)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		// Small jitter so waiters on many replicas don't retry in lockstep.
		wait += time.Duration(rand.Int64N(int64(2 * time.Millisecond)))
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
