package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/umutsahin222/insider/internal/domain"
	"github.com/umutsahin222/insider/internal/observability"
)

func TestConfigValidate(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatalf("test config rejected: %v", err)
	}
	tests := map[string]func(*Config){
		"zero concurrency":            func(c *Config) { c.Concurrency = 0 },
		"zero poll interval":          func(c *Config) { c.PollInterval = 0 },
		"zero retry base":             func(c *Config) { c.Retry.Base = 0 },
		"retry max below base":        func(c *Config) { c.Retry.Max = time.Second },
		"lease without send headroom": func(c *Config) { c.Lease = c.SendTimeout + StoreTimeout },
	}
	for name, mutate := range tests {
		c := testConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// A row whose rate-limit wait reaches the send deadline goes back to the queue instead of
// being sent after its lease may have been handed to another worker.
func TestDeliver_ReleasesWhenTheLeaseWouldExpireWhileWaiting(t *testing.T) {
	q := &fakeQueue{}
	s := &scriptedSender{reply: domain.ProviderReply{StatusCode: 202}}
	blocked := limiterFunc(func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() })
	m := &recordingMetrics{}
	w := newMeteredTestWorker(q, s, blocked, m)

	start := time.Now()
	w.deliver(context.Background(), claimed(1, 5), time.Now().Add(50*time.Millisecond))

	if time.Since(start) > 2*time.Second {
		t.Fatal("the rate-limit wait must end at the send deadline")
	}
	if ops := q.ops(); len(ops) != 1 || ops[0] != "release" || s.sent != 0 {
		t.Fatalf("ops = %v, sent = %d; want one release and no send", ops, s.sent)
	}
	if q.calls[0].delay != 0 {
		t.Errorf("release delay = %v, want 0 (due again at once)", q.calls[0].delay)
	}
	if len(m.released) != 1 || m.released[0] != ReleaseLeaseExpiring {
		t.Errorf("release reasons = %v, want [%s]", m.released, ReleaseLeaseExpiring)
	}
}

func TestDeliver_PastDeadlineReleasesWithoutTakingAToken(t *testing.T) {
	q := &fakeQueue{}
	var tokens atomic.Int32
	counting := limiterFunc(func(context.Context, string) error { tokens.Add(1); return nil })
	w := newTestWorker(q, &scriptedSender{reply: domain.ProviderReply{StatusCode: 202}}, counting)

	w.deliver(context.Background(), claimed(1, 5), time.Now().Add(-time.Second))

	if ops := q.ops(); len(ops) != 1 || ops[0] != "release" || tokens.Load() != 0 {
		t.Errorf("ops = %v, tokens = %d; want a release and no token", ops, tokens.Load())
	}
}

// The batch loop computes the deadline from the claim time: with a lease that leaves no
// time for queued rows, the first row is sent and later rows are released.
func TestLoop_LateRowsOfABatchAreReleased(t *testing.T) {
	q := &fakeQueue{}
	for range 3 {
		q.pending = append(q.pending, claimed(1, 5))
	}
	cfg := testConfig()
	cfg.SendTimeout = 10 * time.Millisecond
	cfg.Lease = cfg.SendTimeout + StoreTimeout + 30*time.Millisecond // 30 ms to start sends
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := limiterFunc(func(ctx context.Context, _ string) error {
		select { // the first token is immediate, later ones take longer than the headroom
		case <-time.After(time.Duration(len(q.ops())) * 100 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	s := &scriptedSender{reply: domain.ProviderReply{StatusCode: 202}}
	w := NewWorker(cfg, Deps{Queue: q, Sender: s, Limiter: slow, Logger: testLogger()})

	go func() {
		for len(q.ops()) < 3 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	w.loop(ctx)

	if ops := q.ops(); len(ops) != 3 || ops[0] != "sent" || ops[1] != "release" || ops[2] != "release" {
		t.Errorf("ops = %v, want sent, release, release", ops)
	}
}

// gatedSender blocks every send until release is closed.
type gatedSender struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *gatedSender) Send(context.Context, domain.Notification) (domain.ProviderReply, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return domain.ProviderReply{StatusCode: 202}, nil
}

// In half-open state the breaker admits a single probe; the other loops must not take a
// rate-limit token for a send the breaker would refuse anyway.
func TestBreaker_HalfOpenTakesOneTokenForTheProbe(t *testing.T) {
	cfg := testConfig()
	cfg.Breaker.OpenTimeout = 20 * time.Millisecond
	q := &fakeQueue{}
	failing := &scriptedSender{reply: domain.ProviderReply{StatusCode: 503}}
	var tokens atomic.Int32
	counting := limiterFunc(func(context.Context, string) error { tokens.Add(1); return nil })

	w := NewWorker(cfg, Deps{Queue: q, Sender: failing, Limiter: counting, Logger: testLogger()})
	for range 3 { // trips after 3 consecutive failures
		w.process(context.Background(), claimed(1, 5))
	}
	if w.breaker.State() != gobreaker.StateOpen {
		t.Fatal("breaker should be open")
	}
	time.Sleep(3 * cfg.Breaker.OpenTimeout)
	if w.breaker.State() != gobreaker.StateHalfOpen {
		t.Fatal("breaker should be half-open")
	}

	probe := &gatedSender{started: make(chan struct{}), release: make(chan struct{})}
	w.sender = probe
	tokens.Store(0)
	done := make(chan struct{})
	go func() { w.process(context.Background(), claimed(1, 5)); close(done) }()
	<-probe.started

	for range 5 { // while the probe is in flight
		w.process(context.Background(), claimed(1, 5))
	}
	close(probe.release)
	<-done

	if got := tokens.Load(); got != 1 {
		t.Errorf("rate-limit tokens taken = %d, want 1 (the probe)", got)
	}
	var released, sent int
	for _, op := range q.ops()[3:] {
		switch op {
		case "release":
			released++
		case "sent":
			sent++
		}
	}
	if released != 5 || sent != 1 {
		t.Errorf("released = %d, sent = %d; want 5 and 1", released, sent)
	}
}

// deadlineQueue records whether outcome writes carry a deadline.
type deadlineQueue struct {
	fakeQueue
	mu        sync.Mutex
	deadlines []bool
}

func (q *deadlineQueue) MarkSent(ctx context.Context, n domain.Notification, id *string, a domain.DeliveryAttempt) error {
	_, ok := ctx.Deadline()
	q.mu.Lock()
	q.deadlines = append(q.deadlines, ok)
	q.mu.Unlock()
	return q.fakeQueue.MarkSent(ctx, n, id, a)
}

func TestDeliver_OutcomeWriteIsBoundedEvenDuringShutdown(t *testing.T) {
	q := &deadlineQueue{}
	ctx, cancel := context.WithCancel(context.Background())
	s := &cancelingSender{cancel: cancel} // shutdown starts mid-send
	w := newTestWorker(q, s, allow)

	w.process(ctx, claimed(1, 5))

	if len(q.deadlines) != 1 || !q.deadlines[0] {
		t.Errorf("MarkSent deadlines = %v, want one write with a deadline", q.deadlines)
	}
}

// failingReleaseQueue fails every release.
type failingReleaseQueue struct{ fakeQueue }

func (q *failingReleaseQueue) Release(context.Context, domain.Notification, time.Duration) error {
	return errors.New("db down")
}

// Release failures on shutdown are logged with the notification's correlation ID, and
// notification_id appears once.
func TestReleaseAll_LogsWithNotificationContext(t *testing.T) {
	var buf bytes.Buffer
	log := observability.NewLogger(&buf, slog.LevelDebug)
	w := NewWorker(testConfig(), Deps{Queue: &failingReleaseQueue{}, Sender: &scriptedSender{}, Limiter: allow, Logger: log})
	n := claimed(1, 5)

	w.releaseAll([]domain.Notification{n}, 0, ReleaseShuttingDown)

	var line string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(l, "release failed") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no release failure logged: %s", buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["correlation_id"] != "corr" || rec["notification_id"] != n.ID.String() {
		t.Errorf("log = %s, want correlation_id and notification_id", line)
	}
	if c := strings.Count(line, `"notification_id"`); c != 1 {
		t.Errorf("notification_id appears %d times: %s", c, line)
	}
}
