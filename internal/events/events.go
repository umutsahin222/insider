// Package events moves notification status events from the database to WebSocket clients
// (ADR-012): a Relay in the worker copies the transactional outbox to a Redis stream, and
// every API process runs one Hub that reads the stream and fans events out to its local
// subscribers.
package events

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
)

// Stream is the Redis stream every status event goes through.
const Stream = "notification.events"

// wireEvent is the JSON form, shared by the stream and WebSocket (api/openapi.yaml
// StatusEvent). In the stream, event_id is null: the entry's own ID is the event ID.
type wireEvent struct {
	EventID        *string       `json:"event_id"`
	NotificationID uuid.UUID     `json:"notification_id"`
	BatchID        *uuid.UUID    `json:"batch_id"`
	Status         domain.Status `json:"status"`
	Attempts       int           `json:"attempts"`
	Error          *string       `json:"error"`
	OccurredAt     time.Time     `json:"occurred_at"`
}

// Marshal encodes ev as JSON.
func Marshal(ev domain.StatusEvent) ([]byte, error) {
	w := wireEvent{
		NotificationID: ev.NotificationID, BatchID: ev.BatchID, Status: ev.Status,
		Attempts: ev.Attempts, Error: ev.Error, OccurredAt: ev.OccurredAt.UTC(),
	}
	if ev.StreamID != "" {
		w.EventID = &ev.StreamID
	}
	return json.Marshal(w)
}

// Unmarshal decodes an event produced by Marshal.
func Unmarshal(b []byte) (domain.StatusEvent, error) {
	var w wireEvent
	if err := json.Unmarshal(b, &w); err != nil {
		return domain.StatusEvent{}, err
	}
	if w.NotificationID == uuid.Nil {
		return domain.StatusEvent{}, errors.New("event without notification_id")
	}
	ev := domain.StatusEvent{
		NotificationID: w.NotificationID, BatchID: w.BatchID, Status: w.Status,
		Attempts: w.Attempts, Error: w.Error, OccurredAt: w.OccurredAt,
	}
	if w.EventID != nil {
		ev.StreamID = *w.EventID
	}
	return ev, nil
}

// streamID is a parsed Redis stream entry ID ("<milliseconds>-<sequence>").
type streamID struct{ ms, seq uint64 }

// ParseStreamID reports whether s is a valid stream entry ID; event IDs are such IDs.
func ParseStreamID(s string) bool {
	_, ok := parseStreamID(s)
	return ok
}

func parseStreamID(s string) (streamID, bool) {
	msPart, seqPart, found := strings.Cut(s, "-")
	if !found {
		return streamID{}, false
	}
	ms, err1 := strconv.ParseUint(msPart, 10, 64)
	seq, err2 := strconv.ParseUint(seqPart, 10, 64)
	if err1 != nil || err2 != nil {
		return streamID{}, false
	}
	return streamID{ms: ms, seq: seq}, true
}

func (a streamID) less(b streamID) bool {
	return a.ms < b.ms || (a.ms == b.ms && a.seq < b.seq)
}

// StreamIDAfter reports whether stream ID a comes after b; invalid IDs never do.
func StreamIDAfter(a, b string) bool {
	x, ok1 := parseStreamID(a)
	y, ok2 := parseStreamID(b)
	return ok1 && ok2 && y.less(x)
}
