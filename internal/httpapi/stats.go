package httpapi

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

// StatsStore provides aggregate statistics; implemented by store/postgres.
type StatsStore interface {
	QueueDepths(ctx context.Context) ([]domain.QueueDepth, error)
	DeliveryStats(ctx context.Context, window time.Duration) ([]domain.DeliveryStats, error)
}

const (
	defaultStatsWindow = 5 * time.Minute
	minStatsWindow     = 10 * time.Second
	maxStatsWindow     = 24 * time.Hour
)

type latencyResponse struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

type queueCounts struct {
	Ready      int `json:"ready"`
	Delayed    int `json:"delayed"`
	Processing int `json:"processing"`
}

type channelQueueResponse struct {
	Channel string `json:"channel"`
	queueCounts
	OldestReadyAgeSeconds float64                `json:"oldest_ready_age_seconds"`
	ByPriority            map[string]queueCounts `json:"by_priority"`
}

type channelDeliveryResponse struct {
	Channel           string          `json:"channel"`
	Attempts          int             `json:"attempts"`
	Succeeded         int             `json:"succeeded"`
	RetryableErrors   int             `json:"retryable_errors"`
	PermanentErrors   int             `json:"permanent_errors"`
	SuccessRate       *float64        `json:"success_rate"`
	Sent              int             `json:"sent"`
	Failed            int             `json:"failed"`
	ProviderLatencyMs latencyResponse `json:"provider_latency_ms"`
	EndToEndLatencyMs latencyResponse `json:"end_to_end_latency_ms"`
}

type statsResponse struct {
	WindowSeconds int64                     `json:"window_seconds"`
	GeneratedAt   time.Time                 `json:"generated_at"`
	Queue         []channelQueueResponse    `json:"queue"`
	Delivery      []channelDeliveryResponse `json:"delivery"`
}

// getStats answers "how is delivery going right now?" without a Prometheus server: the
// current backlog plus success rate and latency percentiles over a recent window. It is
// computed from the database, so it aggregates every worker replica.
func (a *api) getStats(w http.ResponseWriter, r *http.Request) {
	window := defaultStatsWindow
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < minStatsWindow || d > maxStatsWindow {
			a.writeError(w, r, &badRequestError{msg: "invalid query parameters", fields: []domain.FieldError{{
				Field: "window", Message: "must be a duration between " + minStatsWindow.String() + " and " + maxStatsWindow.String() + ", e.g. 5m",
			}}})
			return
		}
		window = d
	}

	depths, err := a.stats.QueueDepths(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	deliveries, err := a.stats.DeliveryStats(r.Context(), window)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, statsResponse{
		WindowSeconds: int64(window / time.Second),
		GeneratedAt:   a.now().UTC(),
		Queue:         toQueueResponse(depths),
		Delivery:      toDeliveryResponse(deliveries),
	})
}

// toQueueResponse reports every channel (and every priority within it), zero-filled.
func toQueueResponse(depths []domain.QueueDepth) []channelQueueResponse {
	out := make([]channelQueueResponse, len(domain.Channels))
	index := map[domain.Channel]int{}
	for i, ch := range domain.Channels {
		index[ch] = i
		out[i] = channelQueueResponse{Channel: string(ch), ByPriority: map[string]queueCounts{}}
		for _, p := range domain.Priorities {
			out[i].ByPriority[p.String()] = queueCounts{}
		}
	}
	for _, d := range depths {
		i, ok := index[d.Channel]
		if !ok {
			continue
		}
		c := &out[i]
		c.Ready += d.Ready
		c.Delayed += d.Delayed
		c.Processing += d.Processing
		c.OldestReadyAgeSeconds = math.Max(c.OldestReadyAgeSeconds, round(d.OldestReadyAge.Seconds()))
		c.ByPriority[d.Priority.String()] = queueCounts{Ready: d.Ready, Delayed: d.Delayed, Processing: d.Processing}
	}
	return out
}

// toDeliveryResponse reports every channel, zero-filled; success_rate is null without attempts.
func toDeliveryResponse(stats []domain.DeliveryStats) []channelDeliveryResponse {
	byChannel := make(map[domain.Channel]domain.DeliveryStats, len(stats))
	for _, s := range stats {
		byChannel[s.Channel] = s
	}
	out := make([]channelDeliveryResponse, len(domain.Channels))
	for i, ch := range domain.Channels {
		s := byChannel[ch]
		out[i] = channelDeliveryResponse{
			Channel: string(ch), Attempts: s.Attempts, Succeeded: s.Succeeded,
			RetryableErrors: s.RetryableErrors, PermanentErrors: s.PermanentErrors,
			Sent: s.Sent, Failed: s.Failed,
			ProviderLatencyMs: toLatency(s.ProviderLatency), EndToEndLatencyMs: toLatency(s.EndToEndLatency),
		}
		if rate, ok := s.SuccessRate(); ok {
			rate = round(rate)
			out[i].SuccessRate = &rate
		}
	}
	return out
}

func toLatency(p domain.Percentiles) latencyResponse {
	ms := func(d time.Duration) float64 { return round(float64(d) / float64(time.Millisecond)) }
	return latencyResponse{P50: ms(p.P50), P95: ms(p.P95), P99: ms(p.P99)}
}

// round keeps responses readable: three decimals are plenty for rates, ms and seconds.
func round(v float64) float64 { return math.Round(v*1000) / 1000 }
