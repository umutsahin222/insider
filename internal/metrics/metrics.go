// Package metrics implements the Prometheus instrumentation of the API and the workers.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/umutsahin222/insider/internal/delivery"
	"github.com/umutsahin222/insider/internal/domain"
)

// Metric names and labels are documented in ADR-007. Label values come from closed sets
// (channels, priorities, outcomes, route patterns) to keep cardinality bounded.

// NewRegistry returns a registry with Go runtime and process collectors. Each binary owns
// its registry; nothing is registered globally.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// MetricsHandler serves reg in the Prometheus exposition format.
func MetricsHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
}

// HTTPMetrics instruments the HTTP API; it implements httpapi.Metrics.
type HTTPMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	accepted *prometheus.CounterVec
}

// NewHTTPMetrics registers the HTTP API metrics with reg.
func NewHTTPMetrics(reg prometheus.Registerer) *HTTPMetrics {
	m := &HTTPMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests by method, route pattern and status code.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by method and route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		accepted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifications_accepted_total",
			Help: "Notifications accepted into the queue, by channel and priority.",
		}, []string{"channel", "priority"}),
	}
	reg.MustRegister(m.requests, m.duration, m.accepted)
	return m
}

// ObserveRequest records one HTTP request. route must be a pattern ("/v1/notifications/{id}"),
// never a raw path, or every ID would become its own time series.
func (m *HTTPMetrics) ObserveRequest(method, route string, status int, d time.Duration) {
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(method, route).Observe(d.Seconds())
}

// NotificationAccepted counts one newly queued notification.
func (m *HTTPMetrics) NotificationAccepted(ch domain.Channel, p domain.Priority) {
	m.accepted.WithLabelValues(string(ch), p.String()).Inc()
}

// DeliveryMetrics instruments the workers; it implements delivery.Metrics.
type DeliveryMetrics struct {
	deliveries *prometheus.CounterVec
	provider   *prometheus.HistogramVec
	endToEnd   *prometheus.HistogramVec
	rateWait   *prometheus.HistogramVec
	releases   *prometheus.CounterVec
	breaker    *prometheus.GaugeVec
	reclaimed  prometheus.Counter
}

var _ delivery.Metrics = (*DeliveryMetrics)(nil)

// NewDeliveryMetrics registers the worker metrics with reg.
func NewDeliveryMetrics(reg prometheus.Registerer) *DeliveryMetrics {
	m := &DeliveryMetrics{
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notification_deliveries_total",
			Help: "Delivery attempts by channel, provider outcome and resulting notification state.",
		}, []string{"channel", "outcome", "result"}),
		provider: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "notification_provider_request_duration_seconds",
			Help:    "Duration of one provider call, by channel and outcome.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"channel", "outcome"}),
		endToEnd: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "notification_end_to_end_latency_seconds",
			Help:    "Time from a notification becoming due (created or scheduled) to being sent, including retries.",
			Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600},
		}, []string{"channel"}),
		rateWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "notification_rate_limit_wait_seconds",
			Help:    "Time a send waited for a rate-limit token.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"channel"}),
		releases: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notification_releases_total",
			Help: "Claimed notifications handed back to the queue without using an attempt, by reason.",
		}, []string{"channel", "reason"}),
		breaker: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "notification_circuit_breaker_state",
			Help: "Circuit breaker state per channel: 0 closed, 1 half-open, 2 open.",
		}, []string{"channel"}),
		reclaimed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "notification_reaper_reclaimed_total",
			Help: "Notifications whose expired lease was taken back by the reaper.",
		}),
	}
	reg.MustRegister(m.deliveries, m.provider, m.endToEnd, m.rateWait, m.releases, m.breaker, m.reclaimed)
	return m
}

func (m *DeliveryMetrics) AttemptFinished(ch domain.Channel, o domain.AttemptOutcome, r delivery.Result, d time.Duration) {
	m.deliveries.WithLabelValues(string(ch), string(o), string(r)).Inc()
	m.provider.WithLabelValues(string(ch), string(o)).Observe(d.Seconds())
}

func (m *DeliveryMetrics) Delivered(ch domain.Channel, sinceDue time.Duration) {
	m.endToEnd.WithLabelValues(string(ch)).Observe(sinceDue.Seconds())
}

func (m *DeliveryMetrics) RateLimitWaited(ch domain.Channel, d time.Duration) {
	m.rateWait.WithLabelValues(string(ch)).Observe(d.Seconds())
}

func (m *DeliveryMetrics) Released(ch domain.Channel, r delivery.ReleaseReason) {
	m.releases.WithLabelValues(string(ch), string(r)).Inc()
}

func (m *DeliveryMetrics) BreakerStateChanged(ch domain.Channel, s delivery.BreakerState) {
	m.breaker.WithLabelValues(string(ch)).Set(float64(s))
}

func (m *DeliveryMetrics) Reclaimed(n int) { m.reclaimed.Add(float64(n)) }

// QueueDepthSource reports the current backlog; implemented by store/postgres.
type QueueDepthSource interface {
	QueueDepths(ctx context.Context) ([]domain.QueueDepth, error)
}

// QueueCollector exports queue depth as gauges, read from the database at scrape time so
// the value is current, not a periodic snapshot. Results are cached for minInterval so
// several scrapers (or API replicas behind one Prometheus job) cannot hammer the database.
type QueueCollector struct {
	src         QueueDepthSource
	timeout     time.Duration
	minInterval time.Duration
	log         *slog.Logger

	depth, oldest, up *prometheus.Desc

	mu      sync.Mutex
	last    []domain.QueueDepth
	lastErr error
	lastAt  time.Time
	now     func() time.Time
}

// NewQueueCollector returns a collector querying src with the given timeout per scrape.
func NewQueueCollector(src QueueDepthSource, timeout, minInterval time.Duration, log *slog.Logger) *QueueCollector {
	return &QueueCollector{
		src: src, timeout: timeout, minInterval: minInterval, log: log, now: time.Now,
		depth: prometheus.NewDesc("notification_queue_depth",
			"Unfinished notifications by channel, priority and state (ready: due now; delayed: scheduled or backing off; processing: claimed).",
			[]string{"channel", "priority", "state"}, nil),
		oldest: prometheus.NewDesc("notification_queue_oldest_ready_age_seconds",
			"How long the oldest due notification of a channel has been waiting (queue lag).",
			[]string{"channel"}, nil),
		up: prometheus.NewDesc("notification_queue_stats_up",
			"1 if the last queue statistics query succeeded, 0 otherwise.", nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *QueueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.depth
	ch <- c.oldest
	ch <- c.up
}

// Collect implements prometheus.Collector. Every channel/priority/state combination is
// always exported, with zeros for empty queues, so series never vanish from graphs.
func (c *QueueCollector) Collect(ch chan<- prometheus.Metric) {
	depths, err := c.read()
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)

	type key struct {
		ch domain.Channel
		p  domain.Priority
	}
	byKey := make(map[key]domain.QueueDepth, len(depths))
	oldest := map[domain.Channel]time.Duration{}
	for _, d := range depths {
		byKey[key{d.Channel, d.Priority}] = d
		oldest[d.Channel] = max(oldest[d.Channel], d.OldestReadyAge)
	}
	for _, channel := range domain.Channels {
		for _, p := range domain.Priorities {
			d := byKey[key{channel, p}]
			for state, v := range map[string]int{"ready": d.Ready, "delayed": d.Delayed, "processing": d.Processing} {
				ch <- prometheus.MustNewConstMetric(c.depth, prometheus.GaugeValue, float64(v), string(channel), p.String(), state)
			}
		}
		ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, oldest[channel].Seconds(), string(channel))
	}
}

func (c *QueueCollector) read() ([]domain.QueueDepth, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.lastAt.IsZero() && c.now().Sub(c.lastAt) < c.minInterval {
		return c.last, c.lastErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	c.last, c.lastErr = c.src.QueueDepths(ctx)
	c.lastAt = c.now()
	if c.lastErr != nil {
		c.log.Warn("queue depth query failed", "error", c.lastErr)
	}
	return c.last, c.lastErr
}
