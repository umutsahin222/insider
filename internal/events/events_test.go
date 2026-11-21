package events

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
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
		Attempts: 3, Error: &msg, OccurredAt: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), StreamID: "1763553600000-7"}
	b, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.NotificationID != in.NotificationID || *out.BatchID != batch || out.Status != in.Status ||
		out.Attempts != 3 || *out.Error != msg || !out.OccurredAt.Equal(in.OccurredAt) || out.StreamID != in.StreamID {
		t.Errorf("round trip = %+v", out)
	}

	snapshot := in
	snapshot.StreamID = ""
	b, _ = Marshal(snapshot)
	if out, _ := Unmarshal(b); out.StreamID != "" {
		t.Errorf("snapshot event got event_id %q, want none", out.StreamID)
	}
	if _, err := Unmarshal([]byte(`{"status":"sent"}`)); err == nil {
		t.Error("an event without notification_id must be rejected")
	}
}

func TestStreamIDs(t *testing.T) {
	for _, s := range []string{"0-0", "1763553600000-0", "1763553600000-12"} {
		if !ParseStreamID(s) {
			t.Errorf("%q should parse", s)
		}
	}
	for _, s := range []string{"", "-", "abc-1", "1-", "1", "1-2-3", "-1-2", "$"} {
		if ParseStreamID(s) {
			t.Errorf("%q should not parse", s)
		}
	}
	tests := []struct {
		a, b string
		want bool
	}{
		{"10-0", "9-5", true},
		{"10-1", "10-0", true},
		{"10-0", "10-0", false},
		{"9-9", "10-0", false},
		{"100-0", "99-0", true}, // numeric, not lexical
		{"bad", "1-0", false},
	}
	for _, tt := range tests {
		if got := StreamIDAfter(tt.a, tt.b); got != tt.want {
			t.Errorf("StreamIDAfter(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

type recordingHubMetrics struct {
	mu      sync.Mutex
	dropped []DropReason
}

func (m *recordingHubMetrics) SubscriberDropped(r DropReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropped = append(m.dropped, r)
}

func TestHub_DispatchesOnlyMatchingEvents(t *testing.T) {
	h := NewHub(nil, discard, nil)
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
	m := &recordingHubMetrics{}
	h := NewHub(nil, discard, m)
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
	if len(m.dropped) != 1 || m.dropped[0] != DropSlowConsumer {
		t.Errorf("dropped = %v, want [slow_consumer]", m.dropped)
	}
	slow.Close() // already dropped: must not panic on double close
}

func TestHub_DropAllClosesEverySubscription(t *testing.T) {
	m := &recordingHubMetrics{}
	h := NewHub(nil, discard, m)
	s1, s2 := h.Subscribe(forID(uuid.New()), 1), h.Subscribe(forID(uuid.New()), 1)
	h.DropAll(DropShutdown)
	for _, s := range []*Subscription{s1, s2} {
		if _, open := <-s.Events(); open {
			t.Error("subscription still open after DropAll")
		}
	}
	if h.Subscribers() != 0 {
		t.Errorf("subscribers = %d", h.Subscribers())
	}
	if len(m.dropped) != 2 || m.dropped[0] != DropShutdown {
		t.Errorf("dropped = %v, want two shutdown drops", m.dropped)
	}
}

func TestHub_ReplayRefusesHistoryItDidNotRead(t *testing.T) {
	h := NewHub(nil, discard, nil) // never positioned: no history at all
	if _, err := h.Replay(context.Background(), "1-0", func(domain.StatusEvent) bool { return true }); !errors.Is(err, ErrReplayUnavailable) {
		t.Errorf("err = %v, want ErrReplayUnavailable", err)
	}
	if _, err := h.Replay(context.Background(), "not-an-id", func(domain.StatusEvent) bool { return true }); !errors.Is(err, ErrReplayUnavailable) {
		t.Errorf("malformed id: err = %v, want ErrReplayUnavailable", err)
	}
}

// fakeOutbox records what the relay does with it.
type fakeOutbox struct {
	mu         sync.Mutex
	lead       bool
	pending    []domain.OutboxEvent
	pendingErr error
	tryLeads   int
	resigns    int
	removed    [][]int64
}

func (o *fakeOutbox) TryLead(context.Context) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tryLeads++
	return o.lead, nil
}

func (o *fakeOutbox) Resign() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.resigns++
}

func (o *fakeOutbox) Pending(context.Context, int) ([]domain.OutboxEvent, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pending, o.pendingErr
}

func (o *fakeOutbox) Remove(_ context.Context, seqs []int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.removed = append(o.removed, seqs)
	o.pending = nil
	return nil
}

type recordingRelayMetrics struct {
	mu             sync.Mutex
	leading        []bool
	relayed        int
	publishFailure int
}

func (m *recordingRelayMetrics) RelayLeading(l bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leading = append(m.leading, l)
}

func (m *recordingRelayMetrics) EventsRelayed(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.relayed += n
}

func (m *recordingRelayMetrics) RelayPublishFailed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishFailure++
}

// unreachableRedis returns a client for an address nothing listens on.
func unreachableRedis(t *testing.T) *redis.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func runRelayFor(t *testing.T, r *Relay, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(d + 3*time.Second):
		t.Fatal("relay did not stop after its context ended")
	}
}

func TestRelay_KeepsEventsWhileRedisIsDown(t *testing.T) {
	o := &fakeOutbox{lead: true, pending: []domain.OutboxEvent{{Seq: 1, Event: event(uuid.New(), domain.StatusSent)}}}
	m := &recordingRelayMetrics{}
	r := NewRelay(o, unreachableRedis(t), RelayConfig{MaxLen: 100, PollInterval: 10 * time.Millisecond, BatchSize: 10}, m, discard)

	// Long enough for a failed publish everywhere: Windows takes seconds to refuse a connection.
	runRelayFor(t, r, 3*time.Second)

	if len(o.removed) != 0 {
		t.Errorf("removed %v: events must stay in the outbox until they are in the stream", o.removed)
	}
	if m.publishFailure == 0 {
		t.Error("publish failures were not counted")
	}
	if o.tryLeads != 1 {
		t.Errorf("tried to lead %d times; a Redis failure must not cost the leadership", o.tryLeads)
	}
}

func TestRelay_StandsByWhileAnotherProcessLeads(t *testing.T) {
	o := &fakeOutbox{lead: false, pendingErr: errors.New("must not be read")}
	m := &recordingRelayMetrics{}
	r := NewRelay(o, unreachableRedis(t), RelayConfig{MaxLen: 100, PollInterval: 10 * time.Millisecond, BatchSize: 10}, m, discard)

	runRelayFor(t, r, 100*time.Millisecond)

	if len(m.leading) == 0 || m.leading[0] {
		t.Errorf("leading = %v, want reported as not leading", m.leading)
	}
}

func TestRelay_ResignsWhenItsSessionFails(t *testing.T) {
	o := &fakeOutbox{lead: true, pendingErr: errors.New("connection reset")}
	r := NewRelay(o, unreachableRedis(t), RelayConfig{MaxLen: 100, PollInterval: 10 * time.Millisecond, BatchSize: 10}, nil, discard)

	runRelayFor(t, r, 1500*time.Millisecond)

	if o.resigns < 2 || o.tryLeads < 2 {
		t.Errorf("resigns = %d, tryLeads = %d: a failed session must resign and try to lead again", o.resigns, o.tryLeads)
	}
}
