package domain

import (
	"time"

	"github.com/google/uuid"
)

// Cursor is a keyset position in the (created_at DESC, id DESC) ordering.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ListFilter selects a page of notifications, newest first.
type ListFilter struct {
	Status  *Status
	Channel *Channel
	BatchID *uuid.UUID
	From    *time.Time // inclusive, on created_at
	To      *time.Time // exclusive, on created_at
	Limit   int
	After   *Cursor // nil for the first page
}

// Page is one page of a listing. Next is nil on the last page.
type Page struct {
	Items []Notification
	Next  *Cursor
}

// BatchSummary aggregates the status of a batch's notifications.
type BatchSummary struct {
	BatchID uuid.UUID
	Total   int
	Counts  map[Status]int
}

// QueueDepth counts the unfinished notifications of one channel and priority.
type QueueDepth struct {
	Channel  Channel
	Priority Priority
	// Ready notifications are pending and due: they would be claimed by an idle worker now.
	Ready int
	// Delayed notifications are pending but not yet due (scheduled, or backing off a retry).
	Delayed int
	// Processing notifications are claimed by a worker.
	Processing int
	// OldestReadyAge is how long the oldest ready notification has been due ("queue lag");
	// zero when nothing is ready.
	OldestReadyAge time.Duration
}

// Percentiles summarises a latency distribution.
type Percentiles struct {
	P50, P95, P99 time.Duration
}

// DeliveryStats summarises one channel's delivery activity over a time window.
type DeliveryStats struct {
	Channel Channel
	// Provider calls in the window, by outcome.
	Attempts        int
	Succeeded       int
	RetryableErrors int
	PermanentErrors int
	// ProviderLatency is the duration of a single provider call.
	ProviderLatency Percentiles
	// Notifications that reached a final state in the window.
	Sent   int
	Failed int
	// EndToEndLatency runs from when a sent notification became due (created, or its
	// scheduled time) until it was sent, including queueing and retries.
	EndToEndLatency Percentiles
}

// SuccessRate is the share of provider calls that succeeded; ok is false without calls.
func (s DeliveryStats) SuccessRate() (rate float64, ok bool) {
	if s.Attempts == 0 {
		return 0, false
	}
	return float64(s.Succeeded) / float64(s.Attempts), true
}
