//go:build integration

package postgres

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

// readyRecorder collects the channels a listener reports.
type readyRecorder struct{ ch chan domain.Channel }

func startListener(t *testing.T) *readyRecorder {
	t.Helper()
	r := &readyRecorder{ch: make(chan domain.Channel, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		testStore.ListenForReadyWork(ctx, func(c domain.Channel) { r.ch <- c }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	t.Cleanup(func() { cancel(); wg.Wait() })
	r.expect(t, "catch-up after connecting", domain.ChannelSMS, domain.ChannelEmail, domain.ChannelPush)
	return r
}

// expect waits for exactly the given channels (in any order), then checks nothing else arrives.
func (r *readyRecorder) expect(t *testing.T, what string, want ...domain.Channel) {
	t.Helper()
	var got []domain.Channel
	timeout := time.After(5 * time.Second)
	for len(got) < len(want) {
		select {
		case c := <-r.ch:
			got = append(got, c)
		case <-timeout:
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
	}
	r.expectNothing(t, what)
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func (r *readyRecorder) expectNothing(t *testing.T, what string) {
	t.Helper()
	select {
	case c := <-r.ch:
		t.Fatalf("%s: unexpected wake-up for %s", what, c)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestListenForReadyWork(t *testing.T) {
	resetQueue(t)
	ctx := context.Background()
	r := startListener(t)

	if _, _, err := testStore.CreateNotification(ctx, newNotification(t, nil)); err != nil {
		t.Fatal(err)
	}
	r.expect(t, "single create", domain.ChannelSMS)

	later := time.Now().Add(time.Hour)
	if _, _, err := testStore.CreateNotification(ctx, newNotification(t, func(in *domain.CreateInput) {
		in.Channel, in.Recipient, in.ScheduledAt = "email", "a@example.com", &later
	})); err != nil {
		t.Fatal(err)
	}
	r.expectNothing(t, "scheduled create")

	items := []domain.CreateInput{
		{Channel: "push", Recipient: "tok", Content: "a"}, {Channel: "push", Recipient: "tok", Content: "b"},
		{Channel: "email", Recipient: "a@example.com", Content: "c"},
	}
	batch, err := domain.NewBatch(items, "", domain.DefaultRules(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := testStore.CreateBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	r.expect(t, "batch: once per channel", domain.ChannelPush, domain.ChannelEmail)

	c := claimOne(t, time.Minute)
	r.expectNothing(t, "claim")
	if err := testStore.ScheduleRetry(ctx, c, time.Hour, attemptRow(domain.OutcomeRetryableError, 503)); err != nil {
		t.Fatal(err)
	}
	r.expectNothing(t, "retry with backoff")

	if _, err := testStore.pool.Exec(ctx, `UPDATE notifications SET next_attempt_at = now() WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	r.expect(t, "backoff cut short", domain.ChannelSMS)
	c = claimOne(t, time.Minute)
	if err := testStore.Release(ctx, c, 0); err != nil {
		t.Fatal(err)
	}
	r.expect(t, "release, due now", domain.ChannelSMS)

	c = claimOne(t, time.Minute)
	if err := testStore.MarkFailed(ctx, c, attemptRow(domain.OutcomePermanentError, 400)); err != nil {
		t.Fatal(err)
	}
	r.expectNothing(t, "dead-lettered")
	if _, err := testStore.RetryNotification(ctx, c.ID, 1); err != nil {
		t.Fatal(err)
	}
	r.expect(t, "manual retry", domain.ChannelSMS)
}

func TestListenForReadyWork_ReconnectsAndCatchesUp(t *testing.T) {
	ctx := context.Background()
	r := startListener(t)

	// Kill the listening session from the server side, as a database failover would.
	var killed int
	if err := testStore.pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE query = 'LISTEN `+readyChannel+`' AND pid <> pg_backend_pid()) AS t`).Scan(&killed); err != nil || killed != 1 {
		t.Fatalf("terminated %d listening sessions (err %v), want 1", killed, err)
	}
	// Announcements made while it was gone are lost, so it reports every channel on reconnect.
	r.expect(t, "catch-up after reconnecting", domain.ChannelSMS, domain.ChannelEmail, domain.ChannelPush)

	if _, _, err := testStore.CreateNotification(ctx, newNotification(t, nil)); err != nil {
		t.Fatal(err)
	}
	r.expect(t, "create after reconnecting", domain.ChannelSMS)
}
