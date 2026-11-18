// Package httpapi exposes the notification service over HTTP (see api/openapi.yaml).
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
)

// NotificationStore is the persistence the API needs; implemented by store/postgres.
type NotificationStore interface {
	CreateNotification(ctx context.Context, n domain.Notification) (domain.Notification, bool, error)
	CreateBatch(ctx context.Context, b domain.Batch) (domain.Batch, bool, error)
	GetNotification(ctx context.Context, id uuid.UUID) (domain.Notification, error)
	ListAttempts(ctx context.Context, id uuid.UUID) ([]domain.DeliveryAttempt, error)
	ListNotifications(ctx context.Context, f domain.ListFilter) (domain.Page, error)
	CancelNotification(ctx context.Context, id uuid.UUID) (domain.Notification, error)
	BatchSummary(ctx context.Context, batchID uuid.UUID) (domain.BatchSummary, error)
	CancelBatch(ctx context.Context, batchID uuid.UUID) (int, error)
}

// Check reports whether a dependency is healthy; used by /readyz.
type Check func(ctx context.Context) error

// Deps are the explicit dependencies of the HTTP API.
type Deps struct {
	Store  NotificationStore
	Rules  domain.Rules
	Logger *slog.Logger
	// Checks are run by /readyz, keyed by dependency name (e.g. "postgres").
	Checks map[string]Check
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
}

const (
	maxSingleBody = 1 << 20  // 1 MiB
	maxBatchBody  = 16 << 20 // 16 MiB: 1000 items × up to 10k-char emails
)

type api struct {
	store  NotificationStore
	rules  domain.Rules
	log    *slog.Logger
	checks map[string]Check
	now    func() time.Time
}

// NewRouter builds the HTTP handler with all routes and middleware.
func NewRouter(d Deps) http.Handler {
	a := &api{store: d.Store, rules: d.Rules, log: d.Logger, checks: d.Checks, now: d.Now}
	if a.now == nil {
		a.now = time.Now
	}

	r := chi.NewRouter()
	r.Use(correlation, a.requestLog, a.recoverer)

	r.Get("/healthz", a.liveness)
	r.Get("/readyz", a.readiness)

	r.Route("/v1", func(r chi.Router) {
		r.With(maxBody(maxSingleBody)).Post("/notifications", a.createNotification)
		r.With(maxBody(maxBatchBody)).Post("/notifications/batch", a.createBatch)
		r.Get("/notifications", a.listNotifications)
		r.Get("/notifications/{id}", a.getNotification)
		r.Post("/notifications/{id}/cancel", a.cancelNotification)
		r.Get("/batches/{id}", a.getBatch)
		r.Post("/batches/{id}/cancel", a.cancelBatch)
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		a.writeError(w, r, domain.NotFound("no route for "+r.Method+" "+r.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		a.writeError(w, r, errMethodNotAllowed)
	})
	return r
}
