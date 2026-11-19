package delivery

import (
	"time"

	"github.com/sony/gobreaker/v2"
)

// BreakerPolicy decides when a channel's circuit breaker opens (see ADR-002).
//
// Only retryable failures (timeouts, 5xx, 429) count; a 4xx is the request's fault, not the
// provider's. The breaker opens on either signal:
//   - a failure ratio of at least FailureRatio over at least MinRequests calls within Window:
//     a provider that is mostly failing, regardless of how failures interleave;
//   - ConsecutiveFailures failures in a row: a hard outage, caught even at low traffic
//     where MinRequests is never reached.
//
// Counting consecutive failures alone is not enough: against a provider that fails 30% of
// the time at random, a streak of 5 occurs a few times per thousand calls, and each trip
// would pause a working channel for OpenTimeout.
type BreakerPolicy struct {
	ConsecutiveFailures uint32
	FailureRatio        float64
	MinRequests         uint32
	Window              time.Duration // counts reset at this interval while closed
	OpenTimeout         time.Duration // how long the breaker stays open before probing
}

func (p BreakerPolicy) shouldTrip(c gobreaker.Counts) bool {
	if c.ConsecutiveFailures >= p.ConsecutiveFailures {
		return true
	}
	return c.Requests >= p.MinRequests && float64(c.TotalFailures)/float64(c.Requests) >= p.FailureRatio
}
