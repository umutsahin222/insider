//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
)

// eventsOf lists the outbox events of one notification as "status/attempts[/error]".
func eventsOf(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	rows, err := testStore.pool.Query(context.Background(),
		`SELECT status::text, attempts, error FROM status_event_outbox WHERE notification_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			status   string
			attempts int
			errMsg   *string
		)
		if err := rows.Scan(&status, &attempts, &errMsg); err != nil {
			t.Fatal(err)
		}
		e := fmt.Sprintf("%s/%d", status, attempts)
		if errMsg != nil {
			e += "/" + *errMsg
		}
		out = append(out, e)
	}
	return out
}

func expectEvents(t *testing.T, id uuid.UUID, want ...string) {
	t.Helper()
	if got := eventsOf(t, id); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("events:\n got  %v\n want %v", got, want)
	}
}

func failedAttempt(msg string) domain.DeliveryAttempt {
	return domain.DeliveryAttempt{Outcome: domain.OutcomeRetryableError, Error: &msg}
}

// TestOutbox_RecordsEveryStatusChange walks one notification through every transition the
// store makes and checks the event each one records, in the same statement (ADR-012).
func TestOutbox_RecordsEveryStatusChange(t *testing.T) {
	resetQueue(t)
	ctx := context.Background()
	key := "outbox-" + uuid.NewString()
	n, _, err := testStore.CreateNotification(ctx, newNotification(t, func(in *domain.CreateInput) { in.IdempotencyKey = key }))
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := testStore.CreateNotification(ctx, newNotification(t, func(in *domain.CreateInput) { in.IdempotencyKey = key })); err != nil || created {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}
	expectEvents(t, n.ID, "pending/0") // the idempotent replay records nothing

	c := claimOne(t, time.Minute)
	if err := testStore.ScheduleRetry(ctx, c, 0, failedAttempt("provider responded 503")); err != nil {
		t.Fatal(err)
	}
	c = claimOne(t, time.Minute)
	if err := testStore.Release(ctx, c, 0); err != nil { // breaker open: the attempt is handed back
		t.Fatal(err)
	}
	c = claimOne(t, time.Minute)
	if err := testStore.MarkFailed(ctx, c, failedAttempt("provider responded 400")); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.RetryNotification(ctx, n.ID, 1); err != nil {
		t.Fatal(err)
	}
	c = claimOne(t, time.Minute)
	if err := testStore.MarkSent(ctx, c, nil, domain.DeliveryAttempt{Outcome: domain.OutcomeSuccess}); err != nil {
		t.Fatal(err)
	}

	// An event mirrors the row after the change, including the last error so far.
	expectEvents(t, n.ID,
		"pending/0",
		"processing/1",
		"pending/1/provider responded 503",
		"processing/2/provider responded 503",
		"pending/1/provider responded 503", // released: attempt handed back
		"processing/2/provider responded 503",
		"failed/2/provider responded 400",
		"pending/2/provider responded 400", // manual retry
		"processing/3/provider responded 400",
		"sent/3",
	)
}

func TestOutbox_NoEventWhenTheLeaseWasLost(t *testing.T) {
	resetQueue(t)
	ctx := context.Background()
	n := insertSMS(t, 1, nil)[0]
	stale := claimOne(t, time.Millisecond) // a worker that stalls past its lease
	time.Sleep(20 * time.Millisecond)
	if _, err := testStore.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if fresh := claimOne(t, time.Minute); fresh.Attempts != 2 {
		t.Fatalf("fresh claim attempts = %d, want 2", fresh.Attempts)
	}

	// The stale worker's outcome matches nothing: a newer attempt owns the notification.
	if err := testStore.MarkSent(ctx, stale, nil, domain.DeliveryAttempt{Outcome: domain.OutcomeSuccess}); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("stale MarkSent: err = %v, want ErrLeaseLost", err)
	}
	const lease = "lease expired before the attempt completed"
	expectEvents(t, n.ID, "pending/0", "processing/1", "pending/1/"+lease, "processing/2/"+lease)
}

func TestOutbox_CancelBatchAndReaper(t *testing.T) {
	resetQueue(t)
	ctx := context.Background()
	items := insertSMS(t, 3, nil)
	claimed := claimOne(t, time.Millisecond) // one is in flight when the batch is cancelled

	cancelled, err := testStore.CancelBatch(ctx, *items[0].BatchID)
	if err != nil || len(cancelled) != 2 {
		t.Fatalf("cancelled %d, err %v; want the two pending items", len(cancelled), err)
	}
	for _, n := range cancelled {
		expectEvents(t, n.ID, "pending/0", "cancelled/0")
	}

	time.Sleep(20 * time.Millisecond) // the in-flight claim's lease expires
	if _, err := testStore.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	expectEvents(t, claimed.ID, "pending/0", "processing/1", "pending/1/lease expired before the attempt completed")
}

func TestOutboxRelay_LeadershipAndDrain(t *testing.T) {
	ctx := context.Background()
	a, b := testStore.NewOutboxRelay(), testStore.NewOutboxRelay()
	t.Cleanup(func() { a.Resign(); b.Resign() })

	if _, err := b.Pending(ctx, 10); err == nil {
		t.Fatal("Pending must fail while not leading")
	}
	if ok, err := a.TryLead(ctx); err != nil || !ok {
		t.Fatalf("a.TryLead = %v, %v; want leadership", ok, err)
	}
	if ok, err := b.TryLead(ctx); err != nil || ok {
		t.Fatalf("b.TryLead = %v, %v; want refused while a leads", ok, err)
	}

	n := insertSMS(t, 1, nil)[0]
	var mine []int64
	for {
		evs, err := a.Pending(ctx, 500)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 0 {
			break
		}
		seqs := make([]int64, len(evs))
		for i, e := range evs {
			seqs[i] = e.Seq
			if i > 0 && e.Seq <= evs[i-1].Seq {
				t.Fatalf("events out of order: %d after %d", e.Seq, evs[i-1].Seq)
			}
			if e.Event.NotificationID == n.ID {
				mine = append(mine, e.Seq)
				if e.Event.Status != domain.StatusPending || e.Event.BatchID == nil || *e.Event.BatchID != *n.BatchID || e.Event.OccurredAt.IsZero() {
					t.Errorf("event = %+v, want the created notification's pending event", e.Event)
				}
			}
		}
		if err := a.Remove(ctx, seqs); err != nil {
			t.Fatal(err)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("found %d events for the new notification, want 1", len(mine))
	}
	if backlog, err := testStore.OutboxBacklog(ctx); err != nil || backlog.Pending != 0 || backlog.OldestAge != 0 {
		t.Errorf("backlog after draining = %+v, %v; want empty", backlog, err)
	}

	// The leader's session dies (crash, failover): the lock goes with it, and the old
	// leader can no longer read or remove.
	var killed bool
	if err := testStore.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, a.conn.PgConn().PID()).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminating the leader session: %v, %v", killed, err)
	}
	insertSMS(t, 1, nil)
	if _, err := a.Pending(ctx, 10); err == nil {
		t.Error("the old leader read the outbox after its session died")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok, err := b.TryLead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("b never took over after a's session died")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if backlog, err := testStore.OutboxBacklog(ctx); err != nil || backlog.Pending != 1 {
		t.Errorf("backlog = %+v, %v; want the new notification's event", backlog, err)
	}
}
