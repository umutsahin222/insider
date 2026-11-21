//go:build integration

package events

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/umutsahin222/insider/internal/domain"
)

var rdb *redis.Client

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		log.Printf("starting redis: %v", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	url, err := ctr.ConnectionString(ctx)
	if err != nil {
		log.Print(err)
		return 1
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		log.Print(err)
		return 1
	}
	opts.ContextTimeoutEnabled = true // as in cmd/api and cmd/worker
	rdb = redis.NewClient(opts)
	defer func() { _ = rdb.Close() }()
	return m.Run()
}

// startHub runs a hub until the test ends and waits until it is positioned on the stream.
func startHub(t *testing.T, m HubMetrics) (*Hub, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hub := NewHub(rdb.Options(), discard, m)
	done := make(chan struct{})
	go func() { hub.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; _ = hub.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		hub.mu.Lock()
		since := hub.since
		hub.mu.Unlock()
		if since != "" {
			return hub, cancel, done
		}
		if time.Now().After(deadline) {
			t.Fatal("hub did not position itself on the stream")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func xadd(t *testing.T, ev domain.StatusEvent) string {
	t.Helper()
	b, err := Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	id, err := rdb.XAdd(context.Background(), &redis.XAddArgs{Stream: Stream, Values: []any{"event", b}}).Result()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func receive(t *testing.T, sub *Subscription, n int) []domain.StatusEvent {
	t.Helper()
	var got []domain.StatusEvent
	timeout := time.After(10 * time.Second)
	for len(got) < n {
		select {
		case e, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription closed after %d of %d events", len(got), n)
			}
			got = append(got, e)
		case <-timeout:
			t.Fatalf("received %d of %d events", len(got), n)
		}
	}
	return got
}

// TestRelayToHub runs the real path: a relay (as in a worker) drains an outbox into the
// stream, and a hub (as in an API process) reads it, connected only through Redis.
func TestRelayToHub(t *testing.T) {
	hub, _, _ := startHub(t, nil)
	batch := uuid.New()
	sub := hub.Subscribe(func(e domain.StatusEvent) bool { return e.BatchID != nil && *e.BatchID == batch }, 16)
	defer sub.Close()

	other := uuid.New()
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	o := &fakeOutbox{lead: true, pending: []domain.OutboxEvent{
		{Seq: 10, Event: domain.StatusEvent{NotificationID: ids[0], BatchID: &batch, Status: domain.StatusProcessing, Attempts: 1, OccurredAt: time.Now()}},
		{Seq: 11, Event: domain.StatusEvent{NotificationID: uuid.New(), BatchID: &other, Status: domain.StatusSent, Attempts: 1, OccurredAt: time.Now()}},
		{Seq: 12, Event: domain.StatusEvent{NotificationID: ids[1], BatchID: &batch, Status: domain.StatusSent, Attempts: 1, OccurredAt: time.Now()}},
	}}
	m := &recordingRelayMetrics{}
	relay := NewRelay(o, rdb, RelayConfig{MaxLen: 1000, PollInterval: 10 * time.Millisecond, BatchSize: 100}, m, discard)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { relay.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	got := receive(t, sub, 2)
	if got[0].NotificationID != ids[0] || got[1].NotificationID != ids[1] {
		t.Errorf("got %v, %v; want the batch's events in outbox order", got[0].NotificationID, got[1].NotificationID)
	}
	if got[0].StreamID == "" || !StreamIDAfter(got[1].StreamID, got[0].StreamID) {
		t.Errorf("stream IDs %q, %q: want increasing event IDs", got[0].StreamID, got[1].StreamID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		o.mu.Lock()
		removed := o.removed
		o.mu.Unlock()
		if len(removed) == 1 && len(removed[0]) == 3 && removed[0][0] == 10 && removed[0][2] == 12 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("removed = %v, want [10 11 12] once published", removed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHub_ResumesAfterLosingRedisConnection kills the hub's connection on the Redis side, as
// a network failure would. Events are kept in the stream, so subscribers stay connected and
// nothing published while the hub was away is missed.
func TestHub_ResumesAfterLosingRedisConnection(t *testing.T) {
	m := &recordingHubMetrics{}
	hub, _, _ := startHub(t, m)
	sub := hub.Subscribe(func(domain.StatusEvent) bool { return true }, 64)
	defer sub.Close()

	first := uuid.New()
	xadd(t, event(first, domain.StatusProcessing))
	if got := receive(t, sub, 1); got[0].NotificationID != first {
		t.Fatalf("got %v, want the first event", got[0].NotificationID)
	}

	if _, err := rdb.ClientKillByFilter(context.Background(), "TYPE", "normal").Result(); err != nil {
		t.Fatalf("killing connections: %v", err)
	}
	missed := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for _, id := range missed {
		xadd(t, event(id, domain.StatusSent))
	}

	got := receive(t, sub, len(missed))
	for i, e := range got {
		if e.NotificationID != missed[i] {
			t.Errorf("event %d = %v, want %v", i, e.NotificationID, missed[i])
		}
	}
	if len(m.dropped) != 0 {
		t.Errorf("dropped = %v: a lost connection must not end subscriptions", m.dropped)
	}
}

// TestHub_ResyncsWhenTheStreamWasTrimmedPastItsPosition: events the hub never read are gone,
// so it cannot vouch for its subscribers any more and ends their streams.
func TestHub_ResyncsWhenTheStreamWasTrimmedPastItsPosition(t *testing.T) {
	m := &recordingHubMetrics{}
	hub, _, _ := startHub(t, m)
	sub := hub.Subscribe(func(domain.StatusEvent) bool { return true }, 64)
	xadd(t, event(uuid.New(), domain.StatusProcessing))
	receive(t, sub, 1)

	ctx := context.Background()
	if err := rdb.XTrimMaxLen(ctx, Stream, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.ClientKillByFilter(ctx, "TYPE", "normal").Result(); err != nil {
		t.Fatal(err)
	}

	select {
	case _, ok := <-sub.Events():
		if ok {
			t.Fatal("got an event; want the subscription ended")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("subscription not ended after the stream lost the hub's position")
	}
	m.mu.Lock()
	dropped := m.dropped
	m.mu.Unlock()
	if len(dropped) != 1 || dropped[0] != DropResync {
		t.Errorf("dropped = %v, want [resync]", dropped)
	}

	after := hub.Subscribe(func(domain.StatusEvent) bool { return true }, 64)
	defer after.Close()
	id := uuid.New()
	xadd(t, event(id, domain.StatusSent))
	if got := receive(t, after, 1); got[0].NotificationID != id {
		t.Errorf("after resync got %v, want %v", got[0].NotificationID, id)
	}
}

func TestHub_Replay(t *testing.T) {
	hub, _, _ := startHub(t, nil)
	before := xadd(t, event(uuid.New(), domain.StatusSent)) // after the hub positioned: in its history
	sub := hub.Subscribe(func(domain.StatusEvent) bool { return true }, 64)
	defer sub.Close()

	mine := uuid.New()
	ids := []string{
		xadd(t, event(mine, domain.StatusProcessing)),
		xadd(t, event(uuid.New(), domain.StatusSent)),
		xadd(t, event(mine, domain.StatusSent)),
	}
	receive(t, sub, 3) // the hub has read them

	ctx := context.Background()
	got, err := hub.Replay(ctx, before, forID(mine))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].StreamID != ids[0] || got[1].StreamID != ids[2] || got[1].Status != domain.StatusSent {
		t.Errorf("replay = %+v, want my two events with their IDs", got)
	}
	if got, err := hub.Replay(ctx, ids[2], forID(mine)); err != nil || len(got) != 0 {
		t.Errorf("replay after the last event = %v, %v; want none", got, err)
	}

	// Before the hub's own history: the hub cannot vouch for it.
	if _, err := hub.Replay(ctx, "1-0", forID(mine)); !errors.Is(err, ErrReplayUnavailable) {
		t.Errorf("replay from before the hub started: err = %v, want ErrReplayUnavailable", err)
	}
	// Trimmed: entries right after it may be gone.
	if err := rdb.XDel(ctx, Stream, ids[0]).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Replay(ctx, ids[0], forID(mine)); !errors.Is(err, ErrReplayUnavailable) {
		t.Errorf("replay from a trimmed entry: err = %v, want ErrReplayUnavailable", err)
	}
}

// TestHub_RunStopsPromptlyOnCancel: on shutdown the hub must let go of Redis and drop its
// subscribers quickly, so WebSocket clients get their 1013 close and reconnect elsewhere.
func TestHub_RunStopsPromptlyOnCancel(t *testing.T) {
	m := &recordingHubMetrics{}
	hub, cancel, done := startHub(t, m)
	sub := hub.Subscribe(func(domain.StatusEvent) bool { return true }, 16)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2 s of cancellation")
	}
	if _, ok := <-sub.Events(); ok {
		t.Error("subscription still open after Run returned")
	}
	if len(m.dropped) != 1 || m.dropped[0] != DropShutdown {
		t.Errorf("dropped = %v, want [shutdown]", m.dropped)
	}
}
