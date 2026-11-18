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
