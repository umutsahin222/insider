package domain

import (
	"time"

	"github.com/google/uuid"
)

// StatusEvent announces that a notification reached a new status. Events are published
// best-effort (Redis pub/sub) and streamed to WebSocket clients; consumers treat them as
// hints and the database as the truth. Attempts orders events of one notification.
type StatusEvent struct {
	NotificationID uuid.UUID
	BatchID        *uuid.UUID
	Status         Status
	Attempts       int
	Error          *string
	OccurredAt     time.Time
}

// EventOf describes n as it is now (e.g. a row returned by an UPDATE … RETURNING).
func EventOf(n Notification, at time.Time) StatusEvent {
	return StatusEvent{
		NotificationID: n.ID, BatchID: n.BatchID, Status: n.Status,
		Attempts: n.Attempts, Error: n.LastError, OccurredAt: at,
	}
}

// Transitioned describes n after it moved to status, with the error that caused it (if any).
func (n Notification) Transitioned(status Status, errMsg *string, at time.Time) StatusEvent {
	return StatusEvent{
		NotificationID: n.ID, BatchID: n.BatchID, Status: status,
		Attempts: n.Attempts, Error: errMsg, OccurredAt: at,
	}
}
