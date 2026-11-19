package events

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/umutsahin222/insider/internal/domain"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func event(id uuid.UUID, status domain.Status) domain.StatusEvent {
	return domain.StatusEvent{NotificationID: id, Status: status, Attempts: 1, OccurredAt: time.Now()}
}

func forID(id uuid.UUID) func(domain.StatusEvent) bool {
	return func(e domain.StatusEvent) bool { return e.NotificationID == id }
}

func TestMarshalRoundTrip(t *testing.T) {
	batch, msg := uuid.New(), "provider responded 503"
	in := domain.StatusEvent{NotificationID: uuid.New(), BatchID: &batch, Status: domain.StatusPending,
		Attempts: 3, Error: &msg, OccurredAt: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	b, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.NotificationID != in.NotificationID || *out.BatchID != batch || out.Status != in.Status ||
		out.Attempts != 3 || *out.Error != msg || !out.OccurredAt.Equal(in.OccurredAt) {
		t.Errorf("round trip = %+v", out)
	}
	if _, err := Unmarshal([]byte(`{"status":"sent"}`)); err == nil {
		t.Error("an event without notification_id must be rejected")
	}
}

func TestHub_DispatchesOnlyMatchingEvents(t *testing.T) {
	h := NewHub(nil, discard)
	a, b := uuid.New(), uuid.New()
	subA := h.Subscribe(forID(a), 4)
	defer subA.Close()

	h.Dispatch(event(b, domain.StatusSent))
	h.Dispatch(event(a, domain.StatusProcessing))

	select {
	case e := <-subA.Events():
		if e.NotificationID != a || e.Status != domain.StatusProcessing {
			t.Errorf("got %+v", e)
		}
	default:
		t.Fatal("matching event not delivered")
	}
	select {
	case e := <-subA.Events():
		t.Errorf("unexpected event %+v", e)
	default:
	}
}

func TestHub_SlowSubscriberIsDroppedNotWaitedFor(t *testing.T) {
	h := NewHub(nil, discard)
	id := uuid.New()
	slow := h.Subscribe(forID(id), 1)
	fast := h.Subscribe(forID(id), 10)
	defer fast.Close()

	done := make(chan struct{})
	go func() {
		for range 3 {
			h.Dispatch(event(id, domain.StatusProcessing))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Dispatch blocked on a slow subscriber")
	}

	// The slow one got its buffered event, then its channel was closed.
	n := 0
	for range slow.Events() {
		n++
	}
	if n != 1 {
		t.Errorf("slow subscriber received %d events before being dropped, want 1", n)
	}
	if len(fast.Events()) != 3 {
		t.Errorf("fast subscriber has %d events, want 3", len(fast.Events()))
	}
	if h.Subscribers() != 1 {
		t.Errorf("subscribers = %d, want 1", h.Subscribers())
	}
	slow.Close() // already dropped: must not panic on double close
}

func TestHub_DropAllClosesEverySubscription(t *testing.T) {
	h := NewHub(nil, discard)
	s1, s2 := h.Subscribe(forID(uuid.New()), 1), h.Subscribe(forID(uuid.New()), 1)
	h.DropAll()
	for _, s := range []*Subscription{s1, s2} {
		if _, open := <-s.Events(); open {
			t.Error("subscription still open after dropAll")
		}
	}
	if h.Subscribers() != 0 {
		t.Errorf("subscribers = %d", h.Subscribers())
	}
}

// A Redis that accepts connections and never answers: every send hangs until its timeout.
func stalledRedis(t *testing.T) *redis.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	rdb := redis.NewClient(&redis.Options{Addr: ln.Addr().String(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestPublisher_NeverBlocksTheCaller(t *testing.T) {
	pub := NewPublisher(stalledRedis(t), discard)

	start := time.Now()
	for range 2 * publishQueueSize { // more than the queue holds: the excess is dropped
		pub.Publish(context.Background(), event(uuid.New(), domain.StatusSent))
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("publishing took %s with a stalled Redis; Publish must only enqueue", d)
	}
	if pub.dropped.Load() == 0 {
		t.Error("events beyond the queue size must be dropped, not block")
	}

	// Close cannot flush into a stalled Redis; it gives up when its context ends.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := pub.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close = %v, want deadline exceeded", err)
	}
	pub.Publish(context.Background(), event(uuid.New(), domain.StatusSent)) // after Close: discarded, no panic
}
