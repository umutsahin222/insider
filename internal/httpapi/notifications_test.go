package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
)

// fakeStore is an in-memory NotificationStore that mimics the store's contract.
type fakeStore struct {
	mu      sync.Mutex
	byID    map[uuid.UUID]domain.Notification
	byKey   map[string]uuid.UUID
	batches map[string]domain.Batch // by batch idempotency key
	lastLF  domain.ListFilter
	listErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{byID: map[uuid.UUID]domain.Notification{}, byKey: map[string]uuid.UUID{}, batches: map[string]domain.Batch{}}
}

func (s *fakeStore) CreateNotification(_ context.Context, n domain.Notification) (domain.Notification, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n.IdempotencyKey != nil {
		if id, ok := s.byKey[*n.IdempotencyKey]; ok {
			return s.byID[id], false, nil
		}
		s.byKey[*n.IdempotencyKey] = n.ID
	}
	s.byID[n.ID] = n
	return n, true, nil
}

func (s *fakeStore) CreateBatch(_ context.Context, b domain.Batch) (domain.Batch, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.IdempotencyKey != nil {
		if existing, ok := s.batches[*b.IdempotencyKey]; ok {
			return existing, false, nil
		}
	}
	for _, n := range b.Notifications {
		if n.IdempotencyKey != nil {
			if _, ok := s.byKey[*n.IdempotencyKey]; ok {
				return domain.Batch{}, false, domain.Conflict("idempotency key already used")
			}
		}
	}
	for _, n := range b.Notifications {
		s.byID[n.ID] = n
		if n.IdempotencyKey != nil {
			s.byKey[*n.IdempotencyKey] = n.ID
		}
	}
	if b.IdempotencyKey != nil {
		s.batches[*b.IdempotencyKey] = b
	}
	return b, true, nil
}

func (s *fakeStore) GetNotification(_ context.Context, id uuid.UUID) (domain.Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.byID[id]
	if !ok {
		return n, domain.NotFound("notification not found")
	}
	return n, nil
}

func (s *fakeStore) ListAttempts(context.Context, uuid.UUID) ([]domain.DeliveryAttempt, error) {
	code := 503
	return []domain.DeliveryAttempt{{Attempt: 1, Outcome: domain.OutcomeRetryableError, StatusCode: &code, Duration: 120 * time.Millisecond}}, nil
}

func (s *fakeStore) ListNotifications(_ context.Context, f domain.ListFilter) (domain.Page, error) {
	s.lastLF = f
	if s.listErr != nil {
		return domain.Page{}, s.listErr
	}
	return domain.Page{Next: &domain.Cursor{CreatedAt: time.UnixMicro(1_700_000_000_000_001), ID: uuid.MustParse("01890000-0000-7000-8000-000000000001")}}, nil
}

func (s *fakeStore) CancelNotification(_ context.Context, id uuid.UUID) (domain.Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.byID[id]
	if !ok {
		return n, domain.NotFound("notification not found")
	}
	if n.Status != domain.StatusPending {
		return domain.Notification{}, domain.Conflict("notification is " + string(n.Status))
	}
	n.Status = domain.StatusCancelled
	s.byID[id] = n
	return n, nil
}

func (s *fakeStore) BatchSummary(_ context.Context, id uuid.UUID) (domain.BatchSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := domain.BatchSummary{BatchID: id, Counts: map[domain.Status]int{}}
	for _, n := range s.byID {
		if n.BatchID != nil && *n.BatchID == id {
			sum.Total++
			sum.Counts[n.Status]++
		}
	}
	if sum.Total == 0 {
		return sum, domain.NotFound("batch not found")
	}
	return sum, nil
}

func (s *fakeStore) CancelBatch(context.Context, uuid.UUID) (int, error) { return 0, nil }

var fixedNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func newTestServer(t *testing.T, store *fakeStore, checks map[string]Check) http.Handler {
	t.Helper()
	return NewRouter(testDeps(store, checks))
}

func testDeps(store *fakeStore, checks map[string]Check) Deps {
	return Deps{
		Store:  store,
		Rules:  domain.DefaultRules(),
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Checks: checks,
		Now:    func() time.Time { return fixedNow },
	}
}

// recordingMetrics captures what the API reports.
type recordingMetrics struct {
	mu       sync.Mutex
	requests []string // "<method> <route> <status>"
	accepted map[string]int
}

func (m *recordingMetrics) ObserveRequest(method, route string, status int, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, method+" "+route+" "+strconv.Itoa(status))
}

func (m *recordingMetrics) NotificationAccepted(ch domain.Channel, p domain.Priority) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accepted == nil {
		m.accepted = map[string]int{}
	}
	m.accepted[string(ch)+"/"+p.String()]++
}

func TestMetrics(t *testing.T) {
	m := &recordingMetrics{}
	deps := testDeps(newFakeStore(), nil)
	deps.Metrics = m
	deps.MetricsHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "# metrics") })
	h := NewRouter(deps)

	created := decode[notificationResponse](t, do(t, h, http.MethodPost, "/v1/notifications", validBody, "Idempotency-Key", "m-1"))
	do(t, h, http.MethodPost, "/v1/notifications", validBody, "Idempotency-Key", "m-1") // replay: not counted
	do(t, h, http.MethodPost, "/v1/notifications/batch",
		`{"items":[{"channel":"push","recipient":"tok","content":"a","priority":"low"},{"channel":"push","recipient":"tok","content":"b","priority":"low"}]}`)
	do(t, h, http.MethodGet, "/v1/notifications/"+created.ID.String(), "")
	do(t, h, http.MethodGet, "/nope", "")
	if rec := do(t, h, http.MethodGet, "/metrics", ""); rec.Code != http.StatusOK || rec.Body.String() != "# metrics" {
		t.Errorf("/metrics = %d %q", rec.Code, rec.Body)
	}

	if m.accepted["sms/high"] != 1 || m.accepted["push/low"] != 2 || len(m.accepted) != 2 {
		t.Errorf("accepted = %v, want sms/high:1 push/low:2", m.accepted)
	}
	want := []string{
		"POST /v1/notifications 201",
		"POST /v1/notifications 200",
		"POST /v1/notifications/batch 201",
		"GET /v1/notifications/{id} 200", // the pattern, never the raw ID
		"GET unmatched 404",
		"GET /metrics 200",
	}
	if strings.Join(m.requests, "|") != strings.Join(want, "|") {
		t.Errorf("requests =\n%v\nwant\n%v", m.requests, want)
	}
}

func do(t *testing.T, h http.Handler, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

const validBody = `{"channel":"sms","recipient":"+905551234567","content":"hello","priority":"high"}`

func TestCreateNotification(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantFields []string
	}{
		{"valid", validBody, http.StatusCreated, nil},
		{"malformed json", `{"channel":`, http.StatusBadRequest, nil},
		{"unknown field", `{"channel":"sms","recipient":"+905551234567","content":"x","priorty":"high"}`, http.StatusBadRequest, nil},
		{"trailing data", validBody + `{}`, http.StatusBadRequest, nil},
		{"bad timestamp", `{"channel":"sms","recipient":"+905551234567","content":"x","scheduled_at":"tomorrow"}`, http.StatusBadRequest, nil},
		{"validation errors", `{"channel":"sms","recipient":"123","content":""}`, http.StatusUnprocessableEntity, []string{"recipient", "content"}},
		{"too large", `{"channel":"email","recipient":"a@b.co","content":"` + strings.Repeat("x", maxSingleBody) + `"}`, http.StatusRequestEntityTooLarge, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestServer(t, newFakeStore(), nil)
			rec := do(t, h, http.MethodPost, "/v1/notifications", tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantStatus >= 400 {
				if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
					t.Errorf("Content-Type = %q", ct)
				}
				p := decode[problem](t, rec)
				if p.Status != tt.wantStatus || p.CorrelationID == "" {
					t.Errorf("problem = %+v", p)
				}
				var got []string
				for _, fe := range p.Errors {
					got = append(got, fe.Field)
				}
				if strings.Join(got, ",") != strings.Join(tt.wantFields, ",") {
					t.Errorf("fields = %v, want %v", got, tt.wantFields)
				}
				return
			}
			n := decode[notificationResponse](t, rec)
			if n.Status != "pending" || n.Priority != "high" || n.Channel != "sms" {
				t.Errorf("body = %+v", n)
			}
			if rec.Header().Get("Location") != "/v1/notifications/"+n.ID.String() {
				t.Errorf("Location = %q", rec.Header().Get("Location"))
			}
		})
	}
}

func TestCreateNotification_IdempotentReplay(t *testing.T) {
	h := newTestServer(t, newFakeStore(), nil)

	first := do(t, h, http.MethodPost, "/v1/notifications", validBody, "Idempotency-Key", "order-1")
	second := do(t, h, http.MethodPost, "/v1/notifications", validBody, "Idempotency-Key", "order-1")

	if first.Code != http.StatusCreated || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d, %d; want 201, 200", first.Code, second.Code)
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("missing Idempotent-Replayed header")
	}
	if a, b := decode[notificationResponse](t, first), decode[notificationResponse](t, second); a.ID != b.ID {
		t.Errorf("replay returned a different notification: %s vs %s", a.ID, b.ID)
	}
}

func TestCreateNotification_IdempotencyKeyReusedWithDifferentBody(t *testing.T) {
	h := newTestServer(t, newFakeStore(), nil)

	first := do(t, h, http.MethodPost, "/v1/notifications", validBody, "Idempotency-Key", "order-2")
	other := strings.Replace(validBody, `"priority":"high"`, `"priority":"low"`, 1)
	if other == validBody {
		t.Fatal("test body was not changed")
	}
	second := do(t, h, http.MethodPost, "/v1/notifications", other, "Idempotency-Key", "order-2")

	if first.Code != http.StatusCreated || second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("statuses = %d, %d; want 201, 422", first.Code, second.Code)
	}
	if p := decode[problem](t, second); p.Title != "Idempotency key reused" {
		t.Errorf("problem = %+v", p)
	}
	if second.Header().Get("Idempotent-Replayed") != "" {
		t.Error("a rejected reuse must not be marked as a replay")
	}
}

func TestCorrelationID(t *testing.T) {
	h := newTestServer(t, newFakeStore(), nil)

	t.Run("propagated from header", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, "/v1/notifications", validBody, "X-Correlation-ID", "abc-123")
		if got := rec.Header().Get("X-Correlation-ID"); got != "abc-123" {
			t.Errorf("echoed = %q", got)
		}
		if n := decode[notificationResponse](t, rec); n.CorrelationID != "abc-123" {
			t.Errorf("stored correlation_id = %q", n.CorrelationID)
		}
	})
	t.Run("generated when missing or unsafe", func(t *testing.T) {
		for _, hdr := range []string{"", "bad id\nwith newline", strings.Repeat("x", 200)} {
			rec := do(t, h, http.MethodGet, "/healthz", "", "X-Correlation-ID", hdr)
			if _, err := uuid.Parse(rec.Header().Get("X-Correlation-ID")); err != nil {
				t.Errorf("header %q: expected generated UUID, got %q", hdr, rec.Header().Get("X-Correlation-ID"))
			}
		}
	})
}

func TestCreateBatch(t *testing.T) {
	store := newFakeStore()
	h := newTestServer(t, store, nil)

	body := `{"items":[` + validBody + `,{"channel":"email","recipient":"a@example.com","content":"hi","idempotency_key":"k1"}]}`
	rec := do(t, h, http.MethodPost, "/v1/notifications/batch", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	resp := decode[batchCreatedResponse](t, rec)
	if resp.Count != 2 || len(resp.NotificationIDs) != 2 {
		t.Fatalf("resp = %+v", resp)
	}

	sum := decode[batchSummaryResponse](t, do(t, h, http.MethodGet, "/v1/batches/"+resp.BatchID.String(), ""))
	if sum.Total != 2 || sum.Counts["pending"] != 2 || sum.Counts["sent"] != 0 {
		t.Errorf("summary = %+v", sum)
	}

	t.Run("indexed validation errors", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, "/v1/notifications/batch", `{"items":[`+validBody+`,{"channel":"fax","recipient":"x","content":"x"}]}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d", rec.Code)
		}
		if p := decode[problem](t, rec); len(p.Errors) != 1 || p.Errors[0].Field != "items[1].channel" {
			t.Errorf("errors = %+v", p.Errors)
		}
	})
	t.Run("batch idempotency key", func(t *testing.T) {
		keyed := `{"items":[` + validBody + `,{"channel":"push","recipient":"tok","content":"hi"}]}`
		first := do(t, h, http.MethodPost, "/v1/notifications/batch", keyed, "Idempotency-Key", "batch-1")
		replay := do(t, h, http.MethodPost, "/v1/notifications/batch", keyed, "Idempotency-Key", "batch-1")
		if first.Code != http.StatusCreated || replay.Code != http.StatusOK {
			t.Fatalf("statuses = %d, %d; want 201, 200", first.Code, replay.Code)
		}
		if replay.Header().Get("Idempotent-Replayed") != "true" {
			t.Error("missing Idempotent-Replayed header")
		}
		a, b := decode[batchCreatedResponse](t, first), decode[batchCreatedResponse](t, replay)
		if a.BatchID != b.BatchID || len(b.NotificationIDs) != 2 || a.NotificationIDs[1] != b.NotificationIDs[1] {
			t.Errorf("replay returned a different batch: %+v vs %+v", a, b)
		}

		changed := `{"items":[` + validBody + `]}`
		if rec := do(t, h, http.MethodPost, "/v1/notifications/batch", changed, "Idempotency-Key", "batch-1"); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("reused key with different items: status = %d, want 422", rec.Code)
		}
	})
	t.Run("existing item key rejects the batch", func(t *testing.T) {
		dup := `{"items":[{"channel":"email","recipient":"b@example.com","content":"hi","idempotency_key":"k1"}]}`
		if rec := do(t, h, http.MethodPost, "/v1/notifications/batch", dup); rec.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409", rec.Code)
		}
	})
	t.Run("empty batch", func(t *testing.T) {
		if rec := do(t, h, http.MethodPost, "/v1/notifications/batch", `{"items":[]}`); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d", rec.Code)
		}
	})
}

func TestGetAndCancel(t *testing.T) {
	store := newFakeStore()
	h := newTestServer(t, store, nil)
	created := decode[notificationResponse](t, do(t, h, http.MethodPost, "/v1/notifications", validBody))
	path := "/v1/notifications/" + created.ID.String()

	rec := do(t, h, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	detail := decode[notificationDetailResponse](t, rec)
	if detail.ID != created.ID || len(detail.DeliveryAttempts) != 1 || detail.DeliveryAttempts[0].DurationMs != 120 {
		t.Errorf("detail = %+v", detail)
	}

	if rec := do(t, h, http.MethodPost, path+"/cancel", ""); rec.Code != http.StatusOK || decode[notificationResponse](t, rec).Status != "cancelled" {
		t.Errorf("cancel: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodPost, path+"/cancel", ""); rec.Code != http.StatusConflict {
		t.Errorf("second cancel status = %d, want 409", rec.Code)
	}

	for _, p := range []string{"/v1/notifications/" + uuid.NewString(), "/v1/notifications/not-a-uuid"} {
		if rec := do(t, h, http.MethodGet, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
}

func TestListNotifications_QueryParsing(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantStatus int
		check      func(t *testing.T, f domain.ListFilter)
	}{
		{"defaults", "", http.StatusOK, func(t *testing.T, f domain.ListFilter) {
			if f.Limit != defaultPageSize || f.Status != nil || f.After != nil {
				t.Errorf("filter = %+v", f)
			}
		}},
		{"all filters", "?status=failed&channel=email&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z&limit=10", http.StatusOK,
			func(t *testing.T, f domain.ListFilter) {
				if *f.Status != domain.StatusFailed || *f.Channel != domain.ChannelEmail || f.Limit != 10 || f.From == nil || f.To == nil {
					t.Errorf("filter = %+v", f)
				}
			}},
		{"bad values", "?status=done&channel=fax&limit=0&from=yesterday&cursor=%%%", http.StatusBadRequest, nil},
		{"limit too large", "?limit=201", http.StatusBadRequest, nil},
		{"inverted range", "?from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z", http.StatusBadRequest, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			rec := do(t, newTestServer(t, store, nil), http.MethodGet, "/v1/notifications"+tt.query, "")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.check != nil {
				tt.check(t, store.lastLF)
			}
		})
	}

	t.Run("bad values are all reported", func(t *testing.T) {
		rec := do(t, newTestServer(t, newFakeStore(), nil), http.MethodGet, "/v1/notifications?status=done&channel=fax&limit=0", "")
		if p := decode[problem](t, rec); len(p.Errors) != 3 {
			t.Errorf("errors = %+v", p.Errors)
		}
	})
}

func TestListNotifications_CursorRoundTrip(t *testing.T) {
	store := newFakeStore()
	h := newTestServer(t, store, nil)

	page := decode[notificationPageResponse](t, do(t, h, http.MethodGet, "/v1/notifications", ""))
	if page.NextCursor == nil {
		t.Fatal("expected next_cursor")
	}
	do(t, h, http.MethodGet, "/v1/notifications?cursor="+*page.NextCursor, "")
	after := store.lastLF.After
	if after == nil || after.CreatedAt.UnixMicro() != 1_700_000_000_000_001 || after.ID.String() != "01890000-0000-7000-8000-000000000001" {
		t.Errorf("decoded cursor = %+v", after)
	}
}

func TestInternalErrorsAreNotLeaked(t *testing.T) {
	store := newFakeStore()
	store.listErr = errors.New("pq: password authentication failed for user secret")
	rec := do(t, newTestServer(t, store, nil), http.MethodGet, "/v1/notifications", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("secret")) {
		t.Errorf("internal error leaked: %s", rec.Body)
	}
}

func TestHealth(t *testing.T) {
	ok := func(context.Context) error { return nil }
	down := func(context.Context) error { return errors.New("connection refused") }

	h := newTestServer(t, newFakeStore(), map[string]Check{"postgres": ok, "redis": down})
	if rec := do(t, h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("healthz = %d", rec.Code)
	}
	rec := do(t, h, http.MethodGet, "/readyz", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503", rec.Code)
	}
	body := decode[map[string]any](t, rec)
	checks := body["checks"].(map[string]any)
	if checks["postgres"] != "ok" || checks["redis"] != "unavailable" {
		t.Errorf("checks = %v", checks)
	}

	h = newTestServer(t, newFakeStore(), map[string]Check{"postgres": ok})
	if rec := do(t, h, http.MethodGet, "/readyz", ""); rec.Code != http.StatusOK {
		t.Errorf("readyz healthy = %d", rec.Code)
	}
}

func TestRouting(t *testing.T) {
	h := newTestServer(t, newFakeStore(), nil)
	if rec := do(t, h, http.MethodGet, "/nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown route = %d", rec.Code)
	}
	if rec := do(t, h, http.MethodDelete, "/v1/notifications", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method = %d", rec.Code)
	}
}
