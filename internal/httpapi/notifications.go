package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
	"github.com/umutsahin222/insider/internal/observability"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
	readyTimeout    = 2 * time.Second
)

func (a *api) createNotification(w http.ResponseWriter, r *http.Request) {
	var req createNotificationRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeError(w, r, err)
		return
	}
	ctx := r.Context()
	in := req.input(r.Header.Get("Idempotency-Key"), observability.CorrelationID(ctx))

	n, err := domain.NewNotification(in, a.rules, a.now())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	stored, created, err := a.store.CreateNotification(ctx, n)
	if err != nil {
		a.writeError(w, r, err)
		return
	}

	ctx = observability.WithNotificationID(ctx, stored.ID.String())
	if !created {
		if err := domain.VerifyReplay(stored.RequestHash, n.RequestHash); err != nil {
			a.log.WarnContext(ctx, "idempotency key reused with a different request", "idempotency_key", in.IdempotencyKey)
			a.writeError(w, r, err)
			return
		}
		a.log.InfoContext(ctx, "idempotent replay", "idempotency_key", in.IdempotencyKey)
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, toNotificationResponse(stored))
		return
	}
	a.metrics.NotificationAccepted(stored.Channel, stored.Priority)
	a.log.InfoContext(ctx, "notification accepted", "channel", stored.Channel, "priority", stored.Priority.String())
	w.Header().Set("Location", "/v1/notifications/"+stored.ID.String())
	writeJSON(w, http.StatusCreated, toNotificationResponse(stored))
}

func (a *api) createBatch(w http.ResponseWriter, r *http.Request) {
	var req createBatchRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeError(w, r, err)
		return
	}
	corrID := observability.CorrelationID(r.Context())
	inputs := make([]domain.CreateInput, len(req.Items))
	for i, item := range req.Items {
		inputs[i] = item.input(item.IdempotencyKey, corrID)
	}

	key := r.Header.Get("Idempotency-Key")
	batch, err := domain.NewBatch(inputs, key, a.rules, a.now())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	stored, created, err := a.store.CreateBatch(r.Context(), batch)
	if err != nil {
		a.writeError(w, r, err)
		return
	}

	resp := toBatchCreatedResponse(stored)
	if !created {
		if err := domain.VerifyReplay(stored.RequestHash, batch.RequestHash); err != nil {
			a.log.WarnContext(r.Context(), "idempotency key reused with a different batch", "idempotency_key", key)
			a.writeError(w, r, err)
			return
		}
		a.log.InfoContext(r.Context(), "idempotent batch replay", "batch_id", stored.ID, "idempotency_key", key)
		w.Header().Set("Idempotent-Replayed", "true")
		w.Header().Set("Location", "/v1/batches/"+stored.ID.String())
		writeJSON(w, http.StatusOK, resp)
		return
	}
	for _, n := range stored.Notifications {
		a.metrics.NotificationAccepted(n.Channel, n.Priority)
	}
	a.log.InfoContext(r.Context(), "batch accepted", "batch_id", stored.ID, "count", resp.Count)
	w.Header().Set("Location", "/v1/batches/"+stored.ID.String())
	writeJSON(w, http.StatusCreated, resp)
}

func (a *api) getNotification(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(r)
	if !ok {
		a.writeError(w, r, domain.NotFound("notification not found"))
		return
	}
	n, err := a.store.GetNotification(r.Context(), id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	attempts, err := a.store.ListAttempts(r.Context(), id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	resp := notificationDetailResponse{
		notificationResponse: toNotificationResponse(n),
		DeliveryAttempts:     make([]deliveryAttemptResponse, len(attempts)),
	}
	for i, at := range attempts {
		resp.DeliveryAttempts[i] = deliveryAttemptResponse{
			Attempt: at.Attempt, Outcome: string(at.Outcome), StatusCode: at.StatusCode, Error: at.Error,
			DurationMs: at.Duration.Milliseconds(), CreatedAt: at.CreatedAt.UTC(),
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *api) listNotifications(w http.ResponseWriter, r *http.Request) {
	f, err := parseListFilter(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	page, err := a.store.ListNotifications(r.Context(), f)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	resp := notificationPageResponse{Items: make([]notificationResponse, len(page.Items))}
	for i, n := range page.Items {
		resp.Items[i] = toNotificationResponse(n)
	}
	if page.Next != nil {
		c := encodeCursor(*page.Next)
		resp.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseListFilter validates query parameters, reporting every bad one at once.
func parseListFilter(r *http.Request) (domain.ListFilter, error) {
	q := r.URL.Query()
	f := domain.ListFilter{Limit: defaultPageSize}
	bad := &badRequestError{msg: "invalid query parameters"}
	fail := func(field, msg string) {
		bad.fields = append(bad.fields, domain.FieldError{Field: field, Message: msg})
	}

	if v := q.Get("status"); v != "" {
		if s, err := domain.ParseStatus(v); err != nil {
			fail("status", "must be one of pending, processing, sent, failed, cancelled")
		} else {
			f.Status = &s
		}
	}
	if v := q.Get("channel"); v != "" {
		if c, err := domain.ParseChannel(v); err != nil {
			fail("channel", "must be one of sms, email, push")
		} else {
			f.Channel = &c
		}
	}
	if v := q.Get("batch_id"); v != "" {
		if id, err := uuid.Parse(v); err != nil {
			fail("batch_id", "must be a UUID")
		} else {
			f.BatchID = &id
		}
	}
	for _, p := range []struct {
		name string
		dst  **time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		if v := q.Get(p.name); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err != nil {
				fail(p.name, "must be an RFC 3339 timestamp, e.g. 2026-11-18T00:00:00Z")
			} else {
				*p.dst = &t
			}
		}
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		fail("to", "must be after from")
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > maxPageSize {
			fail("limit", "must be an integer between 1 and "+strconv.Itoa(maxPageSize))
		} else {
			f.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		if c, ok := decodeCursor(v); !ok {
			fail("cursor", "is not a valid cursor")
		} else {
			f.After = &c
		}
	}
	if len(bad.fields) > 0 {
		return domain.ListFilter{}, bad
	}
	return f, nil
}

func (a *api) cancelNotification(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(r)
	if !ok {
		a.writeError(w, r, domain.NotFound("notification not found"))
		return
	}
	n, err := a.store.CancelNotification(r.Context(), id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.log.InfoContext(observability.WithNotificationID(r.Context(), id.String()), "notification cancelled")
	writeJSON(w, http.StatusOK, toNotificationResponse(n))
}

func (a *api) getBatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(r)
	if !ok {
		a.writeError(w, r, domain.NotFound("batch not found"))
		return
	}
	sum, err := a.store.BatchSummary(r.Context(), id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBatchSummaryResponse(sum))
}

func (a *api) cancelBatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(r)
	if !ok {
		a.writeError(w, r, domain.NotFound("batch not found"))
		return
	}
	n, err := a.store.CancelBatch(r.Context(), id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.log.InfoContext(r.Context(), "batch cancelled", "batch_id", id, "cancelled", n)
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": id, "cancelled": n})
}

func (a *api) liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *api) readiness(w http.ResponseWriter, r *http.Request) {
	status, code := "ok", http.StatusOK
	results := make(map[string]string, len(a.checks))
	for name, check := range a.checks {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		err := check(ctx)
		cancel()
		if err != nil {
			a.log.WarnContext(r.Context(), "readiness check failed", "dependency", name, "error", err)
			results[name], status, code = "unavailable", "unavailable", http.StatusServiceUnavailable
			continue
		}
		results[name] = "ok"
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": results})
}

// pathUUID parses {id}. A malformed ID cannot match any resource, so callers answer 404.
func pathUUID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	return id, err == nil
}
