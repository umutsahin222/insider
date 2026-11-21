package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
	"github.com/umutsahin222/insider/internal/events"
)

// wsServer runs the router on a real listener with a hub that the test feeds directly.
func wsServer(t *testing.T) (*httptest.Server, *fakeStore, *events.Hub) {
	t.Helper()
	store := newFakeStore()
	hub := events.NewHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	deps := testDeps(store, nil)
	deps.Stream = hub
	srv := httptest.NewServer(NewRouter(deps))
	t.Cleanup(srv.Close)
	return srv, store, hub
}

func dial(t *testing.T, srv *httptest.Server, query string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/ws?"+query, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func readEvent(t *testing.T, conn *websocket.Conn) domain.StatusEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	ev, err := events.Unmarshal(b)
	if err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return ev
}

// waitSubscribed waits until the handler has registered with the hub.
func waitSubscribed(t *testing.T, hub *events.Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for hub.Subscribers() < n {
		if time.Now().After(deadline) {
			t.Fatalf("subscribers = %d, want %d", hub.Subscribers(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// racingStream changes the stored notification while the handler subscribes, standing in
// for a worker transition whose event was published just before the subscription existed.
type racingStream struct {
	EventStream
	onSubscribe func()
}

func (s racingStream) Subscribe(match func(domain.StatusEvent) bool, buffer int) *events.Subscription {
	s.onSubscribe()
	return s.EventStream.Subscribe(match, buffer)
}

func TestWS_SnapshotIsReadAfterSubscribing(t *testing.T) {
	store := newFakeStore()
	hub := events.NewHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	deps := testDeps(store, nil)
	var id uuid.UUID
	deps.Stream = racingStream{EventStream: hub, onSubscribe: func() {
		store.mu.Lock()
		n := store.byID[id]
		n.Status = domain.StatusSent
		store.byID[id] = n
		store.mu.Unlock()
	}}
	srv := httptest.NewServer(NewRouter(deps))
	t.Cleanup(srv.Close)

	id = decode[notificationResponse](t, do(t, srv.Config.Handler, http.MethodPost, "/v1/notifications", validBody)).ID
	conn := dial(t, srv, "notification_id="+id.String())
	if snap := readEvent(t, conn); snap.Status != domain.StatusSent {
		t.Fatalf("snapshot status = %s, want sent (state after subscribing)", snap.Status)
	}
}

func TestWS_NotificationSnapshotThenLiveEvents(t *testing.T) {
	srv, store, hub := wsServer(t)
	h := srv.Config.Handler
	created := decode[notificationResponse](t, do(t, h, http.MethodPost, "/v1/notifications", validBody))
	n := store.byID[created.ID]
	n.UpdatedAt = n.UpdatedAt.Add(-time.Minute) // the row reached its state a while ago
	store.byID[created.ID] = n

	conn := dial(t, srv, "notification_id="+created.ID.String())
	snap := readEvent(t, conn)
	if snap.NotificationID != created.ID || snap.Status != domain.StatusPending {
		t.Fatalf("snapshot = %+v, want the current pending state", snap)
	}
	// Same clock as the stream's events (the change's database time), so clients can order both.
	if !snap.OccurredAt.Equal(n.UpdatedAt) {
		t.Errorf("snapshot occurred_at = %v, want the row's updated_at %v", snap.OccurredAt, n.UpdatedAt)
	}
	waitSubscribed(t, hub, 1)

	hub.Dispatch(domain.StatusEvent{NotificationID: uuid.New(), Status: domain.StatusSent}) // someone else's
	hub.Dispatch(domain.StatusEvent{NotificationID: n.ID, Status: domain.StatusProcessing, Attempts: 1, StreamID: "5-0"})
	hub.Dispatch(domain.StatusEvent{NotificationID: n.ID, Status: domain.StatusSent, Attempts: 1, StreamID: "6-0"})

	if ev := readEvent(t, conn); ev.Status != domain.StatusProcessing || ev.NotificationID != created.ID {
		t.Errorf("first live event = %+v", ev)
	}
	if ev := readEvent(t, conn); ev.Status != domain.StatusSent || ev.Attempts != 1 {
		t.Errorf("second live event = %+v", ev)
	}
}

func TestWS_BatchSnapshotCoversEveryNotification(t *testing.T) {
	srv, _, hub := wsServer(t)
	batch := decode[batchCreatedResponse](t, do(t, srv.Config.Handler, http.MethodPost, "/v1/notifications/batch",
		`{"items":[`+validBody+`,`+validBody+`,`+validBody+`]}`))

	conn := dial(t, srv, "batch_id="+batch.BatchID.String())
	seen := map[uuid.UUID]bool{}
	for range 3 {
		ev := readEvent(t, conn)
		if ev.BatchID == nil || *ev.BatchID != batch.BatchID {
			t.Fatalf("snapshot event outside the batch: %+v", ev)
		}
		seen[ev.NotificationID] = true
	}
	if len(seen) != 3 {
		t.Errorf("snapshot covered %d notifications, want 3", len(seen))
	}

	waitSubscribed(t, hub, 1)
	b := batch.BatchID
	hub.Dispatch(domain.StatusEvent{NotificationID: batch.NotificationIDs[2], BatchID: &b, Status: domain.StatusFailed})
	if ev := readEvent(t, conn); ev.NotificationID != batch.NotificationIDs[2] || ev.Status != domain.StatusFailed {
		t.Errorf("live event = %+v", ev)
	}
}

func TestWS_DroppedSubscriptionClosesWithTryAgainLater(t *testing.T) {
	srv, _, hub := wsServer(t)
	created := decode[notificationResponse](t, do(t, srv.Config.Handler, http.MethodPost, "/v1/notifications", validBody))
	conn := dial(t, srv, "notification_id="+created.ID.String())
	readEvent(t, conn) // snapshot
	waitSubscribed(t, hub, 1)

	hub.DropAll(events.DropResync) // e.g. a gap in the stream, or API shutdown

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusTryAgainLater {
		t.Errorf("read err = %v, want close status 1013", err)
	}
}

func TestWS_ClientDisconnectUnsubscribes(t *testing.T) {
	srv, _, hub := wsServer(t)
	created := decode[notificationResponse](t, do(t, srv.Config.Handler, http.MethodPost, "/v1/notifications", validBody))
	conn := dial(t, srv, "notification_id="+created.ID.String())
	readEvent(t, conn)
	waitSubscribed(t, hub, 1)

	_ = conn.Close(websocket.StatusNormalClosure, "bye")

	deadline := time.Now().Add(5 * time.Second)
	for hub.Subscribers() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscription leaked after the client disconnected")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWS_RejectsBadRequestsBeforeUpgrading(t *testing.T) {
	srv, _, _ := wsServer(t)
	h := srv.Config.Handler
	tests := map[string]int{
		"":                  http.StatusBadRequest,
		"notification_id=x": http.StatusBadRequest,
		"batch_id=nope":     http.StatusBadRequest,
		"notification_id=" + uuid.NewString() + "&batch_id=" + uuid.NewString(): http.StatusBadRequest,
		"notification_id=" + uuid.NewString():                                   http.StatusNotFound,
		"batch_id=" + uuid.NewString():                                          http.StatusNotFound,
	}
	for q, want := range tests {
		if rec := do(t, h, http.MethodGet, "/v1/ws?"+q, ""); rec.Code != want {
			t.Errorf("?%s: status = %d, want %d", q, rec.Code, want)
		}
	}
}

// The handshake rejections documented for GET /v1/ws in api/openapi.yaml.
func TestWS_HandshakeRejections(t *testing.T) {
	srv, _, _ := wsServer(t)
	id := decode[notificationResponse](t, do(t, srv.Config.Handler, http.MethodPost, "/v1/notifications", validBody)).ID
	url := srv.URL + "/v1/ws?notification_id=" + id.String()

	// A browser from another origin (not in WS_ALLOWED_ORIGINS): 403.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin: err=%v status=%v, want 403", err, statusOf(resp))
	}

	// A plain GET without the upgrade headers: 426.
	plain, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = plain.Body.Close()
	if plain.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("plain GET: status %d, want 426", plain.StatusCode)
	}
}

func statusOf(r *http.Response) any {
	if r == nil {
		return nil
	}
	return r.StatusCode
}

// replayStream answers Replay from a fixed list (or with an error) and fans out live events
// through a real hub.
type replayStream struct {
	*events.Hub
	replay []domain.StatusEvent
	err    error
	after  string // what the handler asked for
}

func (s *replayStream) Replay(_ context.Context, after string, match func(domain.StatusEvent) bool) ([]domain.StatusEvent, error) {
	s.after = after
	if s.err != nil {
		return nil, s.err
	}
	var out []domain.StatusEvent
	for _, e := range s.replay {
		if match(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

func resumeServer(t *testing.T, stream *replayStream) (*httptest.Server, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	deps := testDeps(store, nil)
	deps.Stream = stream
	srv := httptest.NewServer(NewRouter(deps))
	t.Cleanup(srv.Close)
	return srv, store
}

func TestWS_ResumeSendsWhatWasMissedThenOnlyNewEvents(t *testing.T) {
	stream := &replayStream{Hub: events.NewHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)}
	srv, _ := resumeServer(t, stream)
	id := decode[notificationResponse](t, do(t, srv.Config.Handler, http.MethodPost, "/v1/notifications", validBody)).ID
	stream.replay = []domain.StatusEvent{
		{NotificationID: id, Status: domain.StatusProcessing, Attempts: 1, StreamID: "10-0"},
		{NotificationID: uuid.New(), Status: domain.StatusSent, Attempts: 1, StreamID: "11-0"}, // not ours
		{NotificationID: id, Status: domain.StatusPending, Attempts: 1, StreamID: "12-0"},
	}

	conn := dial(t, srv, "notification_id="+id.String()+"&after=9-0")
	if ev := readEvent(t, conn); ev.StreamID != "10-0" || ev.Status != domain.StatusProcessing {
		t.Fatalf("first event = %+v, want the first missed one (no snapshot)", ev)
	}
	if ev := readEvent(t, conn); ev.StreamID != "12-0" {
		t.Fatalf("second event = %+v, want the second missed one", ev)
	}
	if stream.after != "9-0" {
		t.Errorf("replayed after %q, want 9-0", stream.after)
	}

	waitSubscribed(t, stream.Hub, 1)
	// The hub may still deliver events the replay already covered: they are skipped.
	stream.Dispatch(domain.StatusEvent{NotificationID: id, Status: domain.StatusPending, Attempts: 1, StreamID: "12-0"})
	stream.Dispatch(domain.StatusEvent{NotificationID: id, Status: domain.StatusSent, Attempts: 2, StreamID: "13-0"})
	if ev := readEvent(t, conn); ev.StreamID != "13-0" || ev.Status != domain.StatusSent {
		t.Errorf("live event = %+v, want only the new one (13-0)", ev)
	}
}

func TestWS_ResumeFallsBackToASnapshot(t *testing.T) {
	stream := &replayStream{Hub: events.NewHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil), err: events.ErrReplayUnavailable}
	srv, _ := resumeServer(t, stream)
	id := decode[notificationResponse](t, do(t, srv.Config.Handler, http.MethodPost, "/v1/notifications", validBody)).ID

	conn := dial(t, srv, "notification_id="+id.String()+"&after=9-0")
	if ev := readEvent(t, conn); ev.NotificationID != id || ev.Status != domain.StatusPending || ev.StreamID != "" {
		t.Fatalf("first event = %+v, want a snapshot (no event_id) when the replay is unavailable", ev)
	}
	waitSubscribed(t, stream.Hub, 1)
	stream.Dispatch(domain.StatusEvent{NotificationID: id, Status: domain.StatusProcessing, Attempts: 1, StreamID: "5-0"})
	if ev := readEvent(t, conn); ev.StreamID != "5-0" {
		t.Errorf("live event = %+v: after a snapshot nothing is skipped", ev)
	}
}

func TestWS_RejectsAMalformedResumePosition(t *testing.T) {
	srv, _, _ := wsServer(t)
	id := decode[notificationResponse](t, do(t, srv.Config.Handler, http.MethodPost, "/v1/notifications", validBody)).ID
	rec := do(t, srv.Config.Handler, http.MethodGet, "/v1/ws?notification_id="+id.String()+"&after=yesterday", "")
	if p := decode[problem](t, rec); rec.Code != http.StatusBadRequest || len(p.Errors) != 1 || p.Errors[0].Field != "after" {
		t.Errorf("status = %d, problem = %+v; want 400 on after", rec.Code, p)
	}
}
