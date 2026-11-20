//go:build integration

package events

import (
	"context"
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
	rdb = redis.NewClient(opts)
	defer func() { _ = rdb.Close() }()
	return m.Run()
}

// TestPublishToHub runs the real path: a publisher (as in a worker) and a hub (as in an
// API process) connected only through Redis.
func TestPublishToHub(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitSubscribers(t, ctx, 0)
	hub := NewHub(rdb, discard)
	go hub.Run(ctx)

	batch := uuid.New()
	sub := hub.Subscribe(func(e domain.StatusEvent) bool { return e.BatchID != nil && *e.BatchID == batch }, 16)
	defer sub.Close()

	waitSubscribers(t, ctx, 1)

	pub := NewPublisher(rdb, discard)
	t.Cleanup(func() { _ = pub.Close(context.Background()) })
	other := uuid.New()
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	pub.Publish(ctx,
		domain.StatusEvent{NotificationID: ids[0], BatchID: &batch, Status: domain.StatusProcessing, Attempts: 1, OccurredAt: time.Now()},
		domain.StatusEvent{NotificationID: uuid.New(), BatchID: &other, Status: domain.StatusSent, Attempts: 1, OccurredAt: time.Now()},
		domain.StatusEvent{NotificationID: ids[1], BatchID: &batch, Status: domain.StatusSent, Attempts: 1, OccurredAt: time.Now()},
	)

	var got []domain.StatusEvent
	timeout := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case e := <-sub.Events():
			got = append(got, e)
		case <-timeout:
			t.Fatalf("received %d of 2 events", len(got))
		}
	}
	if got[0].NotificationID != ids[0] || got[1].NotificationID != ids[1] || got[1].Status != domain.StatusSent {
		t.Errorf("events = %+v, want the two batch events in publish order", got)
	}
}

// waitSubscribers waits until exactly want connections are subscribed to the event channel.
// Tests wait for 0 before starting a hub, so a previous test's hub that is still shutting
// down is never mistaken for (or killed instead of) the one under test.
func waitSubscribers(t *testing.T, ctx context.Context, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, err := rdb.PubSubNumSub(ctx, Channel).Result()
		if err == nil && n[Channel] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscribers = %v (err %v), want %d", n[Channel], err, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHub_ResubscribesAfterLosingRedis kills the hub's pub/sub connection on the Redis side,
// as a Redis restart or a network failure would. Every local subscriber must be told (its
// channel closes, so a WebSocket client reconnects and gets a fresh snapshot instead of
// silently missing events), and the hub must subscribe again and keep delivering.
func TestHub_ResubscribesAfterLosingRedis(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitSubscribers(t, ctx, 0)
	hub := NewHub(rdb, discard)
	go hub.Run(ctx)
	waitSubscribers(t, ctx, 1)

	before := hub.Subscribe(func(domain.StatusEvent) bool { return true }, 16)
	if n, err := rdb.ClientKillByFilter(ctx, "TYPE", "pubsub").Result(); err != nil || n != 1 {
		t.Fatalf("killing the hub's pub/sub connection: killed %d, err %v", n, err)
	}
	select {
	case _, ok := <-before.Events():
		if ok {
			t.Fatal("got an event; want the subscription closed after the connection was lost")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("subscriber was not dropped after the connection was lost")
	}

	waitSubscribers(t, ctx, 1) // the hub subscribed again on a new connection
	after := hub.Subscribe(func(domain.StatusEvent) bool { return true }, 16)
	defer after.Close()
	pub := NewPublisher(rdb, discard)
	t.Cleanup(func() { _ = pub.Close(context.Background()) })
	id := uuid.New()
	pub.Publish(ctx, domain.StatusEvent{NotificationID: id, Status: domain.StatusSent, Attempts: 1, OccurredAt: time.Now()})
	select {
	case e, ok := <-after.Events():
		if !ok || e.NotificationID != id {
			t.Fatalf("event = %+v (open %v), want the one published after reconnecting", e, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event after the hub resubscribed")
	}
}

// TestHub_RunStopsPromptlyOnCancel: on shutdown the hub must let go of Redis and drop its
// subscribers at once, so WebSocket clients get their 1013 close and reconnect elsewhere.
// go-redis reads pub/sub messages without watching the context, so this needs Run's help.
func TestHub_RunStopsPromptlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitSubscribers(t, ctx, 0)
	hub := NewHub(rdb, discard)
	done := make(chan struct{})
	go func() { hub.Run(ctx); close(done) }()
	waitSubscribers(t, ctx, 1)
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
	waitSubscribers(t, context.Background(), 0)
}
