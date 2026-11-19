//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
)

// resetQueue cancels pending rows left by other tests so claims only see this test's data.
func resetQueue(t *testing.T) {
	t.Helper()
	if _, err := testStore.pool.Exec(context.Background(),
		`UPDATE notifications SET status = 'cancelled' WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
}

func insertSMS(t *testing.T, count int, mutate func(i int, in *domain.CreateInput)) []domain.Notification {
	t.Helper()
	items := make([]domain.CreateInput, count)
	for i := range items {
		items[i] = domain.CreateInput{Channel: "sms", Recipient: "+905551234567", Content: "x"}
		if mutate != nil {
			mutate(i, &items[i])
		}
	}
	batch, err := domain.NewBatch(items, "", domain.DefaultRules(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := testStore.CreateBatch(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	return batch.Notifications
}

func TestRetryNotification(t *testing.T) {
	resetQueue(t)
	ctx := context.Background()
	n := insertSMS(t, 1, func(_ int, in *domain.CreateInput) {})[0]

	if _, err := testStore.RetryNotification(ctx, n.ID, 5); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("retry of pending: err = %v, want ErrConflict", err)
	}

	// Exhaust the attempts: 5 claims, each failing.
	for i := 1; i <= 5; i++ {
		c := claimOne(t, time.Minute)
		msg := "boom"
		a := domain.DeliveryAttempt{NotificationID: c.ID, Attempt: c.Attempts, Outcome: domain.OutcomeRetryableError, Error: &msg}
		var err error
		if i < 5 {
			err = testStore.ScheduleRetry(ctx, c, 0, a)
		} else {
			err = testStore.MarkFailed(ctx, c, a)
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	retried, err := testStore.RetryNotification(ctx, n.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != domain.StatusPending || retried.Attempts != 5 || retried.MaxAttempts != 10 {
		t.Fatalf("retried = %s %d/%d, want pending 5/10", retried.Status, retried.Attempts, retried.MaxAttempts)
	}

	// The next attempt is number 6: the fencing token keeps rising and the audit trail grows.
	c := claimOne(t, time.Minute)
	if c.ID != n.ID || c.Attempts != 6 {
		t.Fatalf("claimed %s attempt %d, want %s attempt 6", c.ID, c.Attempts, n.ID)
	}
	if err := testStore.MarkSent(ctx, c, nil, domain.DeliveryAttempt{NotificationID: c.ID, Attempt: 6, Outcome: domain.OutcomeSuccess}); err != nil {
		t.Fatal(err)
	}
	attempts, err := testStore.ListAttempts(ctx, n.ID)
	if err != nil || len(attempts) != 6 || attempts[5].Outcome != domain.OutcomeSuccess {
		t.Errorf("attempts = %d (err %v), want 6 with the last one successful", len(attempts), err)
	}

	if _, err := testStore.RetryNotification(ctx, n.ID, 5); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("retry of sent: err = %v, want ErrConflict", err)
	}
	if _, err := testStore.RetryNotification(ctx, uuid.New(), 5); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("retry of unknown id: err = %v, want ErrNotFound", err)
	}
}

func claimOne(t *testing.T, lease time.Duration) domain.Notification {
	t.Helper()
	got, err := testStore.Claim(context.Background(), domain.ChannelSMS, 1, lease)
	if err != nil || len(got) != 1 {
		t.Fatalf("claim: %d rows, err %v", len(got), err)
	}
	return got[0]
}

func attemptRow(outcome domain.AttemptOutcome, code int) domain.DeliveryAttempt {
	msg := "boom"
	return domain.DeliveryAttempt{Outcome: outcome, StatusCode: &code, Error: &msg, Duration: 42 * time.Millisecond}
}

func TestClaim_ConcurrentWorkersNeverShareRows(t *testing.T) {
	resetQueue(t)
	const total = 300
	insertSMS(t, total, nil)

	var (
		mu      sync.Mutex
		claimed = map[uuid.UUID]int{}
		wg      sync.WaitGroup
	)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ns, err := testStore.Claim(context.Background(), domain.ChannelSMS, 7, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(ns) == 0 {
					return
				}
				mu.Lock()
				for _, n := range ns {
					claimed[n.ID]++
					if n.Status != domain.StatusProcessing || n.Attempts != 1 || n.LockedUntil == nil {
						t.Errorf("claimed row in wrong state: %+v", n)
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != total {
		t.Errorf("claimed %d distinct rows, want %d", len(claimed), total)
	}
	for id, c := range claimed {
		if c != 1 {
			t.Errorf("%s claimed %d times", id, c)
		}
	}
}

func TestClaim_PriorityAndSchedule(t *testing.T) {
	resetQueue(t)
	future := time.Now().Add(time.Hour)
	ns := insertSMS(t, 4, func(i int, in *domain.CreateInput) {
		in.Priority = []string{"low", "normal", "high", "high"}[i]
		if i == 3 {
			in.ScheduledAt = &future
		}
	})

	got, err := testStore.Claim(context.Background(), domain.ChannelSMS, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("claimed %d, want 3 (the scheduled one is not due)", len(got))
	}
	byID := map[uuid.UUID]bool{}
	for _, n := range got {
		byID[n.ID] = true
	}
	if byID[ns[3].ID] {
		t.Error("future-scheduled notification was claimed")
	}

	// With limit 1, high must win over normal and low.
	resetQueue(t)
	ns = insertSMS(t, 3, func(i int, in *domain.CreateInput) { in.Priority = []string{"low", "normal", "high"}[i] })
	for _, want := range []int{2, 1, 0} {
		if n := claimOne(t, time.Minute); n.ID != ns[want].ID {
			t.Errorf("claimed priority %s, want %s", n.Priority, ns[want].Priority)
		}
	}
}

func TestFinishAttempt_Fencing(t *testing.T) {
	resetQueue(t)
	insertSMS(t, 1, nil)
	ctx := context.Background()
	n := claimOne(t, time.Minute)

	// Simulate the lease being reaped and the row re-claimed by another worker (attempt 2).
	if _, err := testStore.pool.Exec(ctx, `UPDATE notifications SET attempts = attempts + 1 WHERE id = $1`, n.ID); err != nil {
		t.Fatal(err)
	}
	msgID := "prov-1"
	if err := testStore.MarkSent(ctx, n, &msgID, attemptRow(domain.OutcomeSuccess, 202)); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("stale MarkSent err = %v, want ErrLeaseLost", err)
	}
	current := n
	current.Attempts++
	if err := testStore.MarkSent(ctx, current, &msgID, attemptRow(domain.OutcomeSuccess, 202)); err != nil {
		t.Fatal(err)
	}
	got, _ := testStore.GetNotification(ctx, n.ID)
	if got.Status != domain.StatusSent || got.SentAt == nil || got.ProviderMessageID == nil || *got.ProviderMessageID != "prov-1" || got.LockedUntil != nil {
		t.Errorf("after MarkSent: %+v", got)
	}
	attempts, err := testStore.ListAttempts(ctx, n.ID)
	// Both provider calls are audited, including the one whose lease was lost.
	if err != nil || len(attempts) != 2 || attempts[0].Attempt != 1 || attempts[1].Duration != 42*time.Millisecond {
		t.Errorf("attempts = %+v, err %v", attempts, err)
	}
	if err := testStore.MarkSent(ctx, current, &msgID, attemptRow(domain.OutcomeSuccess, 202)); !errors.Is(err, domain.ErrLeaseLost) {
		t.Errorf("second MarkSent err = %v, want ErrLeaseLost", err)
	}
}

func TestRetryThenFail(t *testing.T) {
	resetQueue(t)
	insertSMS(t, 1, nil)
	ctx := context.Background()

	n := claimOne(t, time.Minute)
	if err := testStore.ScheduleRetry(ctx, n, 0, attemptRow(domain.OutcomeRetryableError, 503)); err != nil {
		t.Fatal(err)
	}
	got, _ := testStore.GetNotification(ctx, n.ID)
	if got.Status != domain.StatusPending || got.LastError == nil || got.LockedUntil != nil {
		t.Errorf("after retry: %+v", got)
	}

	n = claimOne(t, time.Minute)
	if n.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", n.Attempts)
	}
	if err := testStore.MarkFailed(ctx, n, attemptRow(domain.OutcomePermanentError, 400)); err != nil {
		t.Fatal(err)
	}
	got, _ = testStore.GetNotification(ctx, n.ID)
	attempts, _ := testStore.ListAttempts(ctx, n.ID)
	if got.Status != domain.StatusFailed || len(attempts) != 2 {
		t.Errorf("after fail: status %s, %d attempts", got.Status, len(attempts))
	}
}

func TestRelease_DoesNotConsumeAnAttempt(t *testing.T) {
	resetQueue(t)
	insertSMS(t, 1, nil)
	n := claimOne(t, time.Minute)
	if err := testStore.Release(context.Background(), n, 0); err != nil {
		t.Fatal(err)
	}
	if again := claimOne(t, time.Minute); again.ID != n.ID || again.Attempts != 1 {
		t.Errorf("re-claimed attempts = %d, want 1", again.Attempts)
	}
}

func TestReaper(t *testing.T) {
	resetQueue(t)
	insertSMS(t, 2, nil)
	ctx := context.Background()

	zombie := claimOne(t, time.Millisecond)
	last := claimOne(t, time.Millisecond)
	// Make the second one look like it was on its final attempt.
	if _, err := testStore.pool.Exec(ctx, `UPDATE notifications SET max_attempts = attempts WHERE id = $1`, last.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	reaped, err := testStore.ReapExpired(ctx)
	if err != nil || reaped != 2 {
		t.Fatalf("reaped %d, err %v; want 2", reaped, err)
	}
	if got, _ := testStore.GetNotification(ctx, last.ID); got.Status != domain.StatusFailed {
		t.Errorf("exhausted notification status = %s, want failed", got.Status)
	}

	// Another worker re-claims the reaped row; the zombie's late result must be rejected.
	fresh := claimOne(t, time.Minute)
	if fresh.ID != zombie.ID || fresh.Attempts != 2 {
		t.Fatalf("re-claimed %s attempts %d", fresh.ID, fresh.Attempts)
	}
	msgID := "late"
	if err := testStore.MarkSent(ctx, zombie, &msgID, attemptRow(domain.OutcomeSuccess, 202)); !errors.Is(err, domain.ErrLeaseLost) {
		t.Errorf("zombie MarkSent err = %v, want ErrLeaseLost", err)
	}
	if err := testStore.MarkSent(ctx, fresh, &msgID, attemptRow(domain.OutcomeSuccess, 202)); err != nil {
		t.Errorf("fresh MarkSent: %v", err)
	}
}
