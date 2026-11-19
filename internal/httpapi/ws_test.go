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
	hub := events.NewHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func TestWS_NotificationSnapshotThenLiveEvents(t *testing.T) {
	srv, store, hub := wsServer(t)
	h := srv.Config.Handler
	created := decode[notificationResponse](t, do(t, h, http.MethodPost, "/v1/notifications", validBody))
	n := store.byID[created.ID]

	conn := dial(t, srv, "notification_id="+created.ID.String())
	if snap := readEvent(t, conn); snap.NotificationID != created.ID || snap.Status != domain.StatusPending {
		t.Fatalf("snapshot = %+v, want the current pending state", snap)
	}
	waitSubscribed(t, hub, 1)

	hub.Dispatch(domain.StatusEvent{NotificationID: uuid.New(), Status: domain.StatusSent}) // someone else's
	hub.Dispatch(n.Transitioned(domain.StatusProcessing, nil, time.Now()))
	n.Attempts = 1
	hub.Dispatch(n.Transitioned(domain.StatusSent, nil, time.Now()))

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

	hub.DropAll() // e.g. Redis reconnect or API shutdown

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
