package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func validSMS() CreateInput {
	return CreateInput{Channel: "sms", Recipient: "+905551234567", Content: "Your code is 1234"}
}

// fields returns the field paths of a validation error, failing if err is not one.
func fields(t *testing.T, err error) []string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %v", err)
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatal("ValidationError must match ErrValidation")
	}
	out := make([]string, len(ve.Errors))
	for i, fe := range ve.Errors {
		out[i] = fe.Field
	}
	return out
}

func TestNewNotification_Validation(t *testing.T) {
	future := now.Add(time.Hour)
	tooFar := now.Add(31 * 24 * time.Hour)

	tests := []struct {
		name       string
		mutate     func(*CreateInput)
		wantFields []string // nil means valid
	}{
		{"valid sms", func(*CreateInput) {}, nil},
		{"valid email", func(in *CreateInput) { in.Channel, in.Recipient = "email", "user@example.com" }, nil},
		{"valid push", func(in *CreateInput) { in.Channel, in.Recipient = "push", "fcm:abc123_-:XYZ" }, nil},
		{"unknown channel", func(in *CreateInput) { in.Channel = "fax" }, []string{"channel"}},
		{"missing channel", func(in *CreateInput) { in.Channel = "" }, []string{"channel"}},
		{"empty request names every required field", func(in *CreateInput) { *in = CreateInput{} }, []string{"channel", "recipient", "content"}},
		{"unknown channel still reports missing content", func(in *CreateInput) { in.Channel, in.Content = "fax", "" }, []string{"channel", "content"}},
		{"missing recipient", func(in *CreateInput) { in.Recipient = "  " }, []string{"recipient"}},
		{"sms without plus", func(in *CreateInput) { in.Recipient = "905551234567" }, []string{"recipient"}},
		{"sms too short", func(in *CreateInput) { in.Recipient = "+9055" }, []string{"recipient"}},
		{"sms with letters", func(in *CreateInput) { in.Recipient = "+90555ABC4567" }, []string{"recipient"}},
		{"email display name", func(in *CreateInput) { in.Channel, in.Recipient = "email", "Bob <bob@example.com>" }, []string{"recipient"}},
		{"email no tld", func(in *CreateInput) { in.Channel, in.Recipient = "email", "bob@localhost" }, []string{"recipient"}},
		{"email garbage", func(in *CreateInput) { in.Channel, in.Recipient = "email", "not-an-email" }, []string{"recipient"}},
		{"push with space", func(in *CreateInput) { in.Channel, in.Recipient = "push", "abc def" }, []string{"recipient"}},
		{"missing content", func(in *CreateInput) { in.Content = "\n\t " }, []string{"content"}},
		{"sms at limit", func(in *CreateInput) { in.Content = strings.Repeat("a", 160) }, nil},
		{"sms over limit", func(in *CreateInput) { in.Content = strings.Repeat("a", 161) }, []string{"content"}},
		{"limit counts runes not bytes", func(in *CreateInput) { in.Content = strings.Repeat("ş", 160) }, nil},
		{"push over limit", func(in *CreateInput) { in.Channel, in.Recipient, in.Content = "push", "tok", strings.Repeat("a", 257) }, []string{"content"}},
		{"invalid utf8", func(in *CreateInput) { in.Content = "hi \xff" }, []string{"content"}},
		{"bad priority", func(in *CreateInput) { in.Priority = "urgent" }, []string{"priority"}},
		{"schedule in future", func(in *CreateInput) { in.ScheduledAt = &future }, nil},
		{"schedule too far", func(in *CreateInput) { in.ScheduledAt = &tooFar }, []string{"scheduled_at"}},
		{"idempotency key too long", func(in *CreateInput) { in.IdempotencyKey = strings.Repeat("k", 256) }, []string{"idempotency_key"}},
		{
			"reports every problem at once",
			func(in *CreateInput) { in.Recipient, in.Content, in.Priority = "x", "", "nope" },
			[]string{"recipient", "content", "priority"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validSMS()
			tt.mutate(&in)
			_, err := NewNotification(in, DefaultRules(), now)
			if tt.wantFields == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			got := fields(t, err)
			if strings.Join(got, ",") != strings.Join(tt.wantFields, ",") {
				t.Errorf("fields = %v, want %v", got, tt.wantFields)
			}
		})
	}
}

func TestNewNotification_Defaults(t *testing.T) {
	in := validSMS()
	in.IdempotencyKey = "order-42"
	in.CorrelationID = "corr-1"

	n, err := NewNotification(in, DefaultRules(), now)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID.Version() != 7 {
		t.Errorf("ID version = %d, want 7", n.ID.Version())
	}
	if n.Status != StatusPending {
		t.Errorf("Status = %s, want pending", n.Status)
	}
	if n.Priority != PriorityNormal {
		t.Errorf("Priority = %s, want normal", n.Priority)
	}
	if !n.NextAttemptAt.Equal(now) {
		t.Errorf("NextAttemptAt = %v, want now", n.NextAttemptAt)
	}
	if n.MaxAttempts != 5 || n.Attempts != 0 {
		t.Errorf("attempts = %d/%d, want 0/5", n.Attempts, n.MaxAttempts)
	}
	if n.IdempotencyKey == nil || *n.IdempotencyKey != "order-42" {
		t.Errorf("IdempotencyKey = %v", n.IdempotencyKey)
	}
	if n.CorrelationID != "corr-1" || n.BatchID != nil {
		t.Errorf("CorrelationID = %q, BatchID = %v", n.CorrelationID, n.BatchID)
	}
}

func TestNewNotification_Scheduling(t *testing.T) {
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	tests := []struct {
		name          string
		scheduledAt   *time.Time
		wantNextAttem time.Time
	}{
		{"unscheduled sends now", nil, now},
		{"future schedule delays first attempt", &future, future},
		{"past schedule sends now", &past, now},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validSMS()
			in.ScheduledAt = tt.scheduledAt
			n, err := NewNotification(in, DefaultRules(), now)
			if err != nil {
				t.Fatal(err)
			}
			if !n.NextAttemptAt.Equal(tt.wantNextAttem) {
				t.Errorf("NextAttemptAt = %v, want %v", n.NextAttemptAt, tt.wantNextAttem)
			}
		})
	}
}

func TestNewBatch(t *testing.T) {
	rules := DefaultRules()
	rules.MaxBatchSize = 3

	t.Run("valid batch shares one batch id", func(t *testing.T) {
		items := []CreateInput{validSMS(), validSMS()}
		b, err := NewBatch(items, "", rules, now)
		if err != nil {
			t.Fatal(err)
		}
		batchID, ns := b.ID, b.Notifications
		if b.IdempotencyKey != nil || b.RequestHash == "" {
			t.Errorf("key = %v, hash = %q", b.IdempotencyKey, b.RequestHash)
		}
		if len(ns) != 2 {
			t.Fatalf("len = %d, want 2", len(ns))
		}
		if ns[0].ID == ns[1].ID {
			t.Error("notifications must have distinct IDs")
		}
		for _, n := range ns {
			if n.BatchID == nil || *n.BatchID != batchID {
				t.Errorf("BatchID = %v, want %v", n.BatchID, batchID)
			}
		}
	})

	tests := []struct {
		name       string
		items      []CreateInput
		key        string
		wantFields []string
	}{
		{"empty", nil, "", []string{"items"}},
		{"too many", []CreateInput{validSMS(), validSMS(), validSMS(), validSMS()}, "", []string{"items"}},
		{"batch key too long", []CreateInput{validSMS()}, strings.Repeat("k", 256), []string{"idempotency_key"}},
		{
			"errors are indexed",
			[]CreateInput{validSMS(), {Channel: "sms", Recipient: "bad", Content: "x"}, {Channel: "fax", Recipient: "x", Content: "x"}},
			"",
			[]string{"items[1].recipient", "items[2].channel"},
		},
		{
			"duplicate idempotency keys",
			[]CreateInput{
				{Channel: "sms", Recipient: "+905551234567", Content: "a", IdempotencyKey: "k"},
				{Channel: "sms", Recipient: "+905551234567", Content: "b", IdempotencyKey: "k"},
			},
			"",
			[]string{"items[1].idempotency_key"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := NewBatch(tt.items, tt.key, rules, now)
			if b.Notifications != nil {
				t.Error("an invalid batch must not return notifications")
			}
			got := fields(t, err)
			if strings.Join(got, ",") != strings.Join(tt.wantFields, ",") {
				t.Errorf("fields = %v, want %v", got, tt.wantFields)
			}
		})
	}
}
