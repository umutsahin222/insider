package delivery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sony/gobreaker/v2"

	"github.com/umutsahin222/insider/internal/domain"
)

type call struct {
	op      string // sent | retry | failed | release
	id      uuid.UUID
	delay   time.Duration
	attempt domain.DeliveryAttempt
	msgID   *string
}

type fakeQueue struct {
	mu      sync.Mutex
	pending []domain.Notification
	calls   []call
}

func (q *fakeQueue) Claim(_ context.Context, _ domain.Channel, limit int, _ time.Duration) ([]domain.Notification, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := min(limit, len(q.pending))
	out := q.pending[:n]
	q.pending = q.pending[n:]
	return out, nil
}

func (q *fakeQueue) record(c call) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls = append(q.calls, c)
	return nil
}

func (q *fakeQueue) MarkSent(_ context.Context, n domain.Notification, id *string, a domain.DeliveryAttempt) error {
	return q.record(call{op: "sent", id: n.ID, attempt: a, msgID: id})
}

func (q *fakeQueue) ScheduleRetry(_ context.Context, n domain.Notification, d time.Duration, a domain.DeliveryAttempt) error {
	return q.record(call{op: "retry", id: n.ID, delay: d, attempt: a})
}

func (q *fakeQueue) MarkFailed(_ context.Context, n domain.Notification, a domain.DeliveryAttempt) error {
	return q.record(call{op: "failed", id: n.ID, attempt: a})
}

func (q *fakeQueue) Release(_ context.Context, n domain.Notification, d time.Duration) error {
	return q.record(call{op: "release", id: n.ID, delay: d})
}

func (q *fakeQueue) ops() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, len(q.calls))
	for i, c := range q.calls {
		out[i] = c.op
	}
	return out
}

type scriptedSender struct {
	mu    sync.Mutex
	reply domain.ProviderReply
	err   error
	sent  int
}

func (s *scriptedSender) Send(context.Context, domain.Notification) (domain.ProviderReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent++
	return s.reply, s.err
}

type limiterFunc func(ctx context.Context, key string) error

func (f limiterFunc) Wait(ctx context.Context, key string) error { return f(ctx, key) }

var allow = limiterFunc(func(context.Context, string) error { return nil })

func testConfig() Config {
	return Config{
		Channel: domain.ChannelSMS, Concurrency: 2, BatchSize: 5, Lease: time.Minute,
		PollInterval: 5 * time.Millisecond, SendTimeout: time.Second,
		Retry: RetryPolicy{Base: 2 * time.Second, Max: 5 * time.Minute},
		Breaker: BreakerPolicy{
			ConsecutiveFailures: 3, FailureRatio: 0.5, MinRequests: 20,
			Window: time.Minute, OpenTimeout: time.Minute,
		},
	}
}

// recordingMetrics captures what the worker reports.
type recordingMetrics struct {
	mu        sync.Mutex
	attempts  []string // "<outcome>/<result>"
	delivered []time.Duration
	released  []ReleaseReason
	breaker   []BreakerState
	waits     int
	reclaimed int
}

func (m *recordingMetrics) AttemptFinished(_ domain.Channel, o domain.AttemptOutcome, r Result, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts = append(m.attempts, string(o)+"/"+string(r))
}

func (m *recordingMetrics) Delivered(_ domain.Channel, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delivered = append(m.delivered, d)
}

func (m *recordingMetrics) RateLimitWaited(domain.Channel, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.waits++
}

func (m *recordingMetrics) Released(_ domain.Channel, r ReleaseReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.released = append(m.released, r)
}

func (m *recordingMetrics) BreakerStateChanged(_ domain.Channel, s BreakerState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.breaker = append(m.breaker, s)
}

func (m *recordingMetrics) Reclaimed(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reclaimed += n
}

func newTestWorker(q Queue, s Sender, l Limiter) *Worker {
	return newMeteredTestWorker(q, s, l, nil)
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newMeteredTestWorker(q Queue, s Sender, l Limiter, m Metrics) *Worker {
	w := NewWorker(testConfig(), Deps{Queue: q, Sender: s, Limiter: l, Metrics: m, Logger: testLogger()})
	w.randN = func(n int64) int64 { return n - 1 } // deterministic: top of the jitter range
	return w
}

func claimed(attempts, maxAttempts int) domain.Notification {
	return domain.Notification{ID: uuid.New(), Channel: domain.ChannelSMS, Status: domain.StatusProcessing,
		Attempts: attempts, MaxAttempts: maxAttempts, CorrelationID: "corr"}
}

func TestProcess_Outcomes(t *testing.T) {
	tests := []struct {
		name      string
		reply     domain.ProviderReply
		err       error
		attempts  int
		wantOp    string
		wantDelay time.Duration
		wantCode  *int
	}{
		{name: "accepted", reply: domain.ProviderReply{StatusCode: 202, MessageID: "m-1"}, attempts: 1, wantOp: "sent"},
		{name: "server error retries with backoff", reply: domain.ProviderReply{StatusCode: 503}, attempts: 2, wantOp: "retry", wantDelay: 4 * time.Second},
		{name: "429 honours retry-after", reply: domain.ProviderReply{StatusCode: 429, RetryAfter: 9 * time.Second}, attempts: 1, wantOp: "retry", wantDelay: 9 * time.Second},
		{name: "timeout retries", err: context.DeadlineExceeded, attempts: 1, wantOp: "retry", wantDelay: 2 * time.Second},
		{name: "bad request fails at once", reply: domain.ProviderReply{StatusCode: 400}, attempts: 1, wantOp: "failed"},
		{name: "last attempt fails", reply: domain.ProviderReply{StatusCode: 500}, attempts: 5, wantOp: "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &fakeQueue{}
			w := newTestWorker(q, &scriptedSender{reply: tt.reply, err: tt.err}, allow)
			n := claimed(tt.attempts, 5)

			w.process(context.Background(), n)

			if len(q.calls) != 1 {
				t.Fatalf("calls = %v, want one", q.ops())
			}
			c := q.calls[0]
			if c.op != tt.wantOp || c.id != n.ID {
				t.Fatalf("op = %s, want %s", c.op, tt.wantOp)
			}
			if c.delay != tt.wantDelay {
				t.Errorf("delay = %v, want %v", c.delay, tt.wantDelay)
			}
			if c.attempt.Attempt != tt.attempts || c.attempt.NotificationID != n.ID {
				t.Errorf("attempt record = %+v", c.attempt)
			}
			if tt.err != nil && c.attempt.StatusCode != nil {
				t.Error("transport errors must not record a status code")
			}
			if tt.wantOp != "sent" && (c.attempt.Error == nil || *c.attempt.Error == "") {
				t.Error("failed attempts must record an error")
			}
			if tt.wantOp == "sent" && (c.msgID == nil || *c.msgID != "m-1") {
				t.Errorf("provider message id = %v", c.msgID)
			}
		})
	}
}

func TestProcess_LimiterFailureReleasesWithoutAttempt(t *testing.T) {
	q := &fakeQueue{}
	s := &scriptedSender{reply: domain.ProviderReply{StatusCode: 202}}
	w := newTestWorker(q, s, limiterFunc(func(context.Context, string) error { return errors.New("redis down") }))

	w.process(context.Background(), claimed(1, 5))

	if ops := q.ops(); len(ops) != 1 || ops[0] != "release" || s.sent != 0 {
		t.Errorf("ops = %v, sent = %d; want a single release and no send", ops, s.sent)
	}
}

func TestBreaker_OpensAndStopsSending(t *testing.T) {
	q := &fakeQueue{}
	s := &scriptedSender{reply: domain.ProviderReply{StatusCode: 503}}
	w := newTestWorker(q, s, allow) // trips after 3 consecutive failures

	for range 5 {
		w.process(context.Background(), claimed(1, 5))
	}

	ops := q.ops()
	want := []string{"retry", "retry", "retry", "release", "release"}
	if len(ops) != len(want) {
		t.Fatalf("ops = %v, want %v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Fatalf("ops = %v, want %v", ops, want)
		}
	}
	if s.sent != 3 {
		t.Errorf("provider called %d times, want 3 (breaker open afterwards)", s.sent)
	}
	if q.calls[3].delay != time.Minute {
		t.Errorf("released with delay %v, want the breaker open timeout", q.calls[3].delay)
	}
}

func TestBreaker_PermanentErrorsDoNotTrip(t *testing.T) {
	q := &fakeQueue{}
	s := &scriptedSender{reply: domain.ProviderReply{StatusCode: 400}}
	w := newTestWorker(q, s, allow)

	for range 6 {
		w.process(context.Background(), claimed(1, 5))
	}
	if s.sent != 6 {
		t.Errorf("provider called %d times, want 6: bad requests are not provider outages", s.sent)
	}
}

func TestRun_DrainsQueueAndStopsOnCancel(t *testing.T) {
	q := &fakeQueue{}
	for range 12 {
		q.pending = append(q.pending, claimed(1, 5))
	}
	s := &scriptedSender{reply: domain.ProviderReply{StatusCode: 202}}
	w := newTestWorker(q, s, allow)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for {
		q.mu.Lock()
		n := len(q.calls)
		q.mu.Unlock()
		if n == 12 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("processed %d of 12", n)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	for _, op := range q.ops() {
		if op != "sent" {
			t.Fatalf("ops = %v, want all sent", q.ops())
		}
	}
}

func TestLoop_ReleasesUnprocessedClaimsOnShutdown(t *testing.T) {
	q := &fakeQueue{}
	for range 5 {
		q.pending = append(q.pending, claimed(1, 5))
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel during the first send: the rest of the claimed batch must go back to the queue.
	s := &cancelingSender{cancel: cancel}
	w := newTestWorker(q, s, allow)

	w.loop(ctx)

	ops := q.ops()
	if len(ops) != 5 || ops[0] != "sent" {
		t.Fatalf("ops = %v, want sent followed by 4 releases", ops)
	}
	for _, op := range ops[1:] {
		if op != "release" {
			t.Fatalf("ops = %v", ops)
		}
	}
}

func TestMetrics_ReportOutcomesReleasesAndBreaker(t *testing.T) {
	m := &recordingMetrics{}
	s := &scriptedSender{reply: domain.ProviderReply{StatusCode: 202}}
	w := newMeteredTestWorker(&fakeQueue{}, s, allow, m)

	due := claimed(1, 5)
	due.CreatedAt = time.Now().Add(-3 * time.Second)
	w.process(context.Background(), due)

	s.reply = domain.ProviderReply{StatusCode: 400}
	w.process(context.Background(), claimed(1, 5))

	s.reply = domain.ProviderReply{StatusCode: 503} // trips the breaker after 3 failures
	for range 4 {
		w.process(context.Background(), claimed(1, 5))
	}

	want := []string{
		"success/sent", "permanent_error/failed",
		"retryable_error/retry_scheduled", "retryable_error/retry_scheduled", "retryable_error/retry_scheduled",
	}
	if strings.Join(m.attempts, ",") != strings.Join(want, ",") {
		t.Errorf("attempts = %v, want %v", m.attempts, want)
	}
	if len(m.delivered) != 1 || m.delivered[0] < 3*time.Second || m.delivered[0] > 10*time.Second {
		t.Errorf("delivered = %v, want one ≈3s end-to-end latency", m.delivered)
	}
	if len(m.released) != 1 || m.released[0] != ReleaseBreakerOpen {
		t.Errorf("released = %v, want one circuit_open", m.released)
	}
	if len(m.breaker) != 2 || m.breaker[0] != BreakerClosed || m.breaker[1] != BreakerOpen {
		t.Errorf("breaker states = %v, want [closed open]", m.breaker)
	}
	if m.waits != 5 {
		t.Errorf("rate-limit waits = %d, want 5 (one per send that got a token)", m.waits)
	}
}

func TestMetrics_ScheduledNotificationLatencyStartsWhenDue(t *testing.T) {
	m := &recordingMetrics{}
	w := newMeteredTestWorker(&fakeQueue{}, &scriptedSender{reply: domain.ProviderReply{StatusCode: 202}}, allow, m)

	n := claimed(1, 5)
	n.CreatedAt = time.Now().Add(-time.Hour)
	at := time.Now().Add(-2 * time.Second)
	n.ScheduledAt = &at
	w.process(context.Background(), n)

	if len(m.delivered) != 1 || m.delivered[0] > 10*time.Second {
		t.Errorf("delivered = %v, want ≈2s (since scheduled time, not creation)", m.delivered)
	}
}

func TestMetrics_LeaseLostIsReported(t *testing.T) {
	m := &recordingMetrics{}
	q := &leaseLosingQueue{}
	w := newMeteredTestWorker(q, &scriptedSender{reply: domain.ProviderReply{StatusCode: 202}}, allow, m)

	w.process(context.Background(), claimed(1, 5))

	if len(m.attempts) != 1 || m.attempts[0] != "success/lease_lost" || len(m.delivered) != 0 {
		t.Errorf("attempts = %v delivered = %v; want success/lease_lost and no delivery", m.attempts, m.delivered)
	}
}

type leaseLosingQueue struct{ fakeQueue }

func (q *leaseLosingQueue) MarkSent(context.Context, domain.Notification, *string, domain.DeliveryAttempt) error {
	return domain.ErrLeaseLost
}

func TestReaper_ReportsReclaimed(t *testing.T) {
	m := &recordingMetrics{}
	ctx, cancel := context.WithCancel(context.Background())
	r := reaperFunc(func(context.Context) (int, error) { cancel(); return 7, nil })

	RunReaper(ctx, r, time.Millisecond, m, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if m.reclaimed != 7 {
		t.Errorf("reclaimed = %d, want 7", m.reclaimed)
	}
}

type reaperFunc func(context.Context) (int, error)

func (f reaperFunc) ReapExpired(ctx context.Context) (int, error) { return f(ctx) }

func TestBreakerPolicy(t *testing.T) {
	p := BreakerPolicy{ConsecutiveFailures: 10, FailureRatio: 0.5, MinRequests: 20}
	tests := []struct {
		name string
		c    gobreaker.Counts
		want bool
	}{
		{"healthy", gobreaker.Counts{Requests: 100, TotalFailures: 5, ConsecutiveFailures: 1}, false},
		{"noisy but working (30%)", gobreaker.Counts{Requests: 100, TotalFailures: 30, ConsecutiveFailures: 4}, false},
		{"mostly failing", gobreaker.Counts{Requests: 40, TotalFailures: 20, ConsecutiveFailures: 2}, true},
		{"high ratio but too few calls to judge", gobreaker.Counts{Requests: 8, TotalFailures: 6, ConsecutiveFailures: 3}, false},
		{"hard outage at low traffic", gobreaker.Counts{Requests: 10, TotalFailures: 10, ConsecutiveFailures: 10}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.shouldTrip(tt.c); got != tt.want {
				t.Errorf("shouldTrip(%+v) = %v, want %v", tt.c, got, tt.want)
			}
		})
	}
}

// sequenceSender replies with the given status codes in order, cycling.
type sequenceSender struct {
	mu    sync.Mutex
	codes []int
	sent  int
}

func (s *sequenceSender) Send(context.Context, domain.Notification) (domain.ProviderReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	code := s.codes[s.sent%len(s.codes)]
	s.sent++
	return domain.ProviderReply{StatusCode: code}, nil
}

func TestBreaker_RandomFailuresDoNotStopAWorkingChannel(t *testing.T) {
	cfg := testConfig()
	cfg.Breaker = BreakerPolicy{ConsecutiveFailures: 10, FailureRatio: 0.5, MinRequests: 20, Window: time.Minute, OpenTimeout: time.Minute}
	// 30% failures with streaks of up to 3: the old "5 in a row" rule would survive this,
	// but streaks of 5 happen naturally at 30% — see TestBreakerPolicy for that case.
	s := &sequenceSender{codes: []int{503, 503, 503, 202, 202, 202, 202, 202, 202, 202}}
	q := &fakeQueue{}
	w := NewWorker(cfg, Deps{Queue: q, Sender: s, Limiter: allow, Logger: testLogger()})

	for range 200 {
		w.process(context.Background(), claimed(1, 5))
	}
	if s.sent != 200 {
		t.Errorf("provider called %d times, want 200: a 30%% failing provider must not be cut off", s.sent)
	}
}

func TestBreaker_OpensOnFailureRatio(t *testing.T) {
	cfg := testConfig()
	cfg.Breaker = BreakerPolicy{ConsecutiveFailures: 100, FailureRatio: 0.5, MinRequests: 20, Window: time.Minute, OpenTimeout: time.Minute}
	// Alternating fail/success never builds a streak but is a 50% failure rate.
	s := &sequenceSender{codes: []int{503, 202}}
	w := NewWorker(cfg, Deps{Queue: &fakeQueue{}, Sender: s, Limiter: allow, Logger: testLogger()})

	for range 40 {
		w.process(context.Background(), claimed(1, 5))
	}
	// gobreaker evaluates the trip rule after each failure: call 20 succeeds, call 21 fails
	// with 11/21 ≥ 50% and opens the breaker.
	if s.sent != 21 {
		t.Errorf("provider called %d times, want 21", s.sent)
	}
}

type cancelingSender struct{ cancel context.CancelFunc }

func (s *cancelingSender) Send(context.Context, domain.Notification) (domain.ProviderReply, error) {
	s.cancel()
	return domain.ProviderReply{StatusCode: 202}, nil
}
