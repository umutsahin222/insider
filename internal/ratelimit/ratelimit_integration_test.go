//go:build integration

package ratelimit

import (
	"context"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
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

func TestAllow_BurstThenDeny(t *testing.T) {
	l := New(rdb, 10, 3)
	ctx := context.Background()
	key := t.Name()
	for i := range 3 {
		if ok, _, err := l.Allow(ctx, key); err != nil || !ok {
			t.Fatalf("token %d: ok=%v err=%v", i, ok, err)
		}
	}
	ok, wait, err := l.Allow(ctx, key)
	if err != nil || ok {
		t.Fatalf("4th token: ok=%v err=%v, want denied", ok, err)
	}
	if wait <= 0 || wait > 100*time.Millisecond {
		t.Errorf("wait = %v, want (0, 100ms] at 10/s", wait)
	}
}

func TestAllow_KeysAreIndependent(t *testing.T) {
	l := New(rdb, 1, 1)
	ctx := context.Background()
	for _, key := range []string{t.Name() + ":sms", t.Name() + ":email"} {
		if ok, _, _ := l.Allow(ctx, key); !ok {
			t.Errorf("%s denied; channels must not share a bucket", key)
		}
	}
}

// Several "replicas" (separate limiters, many goroutines) share one bucket via Redis;
// the total admitted in a window must respect rate·window + burst.
func TestWait_DistributedThroughputIsCapped(t *testing.T) {
	const (
		rate   = 100
		burst  = 5
		window = 1500 * time.Millisecond
	)
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	key := t.Name()

	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 3 { // replicas
		l := New(rdb, rate, burst)
		for range 8 { // workers per replica
			wg.Add(1)
			go func() {
				defer wg.Done()
				for l.Wait(ctx, key) == nil {
					admitted.Add(1)
				}
			}()
		}
	}
	wg.Wait()

	max := int64(rate*window.Seconds()) + burst
	minimum := int64(float64(max) * 0.8)
	if got := admitted.Load(); got > max || got < minimum {
		t.Errorf("admitted %d in %v, want between %d and %d", got, window, minimum, max)
	}
}
