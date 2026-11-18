package domain

import (
	"time"

	"github.com/google/uuid"
)

// Notification is a single message to deliver over one channel.
// The same row is the unit of work in the queue (see ADR-001).
type Notification struct {
	ID             uuid.UUID
	BatchID        *uuid.UUID
	IdempotencyKey *string
	Channel        Channel
	Recipient      string
	Content        string
	TemplateID     *uuid.UUID
	Priority       Priority
	Status         Status

	ScheduledAt *time.Time
	// NextAttemptAt is when the notification becomes claimable. Stores persist it using the
	// database clock (max(scheduled_at, now())); the value set here is the API's estimate.
	NextAttemptAt time.Time
	Attempts      int
	MaxAttempts   int
	LockedUntil   *time.Time

	LastError         *string
	ProviderMessageID *string
	CorrelationID     string
	// TraceContext carries W3C trace headers (traceparent/tracestate) from the API to the worker.
	TraceContext map[string]string

	CreatedAt time.Time
	UpdatedAt time.Time
	SentAt    *time.Time
}

// AttemptOutcome classifies a single delivery attempt (see ADR-002).
type AttemptOutcome string

const (
	OutcomeSuccess        AttemptOutcome = "success"
	OutcomeRetryableError AttemptOutcome = "retryable_error"
	OutcomePermanentError AttemptOutcome = "permanent_error"
)

// DeliveryAttempt is the audit record of one call to the provider.
type DeliveryAttempt struct {
	ID             int64
	NotificationID uuid.UUID
	Attempt        int
	Outcome        AttemptOutcome
	StatusCode     *int
	Error          *string
	Duration       time.Duration
	CreatedAt      time.Time
}

// ProviderReply is the external provider's answer to one send request. Interpreting it
// (success / retryable / permanent) is a delivery-policy decision, not the client's.
type ProviderReply struct {
	StatusCode int
	MessageID  string
	// RetryAfter is the provider's requested delay (Retry-After header), zero if absent.
	RetryAfter time.Duration
}

// Template is a reusable message body with text/template placeholders.
type Template struct {
	ID        uuid.UUID
	Name      string
	Channel   Channel
	Body      string
	CreatedAt time.Time
}

// CreateInput is the raw, unvalidated request to create a notification.
// Content must already be rendered if a template was used.
type CreateInput struct {
	Channel        string
	Recipient      string
	Content        string
	Priority       string
	ScheduledAt    *time.Time
	IdempotencyKey string
	TemplateID     *uuid.UUID
	CorrelationID  string
}

// NewNotification validates in and builds a pending notification ready to be enqueued.
// A scheduled time in the past is accepted and means "send as soon as possible".
func NewNotification(in CreateInput, rules Rules, now time.Time) (Notification, error) {
	ve := &ValidationError{}
	n := build(in, rules, now, "", ve)
	if err := ve.errOrNil(); err != nil {
		return Notification{}, err
	}
	return n, nil
}

// NewBatch validates every item and builds the notifications of one batch.
// It is all-or-nothing: any invalid item rejects the whole batch, and every
// problem is reported with its item index (e.g. "items[3].recipient").
func NewBatch(items []CreateInput, rules Rules, now time.Time) (uuid.UUID, []Notification, error) {
	ve := &ValidationError{}
	switch {
	case len(items) == 0:
		ve.add("items", "must contain at least one notification")
	case len(items) > rules.MaxBatchSize:
		ve.add("items", "must contain at most "+itoa(rules.MaxBatchSize)+" notifications")
	}
	if err := ve.errOrNil(); err != nil {
		return uuid.Nil, nil, err
	}

	batchID := newID()
	out := make([]Notification, 0, len(items))
	seenKeys := make(map[string]int, len(items))
	for i, in := range items {
		prefix := "items[" + itoa(i) + "]."
		n := build(in, rules, now, prefix, ve)
		if in.IdempotencyKey != "" {
			if first, dup := seenKeys[in.IdempotencyKey]; dup {
				ve.add(prefix+"idempotency_key", "duplicates items["+itoa(first)+"].idempotency_key")
			} else {
				seenKeys[in.IdempotencyKey] = i
			}
		}
		n.BatchID = &batchID
		out = append(out, n)
	}
	if err := ve.errOrNil(); err != nil {
		return uuid.Nil, nil, err
	}
	return batchID, out, nil
}

func build(in CreateInput, rules Rules, now time.Time, prefix string, ve *ValidationError) Notification {
	n := Notification{
		ID:            newID(),
		Recipient:     in.Recipient,
		Content:       in.Content,
		TemplateID:    in.TemplateID,
		Status:        StatusPending,
		NextAttemptAt: now,
		MaxAttempts:   rules.MaxAttempts,
		CorrelationID: in.CorrelationID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	ch, err := ParseChannel(in.Channel)
	if err != nil {
		ve.add(prefix+"channel", "must be one of sms, email, push")
	} else {
		n.Channel = ch
		rules.validateRecipient(ch, in.Recipient, prefix, ve)
		rules.validateContent(ch, in.Content, prefix, ve)
	}

	if p, err := ParsePriority(in.Priority); err != nil {
		ve.add(prefix+"priority", "must be one of high, normal, low")
	} else {
		n.Priority = p
	}

	if in.ScheduledAt != nil {
		at := in.ScheduledAt.UTC()
		if at.After(now.Add(rules.MaxScheduleAhead)) {
			ve.add(prefix+"scheduled_at", "must be at most "+rules.MaxScheduleAhead.String()+" in the future")
		}
		n.ScheduledAt = &at
		if at.After(now) {
			n.NextAttemptAt = at
		}
	}

	if in.IdempotencyKey != "" {
		if len(in.IdempotencyKey) > rules.MaxIdempotencyKeyLength {
			ve.add(prefix+"idempotency_key", "must be at most "+itoa(rules.MaxIdempotencyKeyLength)+" bytes")
		}
		key := in.IdempotencyKey
		n.IdempotencyKey = &key
	}
	return n
}

// newID returns a time-ordered UUIDv7, which keeps B-tree inserts append-mostly.
func newID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New() // only fails if the random source fails
	}
	return id
}
