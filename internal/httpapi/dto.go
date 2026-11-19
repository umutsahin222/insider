package httpapi

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
)

// Wire types mirror api/openapi.yaml. They are kept separate from domain types so the
// public contract can evolve independently of the internal model.

type createNotificationRequest struct {
	Channel     string              `json:"channel"`
	Recipient   string              `json:"recipient"`
	Content     string              `json:"content"`
	Priority    string              `json:"priority"`
	ScheduledAt *time.Time          `json:"scheduled_at"`
	Template    *templateRefRequest `json:"template"`
}

func (r createNotificationRequest) input(idempotencyKey, correlationID string) domain.CreateInput {
	in := domain.CreateInput{
		Channel:        r.Channel,
		Recipient:      r.Recipient,
		Content:        r.Content,
		Priority:       r.Priority,
		ScheduledAt:    r.ScheduledAt,
		IdempotencyKey: idempotencyKey,
		CorrelationID:  correlationID,
	}
	if r.Template != nil {
		in.Template = &domain.TemplateRef{Name: r.Template.Name, Variables: r.Template.Variables}
	}
	return in
}

type createBatchItem struct {
	createNotificationRequest
	IdempotencyKey string `json:"idempotency_key"`
}

type createBatchRequest struct {
	Items []createBatchItem `json:"items"`
}

type batchCreatedResponse struct {
	BatchID         uuid.UUID   `json:"batch_id"`
	Count           int         `json:"count"`
	NotificationIDs []uuid.UUID `json:"notification_ids"`
}

func toBatchCreatedResponse(b domain.Batch) batchCreatedResponse {
	ids := make([]uuid.UUID, len(b.Notifications))
	for i, n := range b.Notifications {
		ids[i] = n.ID
	}
	return batchCreatedResponse{BatchID: b.ID, Count: len(ids), NotificationIDs: ids}
}

type notificationResponse struct {
	ID                uuid.UUID  `json:"id"`
	BatchID           *uuid.UUID `json:"batch_id"`
	IdempotencyKey    *string    `json:"idempotency_key"`
	Channel           string     `json:"channel"`
	Recipient         string     `json:"recipient"`
	Content           string     `json:"content"`
	TemplateID        *uuid.UUID `json:"template_id"`
	Priority          string     `json:"priority"`
	Status            string     `json:"status"`
	ScheduledAt       *time.Time `json:"scheduled_at"`
	NextAttemptAt     time.Time  `json:"next_attempt_at"`
	Attempts          int        `json:"attempts"`
	MaxAttempts       int        `json:"max_attempts"`
	LastError         *string    `json:"last_error"`
	ProviderMessageID *string    `json:"provider_message_id"`
	CorrelationID     string     `json:"correlation_id"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	SentAt            *time.Time `json:"sent_at"`
}

func toNotificationResponse(n domain.Notification) notificationResponse {
	return notificationResponse{
		ID:                n.ID,
		BatchID:           n.BatchID,
		IdempotencyKey:    n.IdempotencyKey,
		Channel:           string(n.Channel),
		Recipient:         n.Recipient,
		Content:           n.Content,
		TemplateID:        n.TemplateID,
		Priority:          n.Priority.String(),
		Status:            string(n.Status),
		ScheduledAt:       utcPtr(n.ScheduledAt),
		NextAttemptAt:     n.NextAttemptAt.UTC(),
		Attempts:          n.Attempts,
		MaxAttempts:       n.MaxAttempts,
		LastError:         n.LastError,
		ProviderMessageID: n.ProviderMessageID,
		CorrelationID:     n.CorrelationID,
		CreatedAt:         n.CreatedAt.UTC(),
		UpdatedAt:         n.UpdatedAt.UTC(),
		SentAt:            utcPtr(n.SentAt),
	}
}

type deliveryAttemptResponse struct {
	Attempt    int       `json:"attempt"`
	Outcome    string    `json:"outcome"`
	StatusCode *int      `json:"status_code"`
	Error      *string   `json:"error"`
	DurationMs int64     `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

type notificationDetailResponse struct {
	notificationResponse
	DeliveryAttempts []deliveryAttemptResponse `json:"delivery_attempts"`
}

type notificationPageResponse struct {
	Items      []notificationResponse `json:"items"`
	NextCursor *string                `json:"next_cursor"`
}

type batchSummaryResponse struct {
	BatchID uuid.UUID      `json:"batch_id"`
	Total   int            `json:"total"`
	Counts  map[string]int `json:"counts"`
}

func toBatchSummaryResponse(s domain.BatchSummary) batchSummaryResponse {
	// Report every status, including zero counts, so clients need no defaulting.
	counts := map[string]int{}
	for _, st := range []domain.Status{domain.StatusPending, domain.StatusProcessing, domain.StatusSent, domain.StatusFailed, domain.StatusCancelled} {
		counts[string(st)] = s.Counts[st]
	}
	return batchSummaryResponse{BatchID: s.BatchID, Total: s.Total, Counts: counts}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// Cursors are opaque to clients: base64url("<created_at unix µs>.<id>").
// Microseconds match PostgreSQL timestamp precision, so the keyset comparison is exact.

func encodeCursor(c domain.Cursor) string {
	raw := strconv.FormatInt(c.CreatedAt.UnixMicro(), 10) + "." + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (domain.Cursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return domain.Cursor{}, false
	}
	ts, id, ok := strings.Cut(string(raw), ".")
	if !ok {
		return domain.Cursor{}, false
	}
	micros, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return domain.Cursor{}, false
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return domain.Cursor{}, false
	}
	return domain.Cursor{CreatedAt: time.UnixMicro(micros).UTC(), ID: uid}, true
}
