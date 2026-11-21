package delivery

import (
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

// Result is what happened to a notification after one delivery attempt.
type Result string

const (
	ResultSent   Result = "sent"
	ResultRetry  Result = "retry_scheduled"
	ResultFailed Result = "failed"
	// ResultLeaseLost: the attempt was audited, but a newer attempt owns the notification.
	ResultLeaseLost Result = "lease_lost"
	// ResultStoreError: the outcome could not be recorded; the reaper will retry the row.
	ResultStoreError Result = "store_error"
)

// ReleaseReason says why claimed work was handed back without using an attempt.
type ReleaseReason string

const (
	ReleaseBreakerOpen     ReleaseReason = "circuit_open"
	ReleaseLimiterDown     ReleaseReason = "rate_limiter_unavailable"
	ReleaseShuttingDown    ReleaseReason = "shutdown"
	ReleaseBreakerHalfOpen ReleaseReason = "circuit_half_open"
	// ReleaseLeaseExpiring: the rate-limit wait ran so long that the lease could expire
	// before the send was recorded; the row goes back rather than risk a duplicate send.
	ReleaseLeaseExpiring ReleaseReason = "lease_expiring"
)

// BreakerState is the circuit breaker state, reported as a number so it can be graphed.
type BreakerState int

const (
	BreakerClosed   BreakerState = 0
	BreakerHalfOpen BreakerState = 1
	BreakerOpen     BreakerState = 2
)

// Metrics receives delivery measurements; implemented by observability.DeliveryMetrics.
// Implementations must be safe for concurrent use.
type Metrics interface {
	// AttemptFinished is called once per provider call.
	AttemptFinished(ch domain.Channel, outcome domain.AttemptOutcome, result Result, d time.Duration)
	// Delivered is called when a notification is sent, with the time since it became due
	// (created, or its scheduled time), including queueing and earlier attempts.
	Delivered(ch domain.Channel, sinceDue time.Duration)
	// RateLimitWaited reports how long a send waited for its rate-limit slot.
	RateLimitWaited(ch domain.Channel, d time.Duration)
	// Released counts claimed work handed back to the queue.
	Released(ch domain.Channel, reason ReleaseReason)
	// BreakerStateChanged reports the channel's circuit breaker state.
	BreakerStateChanged(ch domain.Channel, s BreakerState)
	// Reclaimed counts notifications whose expired lease the reaper took back.
	Reclaimed(n int)
}

// NopMetrics discards every measurement.
type NopMetrics struct{}

func (NopMetrics) AttemptFinished(domain.Channel, domain.AttemptOutcome, Result, time.Duration) {}
func (NopMetrics) Delivered(domain.Channel, time.Duration)                                      {}
func (NopMetrics) RateLimitWaited(domain.Channel, time.Duration)                                {}
func (NopMetrics) Released(domain.Channel, ReleaseReason)                                       {}
func (NopMetrics) BreakerStateChanged(domain.Channel, BreakerState)                             {}
func (NopMetrics) Reclaimed(int)                                                                {}

// dueAt is when n became deliverable: its scheduled time if that is after creation.
func dueAt(n domain.Notification) time.Time {
	if n.ScheduledAt != nil && n.ScheduledAt.After(n.CreatedAt) {
		return *n.ScheduledAt
	}
	return n.CreatedAt
}
