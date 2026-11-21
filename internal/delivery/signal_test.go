package delivery

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestSignal_WakesEveryWaiterOnceAndIsReusable(t *testing.T) {
	s := NewSignal()
	a, b := s.C(), s.C()
	if closed(a) {
		t.Fatal("fired before Notify")
	}
	s.Notify()
	if !closed(a) || !closed(b) {
		t.Fatal("Notify must wake every waiter")
	}
	next := s.C()
	if closed(next) {
		t.Fatal("a channel taken after Notify must wait for the next one")
	}
	s.Notify()
	if !closed(next) {
		t.Fatal("second Notify did not fire")
	}
}

func TestSignal_NotifyBetweenTakeAndWaitIsNotMissed(t *testing.T) {
	s := NewSignal()
	ch := s.C() // a loop takes the channel, then looks for work and finds none...
	s.Notify()  // ...while work arrives and is announced...
	if !closed(ch) {
		t.Fatal("...so the wait that follows must return at once")
	}
}

func TestSignal_NilNeverFires(t *testing.T) {
	var s *Signal
	s.Notify()
	if s.C() != nil {
		t.Fatal("nil signal must return a nil (never ready) channel")
	}
}

// countingQueue counts claims so the test knows when every loop has found the queue empty.
type countingQueue struct {
	fakeQueue
	claims atomic.Int64
}

func (q *countingQueue) Claim(ctx context.Context, ch domain.Channel, limit int, lease time.Duration) ([]domain.Notification, error) {
	defer q.claims.Add(1)
	return q.fakeQueue.Claim(ctx, ch, limit, lease)
}

func (q *countingQueue) add(n domain.Notification) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, n)
}

func TestWorker_WakeClaimsAtOnceInsteadOfWaitingForThePoll(t *testing.T) {
	cfg := testConfig()
	cfg.PollInterval = time.Hour // only a wake-up can make an idle loop claim again in time
	q := &countingQueue{}
	wake := NewSignal()
	w := NewWorker(cfg, Deps{Queue: q, Sender: &scriptedSender{reply: domain.ProviderReply{StatusCode: 202}},
		Limiter: allow, Logger: testLogger(), Wake: wake})

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { w.Run(ctx) })
	defer func() { cancel(); wg.Wait() }()

	// Every loop claims once, finds nothing and goes idle.
	waitUntil(t, time.Second, func() bool { return q.claims.Load() >= int64(cfg.Concurrency) })
	q.add(claimed(1, 5))
	wake.Notify()

	waitUntil(t, 2*time.Second, func() bool { return len(q.ops()) == 1 })
	if ops := q.ops(); ops[0] != "sent" {
		t.Errorf("ops = %v, want [sent]", ops)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
