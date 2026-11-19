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
	hub := NewHub(rdb, discard)
	go hub.Run(ctx)

	batch := uuid.New()
	sub := hub.Subscribe(func(e domain.StatusEvent) bool { return e.BatchID != nil && *e.BatchID == batch }, 16)
	defer sub.Close()

	// Wait until the hub's Redis subscription is live: PUBLISH returns the receiver count.
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := rdb.PubSubNumSub(ctx, Channel).Result()
		if err == nil && n[Channel] > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hub never subscribed")
		}
		time.Sleep(20 * time.Millisecond)
	}

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
