package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/umutsahin222/insider/internal/delivery"
	"github.com/umutsahin222/insider/internal/domain"
	"github.com/umutsahin222/insider/internal/events"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestHTTPMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewHTTPMetrics(reg)

	m.ObserveRequest("GET", "/v1/notifications/{id}", 200, 30*time.Millisecond)
	m.ObserveRequest("GET", "/v1/notifications/{id}", 200, 10*time.Millisecond)
	m.ObserveRequest("POST", "/v1/notifications", 422, time.Millisecond)
	m.NotificationAccepted(domain.ChannelSMS, domain.PriorityHigh)

	if got := testutil.ToFloat64(m.requests.WithLabelValues("GET", "/v1/notifications/{id}", "200")); got != 2 {
		t.Errorf("requests{GET,200} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.accepted.WithLabelValues("sms", "high")); got != 1 {
		t.Errorf("accepted{sms,high} = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(m.duration); n != 2 {
		t.Errorf("duration series = %d, want 2 (one per method+route)", n)
	}
}

func TestDeliveryMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewDeliveryMetrics(reg)

	m.AttemptFinished(domain.ChannelEmail, domain.OutcomeSuccess, delivery.ResultSent, 40*time.Millisecond)
	m.AttemptFinished(domain.ChannelEmail, domain.OutcomeRetryableError, delivery.ResultRetry, 2*time.Second)
	m.Delivered(domain.ChannelEmail, 3*time.Second)
	m.Released(domain.ChannelEmail, delivery.ReleaseBreakerOpen)
	m.BreakerStateChanged(domain.ChannelEmail, delivery.BreakerOpen)
	m.Reclaimed(4)

	checks := map[string]float64{
		"sent":     testutil.ToFloat64(m.deliveries.WithLabelValues("email", "success", "sent")),
		"retry":    testutil.ToFloat64(m.deliveries.WithLabelValues("email", "retryable_error", "retry_scheduled")),
		"released": testutil.ToFloat64(m.releases.WithLabelValues("email", "circuit_open")),
		"breaker":  testutil.ToFloat64(m.breaker.WithLabelValues("email")),
		"reaped":   testutil.ToFloat64(m.reclaimed),
	}
	want := map[string]float64{"sent": 1, "retry": 1, "released": 1, "breaker": 2, "reaped": 4}
	for k, v := range want {
		if checks[k] != v {
			t.Errorf("%s = %v, want %v", k, checks[k], v)
		}
	}

	// The exposition must contain the histograms with their documented names.
	rec := httptest.NewRecorder()
	MetricsHandler(reg).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, name := range []string{
		"notification_provider_request_duration_seconds_bucket",
		`notification_end_to_end_latency_seconds_count{channel="email"} 1`,
	} {
		if !strings.Contains(rec.Body.String(), name) {
			t.Errorf("exposition lacks %q", name)
		}
	}
}

type fakeDepths struct {
	calls  atomic.Int32
	depths []domain.QueueDepth
	outbox domain.OutboxBacklog
	err    error
}

func (f *fakeDepths) QueueDepths(context.Context) ([]domain.QueueDepth, error) {
	f.calls.Add(1)
	return f.depths, f.err
}

func (f *fakeDepths) OutboxBacklog(context.Context) (domain.OutboxBacklog, error) {
	return f.outbox, f.err
}

func TestQueueCollector(t *testing.T) {
	src := &fakeDepths{depths: []domain.QueueDepth{
		{Channel: domain.ChannelSMS, Priority: domain.PriorityHigh, Ready: 7, Delayed: 2, Processing: 3, OldestReadyAge: 4 * time.Second},
		{Channel: domain.ChannelSMS, Priority: domain.PriorityLow, Ready: 1, OldestReadyAge: 9 * time.Second},
	}, outbox: domain.OutboxBacklog{Pending: 12, OldestAge: 1500 * time.Millisecond}}
	c := NewQueueCollector(src, time.Second, time.Hour, discard)

	expected := `
# HELP notification_outbox_oldest_age_seconds How long the oldest unrelayed status event has waited (0 when none).
# TYPE notification_outbox_oldest_age_seconds gauge
notification_outbox_oldest_age_seconds 1.5
# HELP notification_outbox_pending Status events recorded but not yet relayed to the event stream.
# TYPE notification_outbox_pending gauge
notification_outbox_pending 12
# HELP notification_queue_oldest_ready_age_seconds How long the oldest due notification of a channel has been waiting (queue lag).
# TYPE notification_queue_oldest_ready_age_seconds gauge
notification_queue_oldest_ready_age_seconds{channel="email"} 0
notification_queue_oldest_ready_age_seconds{channel="push"} 0
notification_queue_oldest_ready_age_seconds{channel="sms"} 9
# HELP notification_queue_stats_up 1 if the last queue statistics query succeeded, 0 otherwise.
# TYPE notification_queue_stats_up gauge
notification_queue_stats_up 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"notification_queue_oldest_ready_age_seconds", "notification_queue_stats_up",
		"notification_outbox_pending", "notification_outbox_oldest_age_seconds"); err != nil {
		t.Error(err)
	}

	// 3 channels × 3 priorities × 3 states, zero-filled.
	if n := testutil.CollectAndCount(c, "notification_queue_depth"); n != 27 {
		t.Errorf("depth series = %d, want 27", n)
	}
	if calls := src.calls.Load(); calls != 1 {
		t.Errorf("source queried %d times across scrapes, want 1 (cached)", calls)
	}
}

func TestRelayAndHubMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	relay := NewRelayMetrics(reg)
	relay.RelayLeading(true)
	relay.EventsRelayed(3)
	relay.EventsRelayed(2)
	relay.RelayPublishFailed()
	if got := testutil.ToFloat64(relay.leader); got != 1 {
		t.Errorf("leader = %v, want 1", got)
	}
	relay.RelayLeading(false)
	if got := testutil.ToFloat64(relay.leader); got != 0 {
		t.Errorf("leader after resigning = %v, want 0", got)
	}
	if got := testutil.ToFloat64(relay.relayed); got != 5 {
		t.Errorf("relayed = %v, want 5", got)
	}
	if got := testutil.ToFloat64(relay.publishFailure); got != 1 {
		t.Errorf("publish failures = %v, want 1", got)
	}

	httpm := NewHTTPMetrics(prometheus.NewRegistry())
	if n := testutil.CollectAndCount(httpm.subscriberDrops); n != 3 {
		t.Errorf("drop series before any drop = %d, want all 3 reasons at zero", n)
	}
	httpm.SubscriberDropped(events.DropResync)
	if got := testutil.ToFloat64(httpm.subscriberDrops.WithLabelValues("resync")); got != 1 {
		t.Errorf("drops{resync} = %v, want 1", got)
	}
}

func TestQueueCollector_ErrorReportsDown(t *testing.T) {
	src := &fakeDepths{err: errors.New("db down")}
	c := NewQueueCollector(src, time.Second, 0, discard)

	expected := `
# HELP notification_queue_stats_up 1 if the last queue statistics query succeeded, 0 otherwise.
# TYPE notification_queue_stats_up gauge
notification_queue_stats_up 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Error(err)
	}
}
