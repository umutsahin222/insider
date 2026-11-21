package domain

import (
	"time"

	"github.com/google/uuid"
)

// StatusEvent announces that a notification reached a new status. The database records one
// for every status change, in the same statement (a transactional outbox, ADR-012); a relay
// moves them to an event stream, and API processes stream them to WebSocket clients.
// Delivery is at least once: consumers may see an event twice. Attempts orders the events of
// one notification.
type StatusEvent struct {
	NotificationID uuid.UUID
	BatchID        *uuid.UUID
	Status         Status
	Attempts       int
	// Error is the notification's last error at that point (e.g. why an attempt failed).
	Error      *string
	OccurredAt time.Time
	// StreamID is the event's position in the event stream: opaque, increasing, and usable
	// to resume after it. Empty for events built from the database (snapshots).
	StreamID string
}

// EventOf describes n as it is now, e.g. for a snapshot read from the database. OccurredAt is
// when n reached its current state (updated_at, the database clock), the same instant its
// outbox event carries, so a client can order snapshot and stream events by it.
func EventOf(n Notification) StatusEvent {
	return StatusEvent{
		NotificationID: n.ID, BatchID: n.BatchID, Status: n.Status,
		Attempts: n.Attempts, Error: n.LastError, OccurredAt: n.UpdatedAt,
	}
}

// OutboxEvent is a status event waiting in the transactional outbox; Seq is its position
// there, used to remove it once relayed.
type OutboxEvent struct {
	Seq   int64
	Event StatusEvent
}

// OutboxBacklog describes the events not yet relayed.
type OutboxBacklog struct {
	Pending int
	// OldestAge is how long the oldest pending event has waited; zero when none are pending.
	OldestAge time.Duration
}
