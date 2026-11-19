// Package httpapi exposes the notification service over HTTP (see api/openapi.yaml).
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"

	"github.com/umutsahin222/insider/internal/domain"
	"github.com/umutsahin222/insider/internal/observability"
)

// NotificationStore is the persistence the API needs; implemented by store/postgres.
type NotificationStore interface {
	CreateNotification(ctx context.Context, n domain.Notification) (domain.Notification, bool, error)
	CreateBatch(ctx context.Context, b domain.Batch) (domain.Batch, bool, error)
	GetNotification(ctx context.Context, id uuid.UUID) (domain.Notification, error)
	ListAttempts(ctx context.Context, id uuid.UUID) ([]domain.DeliveryAttempt, error)
	ListNotifications(ctx context.Context, f domain.ListFilter) (domain.Page, error)
	CancelNotification(ctx context.Context, id uuid.UUID) (domain.Notification, error)
	RetryNotification(ctx context.Context, id uuid.UUID, extraAttempts int) (domain.Notification, error)
	BatchSummary(ctx context.Context, batchID uuid.UUID) (domain.BatchSummary, error)
	CancelBatch(ctx context.Context, batchID uuid.UUID) (int, error)
}

// Check reports whether a dependency is healthy; used by /readyz.
type Check func(ctx context.Context) error

// Metrics receives API measurements; implemented by metrics.HTTPMetrics.
type Metrics interface {
	// ObserveRequest records one request; route is the matched pattern, not the raw path.
	ObserveRequest(method, route string, status int, d time.Duration)
	// NotificationAccepted counts one newly queued notification (not idempotent replays).
	NotificationAccepted(ch domain.Channel, p domain.Priority)
}

type nopMetrics struct{}

func (nopMetrics) ObserveRequest(string, string, int, time.Duration)    {}
func (nopMetrics) NotificationAccepted(domain.Channel, domain.Priority) {}

// Deps are the explicit dependencies of the HTTP API.
type Deps struct {
	Store  NotificationStore
	Rules  domain.Rules
	Logger *slog.Logger
	// Checks are run by /readyz, keyed by dependency name (e.g. "postgres").
	Checks map[string]Check
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// Metrics records request and intake metrics; nil disables them.
	Metrics Metrics
	// MetricsHandler, if set, is served at GET /metrics.
	MetricsHandler http.Handler
	// Stats, if set, serves GET /v1/stats.
	Stats StatsStore
	// Tracing creates a server span per request and its context is stored with every
	// notification for the worker to continue; nil propagates context without recording.
	Tracing *observability.Tracing
}

const (
	maxSingleBody = 1 << 20  // 1 MiB
	maxBatchBody  = 16 << 20 // 16 MiB: 1000 items × up to 10k-char emails
)

type api struct {
	store      NotificationStore
	rules      domain.Rules
	log        *slog.Logger
	checks     map[string]Check
	now        func() time.Time
	metrics    Metrics
	stats      StatsStore
	propagator propagation.TextMapPropagator
}

// NewRouter builds the HTTP handler with all routes and middleware.
func NewRouter(d Deps) http.Handler {
	a := &api{store: d.Store, rules: d.Rules, log: d.Logger, checks: d.Checks, now: d.Now, metrics: d.Metrics, stats: d.Stats}
	if a.now == nil {
		a.now = time.Now
	}
	if a.metrics == nil {
		a.metrics = nopMetrics{}
	}
	tracing := d.Tracing
	if tracing == nil {
		tracing = observability.DisabledTracing()
	}
	a.propagator = tracing.Propagator

	r := chi.NewRouter()
	r.Use(correlation, a.observe, a.recoverer)

	r.Get("/healthz", a.liveness)
	r.Get("/readyz", a.readiness)
	if d.MetricsHandler != nil {
		r.Method(http.MethodGet, "/metrics", d.MetricsHandler)
	}

	r.Route("/v1", func(r chi.Router) {
		r.With(maxBody(maxSingleBody)).Post("/notifications", a.createNotification)
		r.With(maxBody(maxBatchBody)).Post("/notifications/batch", a.createBatch)
		r.Get("/notifications", a.listNotifications)
		r.Get("/notifications/{id}", a.getNotification)
		r.Post("/notifications/{id}/cancel", a.cancelNotification)
		r.Post("/notifications/{id}/retry", a.retryNotification)
		r.Get("/batches/{id}", a.getBatch)
		r.Post("/batches/{id}/cancel", a.cancelBatch)
		if a.stats != nil {
			r.Get("/stats", a.getStats)
		}
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		a.writeError(w, r, domain.NotFound("no route for "+r.Method+" "+r.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		a.writeError(w, r, errMethodNotAllowed)
	})

	// Outermost, so the span is in the context of every log line. The span is renamed to
	// "METHOD /route/{pattern}" once chi has matched the route (see observe).
	return otelhttp.NewHandler(r, "http.server",
		otelhttp.WithTracerProvider(tracing.Provider),
		otelhttp.WithPropagators(tracing.Propagator),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method }),
		otelhttp.WithFilter(func(r *http.Request) bool {
			switch r.URL.Path {
			case "/healthz", "/readyz", "/metrics":
				return false // probes and scrapes would drown real traces
			}
			return true
		}),
	)
}
