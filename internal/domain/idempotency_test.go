package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mustNotification(t *testing.T, in CreateInput, now time.Time) Notification {
	t.Helper()
	n, err := NewNotification(in, DefaultRules(), now)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNotificationFingerprint(t *testing.T) {
	at := now.Add(time.Hour)
	base := CreateInput{Channel: "sms", Recipient: "+905551234567", Content: "hi", Priority: "high", ScheduledAt: &at}
	baseHash := mustNotification(t, base, now).RequestHash

	t.Run("same request hashes the same regardless of metadata", func(t *testing.T) {
		in := base
		in.IdempotencyKey, in.CorrelationID = "other-key", "other-corr"
		// Different "now" and a sub-microsecond difference PostgreSQL would not store.
		later := at.Add(300 * time.Nanosecond)
		in.ScheduledAt = &later
		if got := mustNotification(t, in, now.Add(time.Minute)).RequestHash; got != baseHash {
			t.Error("fingerprint changed for an equivalent request")
		}
	})

	t.Run("normalised priority", func(t *testing.T) {
		a := mustNotification(t, CreateInput{Channel: "sms", Recipient: "+905551234567", Content: "hi"}, now)
		b := mustNotification(t, CreateInput{Channel: "sms", Recipient: "+905551234567", Content: "hi", Priority: "normal"}, now)
		if a.RequestHash != b.RequestHash {
			t.Error(`omitted priority and "normal" must hash the same`)
		}
	})

	tmpl := uuid.New()
	changes := map[string]func(*CreateInput){
		"channel":      func(in *CreateInput) { in.Channel, in.Recipient = "push", "tok" },
		"recipient":    func(in *CreateInput) { in.Recipient = "+905551234568" },
		"content":      func(in *CreateInput) { in.Content = "hi!" },
		"priority":     func(in *CreateInput) { in.Priority = "low" },
		"scheduled_at": func(in *CreateInput) { s := at.Add(time.Second); in.ScheduledAt = &s },
		"unscheduled":  func(in *CreateInput) { in.ScheduledAt = nil },
		"template":     func(in *CreateInput) { in.TemplateID = &tmpl },
	}
	for name, change := range changes {
		t.Run("differs on "+name, func(t *testing.T) {
			in := base
			change(&in)
			if mustNotification(t, in, now).RequestHash == baseHash {
				t.Error("fingerprint did not change")
			}
		})
	}

	t.Run("field boundaries are unambiguous", func(t *testing.T) {
		a := mustNotification(t, CreateInput{Channel: "email", Recipient: "a@b.co", Content: "mx"}, now)
		b := mustNotification(t, CreateInput{Channel: "email", Recipient: "a@b.com", Content: "x"}, now)
		if a.RequestHash == b.RequestHash {
			t.Error("concatenation collision")
		}
	})
}

func TestBatchFingerprint(t *testing.T) {
	rules := DefaultRules()
	a := CreateInput{Channel: "sms", Recipient: "+905551234567", Content: "a"}
	b := CreateInput{Channel: "sms", Recipient: "+905551234567", Content: "b"}
	hash := func(items ...CreateInput) string {
		t.Helper()
		batch, err := NewBatch(items, "batch-key", rules, now)
		if err != nil {
			t.Fatal(err)
		}
		return batch.RequestHash
	}

	// Two separate NewBatch calls produce different notification and batch IDs.
	first, retried := hash(a, b), hash(a, b)
	if first != retried {
		t.Error("same items must hash the same (IDs and batch ID are not part of it)")
	}
	if hash(a, b) == hash(b, a) {
		t.Error("item order must matter")
	}
	if hash(a) == hash(a, b) {
		t.Error("item count must matter")
	}
	keyed := a
	keyed.IdempotencyKey = "item-key"
	if hash(a, b) == hash(keyed, b) {
		t.Error("item idempotency keys must matter")
	}
}

func TestVerifyReplay(t *testing.T) {
	tests := []struct {
		name, stored, requested string
		wantErr                 bool
	}{
		{"same request", "h1", "h1", false},
		{"different request", "h1", "h2", true},
		{"legacy row without fingerprint", "", "h2", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyReplay(tt.stored, tt.requested)
			if got := errors.Is(err, ErrIdempotencyMismatch); got != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
