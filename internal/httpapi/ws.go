package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
	"github.com/umutsahin222/insider/internal/events"
)

// EventStream is the local fan-out of status events; implemented by events.Hub.
type EventStream interface {
	Subscribe(match func(domain.StatusEvent) bool, buffer int) *events.Subscription
	// Replay returns the events after an event ID, or events.ErrReplayUnavailable when the
	// stream cannot prove it still holds all of them.
	Replay(ctx context.Context, after string, match func(domain.StatusEvent) bool) ([]domain.StatusEvent, error)
}

const (
	wsWriteTimeout = 5 * time.Second
	wsPingInterval = 30 * time.Second
	// Event buffers per connection. A batch of 1000 produces a few thousand events at the
	// rate limit; the client must keep up or it is dropped and resynchronises.
	wsBufferNotification = 32
	wsBufferBatch        = 1024
)

// subscribeStatus streams status events for one notification or one batch:
//
//  1. subscribe to the hub first, then
//  2. send what the client missed: the events after `after` (its last event_id) if the
//     stream still holds them all, otherwise a snapshot of the current state from the
//     database, then
//  3. forward live events.
//
// Subscribing first leaves no gap: an event raced with step 2 arrives twice at worst, and a
// live event a replay already sent is skipped. If the subscription is dropped (slow client,
// a gap in the stream, API shutdown) the socket closes with 1013 "try again later" and the
// client reconnects, with `after` set to resume (ADR-012).
func (a *api) subscribeStatus(w http.ResponseWriter, r *http.Request) {
	match, buffer, snapshot, err := a.parseSubscription(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	after := r.URL.Query().Get("after")
	if after != "" && !events.ParseStreamID(after) {
		a.writeError(w, r, &badRequestError{msg: "invalid query parameters",
			fields: []domain.FieldError{{Field: "after", Message: "must be an event_id from this stream"}}})
		return
	}
	sub := a.stream.Subscribe(match, buffer)
	defer sub.Close()

	var initial []domain.StatusEvent
	replayed := false
	if after != "" {
		initial, err = a.stream.Replay(r.Context(), after, match)
		replayed = err == nil
	}
	if !replayed {
		if initial, err = snapshot(r.Context()); err != nil {
			a.writeError(w, r, err)
			return
		}
	}
	// Live events up to the last one already sent are duplicates of the replay.
	sentUpTo := after
	if n := len(initial); replayed && n > 0 {
		sentUpTo = initial[n-1].StreamID
	}

	// The server's read/write timeouts are meant for ordinary requests; a WebSocket lives
	// for as long as the client watches.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: a.wsOrigins})
	if err != nil {
		a.log.WarnContext(r.Context(), "websocket upgrade failed", "error", err)
		return // Accept has already written the HTTP error
	}
	defer func() { _ = conn.CloseNow() }()
	a.metrics.WebSocketConnections(1)
	defer a.metrics.WebSocketConnections(-1)

	// We never expect messages from the client; CloseRead handles control frames and
	// cancels ctx when the client goes away.
	ctx := conn.CloseRead(r.Context())

	for _, ev := range initial {
		if err := writeEvent(ctx, conn, ev); err != nil {
			return
		}
	}

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.Events():
			if !ok {
				_ = conn.Close(websocket.StatusTryAgainLater, "subscription dropped; reconnect to resynchronise")
				return
			}
			if replayed && !events.StreamIDAfter(ev.StreamID, sentUpTo) {
				continue
			}
			if err := writeEvent(ctx, conn, ev); err != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func writeEvent(ctx context.Context, conn *websocket.Conn, ev domain.StatusEvent) error {
	b, err := events.Marshal(ev)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}

// parseSubscription validates the query (exactly one of notification_id / batch_id), checks
// that the target exists — before upgrading, so errors are ordinary HTTP responses — and
// returns the event filter and a snapshot loader.
func (a *api) parseSubscription(r *http.Request) (func(domain.StatusEvent) bool, int, func(context.Context) ([]domain.StatusEvent, error), error) {
	q := r.URL.Query()
	nID, bID := q.Get("notification_id"), q.Get("batch_id")
	bad := func(msg string) error {
		return &badRequestError{msg: "invalid query parameters", fields: []domain.FieldError{{Field: "notification_id", Message: msg}}}
	}
	if (nID == "") == (bID == "") {
		return nil, 0, nil, bad("exactly one of notification_id and batch_id is required")
	}

	if nID != "" {
		id, err := uuid.Parse(nID)
		if err != nil {
			return nil, 0, nil, bad("must be a UUID")
		}
		if _, err := a.store.GetNotification(r.Context(), id); err != nil {
			return nil, 0, nil, err
		}
		match := func(e domain.StatusEvent) bool { return e.NotificationID == id }
		// Read again for the snapshot: the existence check above ran before subscribing, and
		// a transition published in between would otherwise be missing from both.
		snapshot := func(ctx context.Context) ([]domain.StatusEvent, error) {
			n, err := a.store.GetNotification(ctx, id)
			if err != nil {
				return nil, err
			}
			return []domain.StatusEvent{domain.EventOf(n)}, nil
		}
		return match, wsBufferNotification, snapshot, nil
	}

	id, err := uuid.Parse(bID)
	if err != nil {
		return nil, 0, nil, &badRequestError{msg: "invalid query parameters", fields: []domain.FieldError{{Field: "batch_id", Message: "must be a UUID"}}}
	}
	if _, err := a.store.BatchSummary(r.Context(), id); err != nil {
		return nil, 0, nil, err
	}
	match := func(e domain.StatusEvent) bool { return e.BatchID != nil && *e.BatchID == id }
	snapshot := func(ctx context.Context) ([]domain.StatusEvent, error) { return a.batchSnapshot(ctx, id) }
	return match, wsBufferBatch, snapshot, nil
}

// batchSnapshot reads the batch's current state page by page.
func (a *api) batchSnapshot(ctx context.Context, batchID uuid.UUID) ([]domain.StatusEvent, error) {
	var (
		out []domain.StatusEvent
		f   = domain.ListFilter{BatchID: &batchID, Limit: maxPageSize}
	)
	for {
		page, err := a.store.ListNotifications(ctx, f)
		if err != nil {
			return nil, err
		}
		for _, n := range page.Items {
			out = append(out, domain.EventOf(n))
		}
		if page.Next == nil {
			return out, nil
		}
		f.After = page.Next
		if len(out) > a.rules.MaxBatchSize { // cannot happen for a valid batch; never loop unbounded
			return nil, errors.New("batch snapshot exceeds MAX_BATCH_SIZE")
		}
	}
}
