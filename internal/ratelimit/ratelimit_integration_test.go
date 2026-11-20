//go:build integration

package ratelimit

import (
	"context"
	"errors"
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

// With burst 1, consecutive slots are at least one interval apart however fast they are
// requested, which is what caps every second at the rate. Slot times are read from the
// schedule stored in Redis (on the Redis clock), so the check is exact.
func TestReserve_SlotsAreSpacedByTheInterval(t *testing.T) {
	l := New(rdb, 100, 1) // 10.1 ms slots: 10 ms plus the jitter allowance
	ctx := context.Background()
	key := t.Name()
	var slots []int64 // slot start, Redis clock, microseconds
	for i := range 150 {
		booked, wait, err := l.Reserve(ctx, key, time.Minute)
		if err != nil || !booked {
			t.Fatalf("booked=%v err=%v", booked, err)
		}
		if i == 0 && wait != 0 {
			t.Errorf("first slot after idle starts in %v, want at once", wait)
		}
		tat, err := rdb.Get(ctx, "ratelimit:"+key).Int64()
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, tat-l.interval)
	}
	for i := 1; i < len(slots); i++ {
		if gap := slots[i] - slots[i-1]; gap != l.interval {
			t.Fatalf("gap between slots %d and %d = %d µs, want %d", i-1, i, gap, l.interval)
		}
	}
	inFirstSecond := 0
	for _, s := range slots {
		if s-slots[0] < time.Second.Microseconds() {
			inFirstSecond++
		}
	}
	if inFirstSecond > 100 {
		t.Errorf("%d slots start within one second, want at most 100", inFirstSecond)
	}
	// 101 slots span more than a second by the jitter allowance, so a send that reaches the
	// provider up to 10 ms late cannot put 101 requests inside one second there.
	if span := time.Duration(slots[100]-slots[0]) * time.Microsecond; span < time.Second+jitterAllowance {
		t.Errorf("101 slots span %v, want at least %v", span, time.Second+jitterAllowance)
	}
}

// A slot further away than the caller can wait is not booked, so it stays available.
func TestReserve_TooFarIsNotBooked(t *testing.T) {
	l := New(rdb, 10, 1) // 100 ms slots
	ctx := context.Background()
	key := t.Name()
	if booked, _, _ := l.Reserve(ctx, key, 0); !booked {
		t.Fatal("first slot must be free")
	}
	for range 3 { // refused attempts must not push the next slot further out
		booked, wait, err := l.Reserve(ctx, key, 10*time.Millisecond)
		if err != nil || booked || wait <= 10*time.Millisecond {
			t.Fatalf("booked=%v wait=%v err=%v, want refused with ~100 ms to wait", booked, wait, err)
		}
	}
	booked, wait, err := l.Reserve(ctx, key, time.Second)
	if err != nil || !booked || wait > 100*time.Millisecond {
		t.Errorf("booked=%v wait=%v err=%v, want the second slot (<= 100 ms away)", booked, wait, err)
	}
}

func TestWait_SlotPastTheDeadlineFailsAtOnce(t *testing.T) {
	l := New(rdb, 1, 1) // 1 s slots
	key := t.Name()
	if err := l.Wait(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := l.Wait(ctx, key)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 50*time.Millisecond {
		t.Errorf("err = %v after %v; want DeadlineExceeded without waiting", err, time.Since(start))
	}
}

// Several "replicas" (separate limiters, many goroutines) share one key via Redis; the total
// admitted in a window must respect rate·window + burst, and with reservations nearly all of
// that capacity is used.
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
	minimum := int64(float64(max) * 0.9)
	if got := admitted.Load(); got > max || got < minimum {
		t.Errorf("admitted %d in %v, want between %d and %d", got, window, minimum, max)
	}
}
