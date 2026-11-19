package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

type fakeStats struct {
	depths     []domain.QueueDepth
	deliveries []domain.DeliveryStats
	err        error
	lastWindow time.Duration
}

func (f *fakeStats) QueueDepths(context.Context) ([]domain.QueueDepth, error) {
	return f.depths, f.err
}

func (f *fakeStats) DeliveryStats(_ context.Context, window time.Duration) ([]domain.DeliveryStats, error) {
	f.lastWindow = window
	return f.deliveries, f.err
}

func newStatsServer(t *testing.T, stats *fakeStats) http.Handler {
	t.Helper()
	deps := testDeps(newFakeStore(), nil)
	deps.Stats = stats
	return NewRouter(deps)
}

func TestGetStats(t *testing.T) {
	stats := &fakeStats{
		depths: []domain.QueueDepth{
			{Channel: domain.ChannelSMS, Priority: domain.PriorityHigh, Ready: 5, Processing: 2, OldestReadyAge: 1500 * time.Millisecond},
			{Channel: domain.ChannelSMS, Priority: domain.PriorityLow, Ready: 10, Delayed: 3, OldestReadyAge: 9 * time.Second},
		},
		deliveries: []domain.DeliveryStats{{
			Channel: domain.ChannelSMS, Attempts: 4, Succeeded: 3, RetryableErrors: 1, Sent: 3,
			ProviderLatency: domain.Percentiles{P50: 42 * time.Millisecond, P95: 90 * time.Millisecond, P99: 99500 * time.Microsecond},
			EndToEndLatency: domain.Percentiles{P50: 2 * time.Second},
		}},
	}
	h := newStatsServer(t, stats)

	rec := do(t, h, http.MethodGet, "/v1/stats?window=15m", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if stats.lastWindow != 15*time.Minute {
		t.Errorf("window passed to store = %v", stats.lastWindow)
	}
	resp := decode[statsResponse](t, rec)
	if resp.WindowSeconds != 900 || len(resp.Queue) != 3 || len(resp.Delivery) != 3 {
		t.Fatalf("resp = %+v, want 900s and all 3 channels", resp)
	}

	sms := resp.Queue[0]
	if sms.Channel != "sms" || sms.Ready != 15 || sms.Delayed != 3 || sms.Processing != 2 || sms.OldestReadyAgeSeconds != 9 {
		t.Errorf("sms queue = %+v", sms)
	}
	if sms.ByPriority["high"].Ready != 5 || sms.ByPriority["normal"] != (queueCounts{}) || len(sms.ByPriority) != 3 {
		t.Errorf("sms by priority = %+v", sms.ByPriority)
	}
	if email := resp.Queue[1]; email.Channel != "email" || email.Ready != 0 || len(email.ByPriority) != 3 {
		t.Errorf("email queue must be zero-filled: %+v", email)
	}

	d := resp.Delivery[0]
	if d.SuccessRate == nil || *d.SuccessRate != 0.75 || d.Sent != 3 {
		t.Errorf("sms delivery = %+v", d)
	}
	if d.ProviderLatencyMs != (latencyResponse{P50: 42, P95: 90, P99: 99.5}) || d.EndToEndLatencyMs.P50 != 2000 {
		t.Errorf("latency = %+v / %+v", d.ProviderLatencyMs, d.EndToEndLatencyMs)
	}
	if resp.Delivery[2].SuccessRate != nil {
		t.Error("success_rate must be null for a channel without attempts")
	}
}

func TestGetStats_Window(t *testing.T) {
	stats := &fakeStats{}
	h := newStatsServer(t, stats)

	do(t, h, http.MethodGet, "/v1/stats", "")
	if stats.lastWindow != 5*time.Minute {
		t.Errorf("default window = %v, want 5m", stats.lastWindow)
	}
	for _, w := range []string{"soon", "1s", "48h", "-5m"} {
		rec := do(t, h, http.MethodGet, "/v1/stats?window="+w, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("window=%s: status = %d, want 400", w, rec.Code)
			continue
		}
		if p := decode[problem](t, rec); len(p.Errors) != 1 || p.Errors[0].Field != "window" {
			t.Errorf("window=%s: errors = %+v", w, p.Errors)
		}
	}
}

func TestGetStats_StoreError(t *testing.T) {
	h := newStatsServer(t, &fakeStats{err: errors.New("db down")})
	if rec := do(t, h, http.MethodGet, "/v1/stats", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestGetStats_NotMountedWithoutStore(t *testing.T) {
	h := newTestServer(t, newFakeStore(), nil)
	if rec := do(t, h, http.MethodGet, "/v1/stats", ""); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
