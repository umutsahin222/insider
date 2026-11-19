//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

// truncateAll empties every table. Stats are global aggregates, so their tests need a
// known starting point; tests in this package run sequentially.
func truncateAll(t *testing.T) {
	t.Helper()
	if _, err := testStore.pool.Exec(context.Background(),
		`TRUNCATE delivery_attempts, notifications, batches, templates CASCADE`); err != nil {
		t.Fatal(err)
	}
}

func insertBatch(t *testing.T, items ...domain.CreateInput) []domain.Notification {
	t.Helper()
	b, err := domain.NewBatch(items, "", domain.DefaultRules(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := testStore.CreateBatch(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	return b.Notifications
}

func claimEmail(t *testing.T) domain.Notification {
	t.Helper()
	ns, err := testStore.Claim(context.Background(), domain.ChannelEmail, 1, time.Minute)
	if err != nil || len(ns) != 1 {
		t.Fatalf("claim: n=%d err=%v", len(ns), err)
	}
	return ns[0]
}

func attempt(n domain.Notification, outcome domain.AttemptOutcome, d time.Duration) domain.DeliveryAttempt {
	a := domain.DeliveryAttempt{NotificationID: n.ID, Attempt: n.Attempts, Outcome: outcome, Duration: d}
	if outcome != domain.OutcomeSuccess {
		msg := "boom"
		a.Error = &msg
	}
	return a
}

func TestQueueDepths(t *testing.T) {
	truncateAll(t)
	ctx := context.Background()
	later := time.Now().Add(time.Hour)
	sms := func(p string) domain.CreateInput {
		return domain.CreateInput{Channel: "sms", Recipient: "+905551234567", Content: "x", Priority: p}
	}
	email := domain.CreateInput{Channel: "email", Recipient: "a@example.com", Content: "x"}
	scheduled := sms("low")
	scheduled.ScheduledAt = &later
	ns := insertBatch(t, sms("high"), sms("high"), scheduled, email, email, email)

	// One sms/high row has been due for 10s; one email row is claimed by a worker.
	if _, err := testStore.pool.Exec(ctx,
		`UPDATE notifications SET next_attempt_at = now() - interval '10 seconds' WHERE id = $1`, ns[0].ID); err != nil {
		t.Fatal(err)
	}
	claimEmail(t)

	depths, err := testStore.QueueDepths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]domain.QueueDepth{}
	for _, d := range depths {
		got[string(d.Channel)+"/"+d.Priority.String()] = d
	}
	if len(got) != 3 {
		t.Fatalf("depths = %+v, want 3 channel/priority groups", depths)
	}
	if d := got["sms/high"]; d.Ready != 2 || d.Delayed != 0 || d.Processing != 0 ||
		d.OldestReadyAge < 9*time.Second || d.OldestReadyAge > time.Minute {
		t.Errorf("sms/high = %+v, want 2 ready, oldest ≈10s", d)
	}
	if d := got["sms/low"]; d.Ready != 0 || d.Delayed != 1 || d.OldestReadyAge != 0 {
		t.Errorf("sms/low = %+v, want 1 delayed", d)
	}
	if d := got["email/normal"]; d.Ready != 2 || d.Processing != 1 {
		t.Errorf("email/normal = %+v, want 2 ready + 1 processing", d)
	}
}

func TestDeliveryStats(t *testing.T) {
	truncateAll(t)
	ctx := context.Background()
	email := domain.CreateInput{Channel: "email", Recipient: "a@example.com", Content: "x"}
	insertBatch(t, email, email, email, email)

	sent := claimEmail(t)
	if err := testStore.MarkSent(ctx, sent, nil, attempt(sent, domain.OutcomeSuccess, 100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	retried := claimEmail(t)
	if err := testStore.ScheduleRetry(ctx, retried, time.Hour, attempt(retried, domain.OutcomeRetryableError, 300*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	failed := claimEmail(t)
	if err := testStore.MarkFailed(ctx, failed, attempt(failed, domain.OutcomePermanentError, 50*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	old := claimEmail(t)
	if err := testStore.MarkSent(ctx, old, nil, attempt(old, domain.OutcomeSuccess, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	// Push the last delivery out of the window.
	if _, err := testStore.pool.Exec(ctx, `UPDATE delivery_attempts SET created_at = now() - interval '2 hours' WHERE notification_id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.pool.Exec(ctx, `UPDATE notifications SET sent_at = now() - interval '2 hours' WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}

	stats, err := testStore.DeliveryStats(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Channel != domain.ChannelEmail {
		t.Fatalf("stats = %+v, want only email", stats)
	}
	st := stats[0]
	if st.Attempts != 3 || st.Succeeded != 1 || st.RetryableErrors != 1 || st.PermanentErrors != 1 {
		t.Errorf("attempt counts = %+v", st)
	}
	if st.Sent != 1 || st.Failed != 1 {
		t.Errorf("sent=%d failed=%d, want 1 and 1", st.Sent, st.Failed)
	}
	// Median of {50, 100, 300} ms; the 5 s call is outside the window.
	if st.ProviderLatency.P50 != 100*time.Millisecond || st.ProviderLatency.P99 > 300*time.Millisecond {
		t.Errorf("provider latency = %+v", st.ProviderLatency)
	}
	if st.EndToEndLatency.P50 <= 0 || st.EndToEndLatency.P50 > time.Minute {
		t.Errorf("end-to-end latency = %+v", st.EndToEndLatency)
	}

	empty, err := testStore.DeliveryStats(ctx, time.Millisecond)
	if err != nil || len(empty) != 0 {
		// A 1 ms window can still catch rows written in the last millisecond; none are.
		t.Errorf("tiny window: stats=%+v err=%v", empty, err)
	}
}
