// Package events carries notification status events over Redis pub/sub: workers and the
// API publish, and every API process runs one Hub that fans events out to its local
// subscribers (WebSocket connections). See ADR-008.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/umutsahin222/insider/internal/domain"
)

// Channel is the Redis pub/sub channel all status events go through.
const Channel = "notification.events"

// wireEvent is the JSON form, shared by Redis and WebSocket (api/openapi.yaml StatusEvent).
type wireEvent struct {
	NotificationID uuid.UUID     `json:"notification_id"`
	BatchID        *uuid.UUID    `json:"batch_id"`
	Status         domain.Status `json:"status"`
	Attempts       int           `json:"attempts"`
	Error          *string       `json:"error"`
	OccurredAt     time.Time     `json:"occurred_at"`
}

// Marshal encodes ev as JSON.
func Marshal(ev domain.StatusEvent) ([]byte, error) {
	return json.Marshal(wireEvent{
		NotificationID: ev.NotificationID, BatchID: ev.BatchID, Status: ev.Status,
		Attempts: ev.Attempts, Error: ev.Error, OccurredAt: ev.OccurredAt.UTC(),
	})
}

// Unmarshal decodes an event produced by Marshal.
func Unmarshal(b []byte) (domain.StatusEvent, error) {
	var w wireEvent
	if err := json.Unmarshal(b, &w); err != nil {
		return domain.StatusEvent{}, err
	}
	if w.NotificationID == uuid.Nil {
		return domain.StatusEvent{}, errors.New("event without notification_id")
	}
	return domain.StatusEvent{
		NotificationID: w.NotificationID, BatchID: w.BatchID, Status: w.Status,
		Attempts: w.Attempts, Error: w.Error, OccurredAt: w.OccurredAt,
	}, nil
}

// Publisher publishes events. It is best-effort by design: a lost event never fails or
// stalls a delivery, because subscribers resynchronise from the database (ADR-008).
type Publisher struct {
	rdb     redis.UniversalClient
	log     *slog.Logger
	timeout time.Duration
}

// NewPublisher returns a publisher on rdb.
func NewPublisher(rdb redis.UniversalClient, log *slog.Logger) *Publisher {
	return &Publisher{rdb: rdb, log: log, timeout: time.Second}
}

// Publish sends evs in one pipelined round trip. Errors are logged, not returned.
func (p *Publisher) Publish(ctx context.Context, evs ...domain.StatusEvent) {
	if len(evs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
	defer cancel()
	_, err := p.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, ev := range evs {
			b, err := Marshal(ev)
			if err != nil {
				return err
			}
			pipe.Publish(ctx, Channel, b)
		}
		return nil
	})
	if err != nil {
		p.log.WarnContext(ctx, "publishing status events failed", "error", err, "count", len(evs))
	}
}

// Subscription receives the events matching its filter. Its channel is closed when the
// subscriber falls behind or the hub loses Redis: events may have been missed, so the
// consumer must resynchronise (e.g. a WebSocket client reconnects and gets a new snapshot).
type Subscription struct {
	ch    chan domain.StatusEvent
	match func(domain.StatusEvent) bool
	hub   *Hub
}

// Events returns the event stream.
func (s *Subscription) Events() <-chan domain.StatusEvent { return s.ch }

// Close stops the subscription. It is safe to call more than once.
func (s *Subscription) Close() { s.hub.remove(s) }

// Hub holds one Redis subscription per process and fans events out to local subscribers.
// It never blocks on a subscriber.
type Hub struct {
	rdb  redis.UniversalClient
	log  *slog.Logger
	idle time.Duration // ping Redis after this long without a message

	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

// NewHub returns a hub on rdb; call Run to start it.
func NewHub(rdb redis.UniversalClient, log *slog.Logger) *Hub {
	return &Hub{rdb: rdb, log: log, idle: 30 * time.Second, subs: map[*Subscription]struct{}{}}
}

// Subscribe registers a subscriber for events accepted by match, buffering up to buffer
// events before it is considered too slow and dropped.
func (h *Hub) Subscribe(match func(domain.StatusEvent) bool, buffer int) *Subscription {
	s := &Subscription{ch: make(chan domain.StatusEvent, buffer), match: match, hub: h}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Subscribers reports the number of active subscriptions.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *Hub) remove(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(s)
}

func (h *Hub) removeLocked(s *Subscription) {
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.ch)
	}
}

// Dispatch delivers ev to every matching subscriber without blocking; a subscriber whose
// buffer is full is dropped.
func (h *Hub) Dispatch(ev domain.StatusEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if !s.match(ev) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			h.log.Warn("dropping slow event subscriber", "notification_id", ev.NotificationID.String())
			h.removeLocked(s)
		}
	}
}

// DropAll closes every subscription, forcing consumers to resynchronise.
func (h *Hub) DropAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		h.removeLocked(s)
	}
}

// Run consumes the Redis channel until ctx is done. If the connection breaks, every
// subscription is dropped (events published meanwhile are lost) and Run reconnects.
func (h *Hub) Run(ctx context.Context) {
	ps := h.rdb.Subscribe(ctx, Channel)
	defer func() { _ = ps.Close() }()
	for {
		msg, err := ps.ReceiveTimeout(ctx, h.idle)
		if err != nil {
			if ctx.Err() != nil {
				h.DropAll() // shutting down: tell WebSocket clients to reconnect elsewhere
				return
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				if err := ps.Ping(ctx); err == nil {
					continue // idle but healthy
				}
			}
			h.log.Warn("event subscription lost; dropping subscribers and reconnecting", "error", err)
			h.DropAll()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		switch m := msg.(type) {
		case *redis.Subscription:
			h.log.Info("subscribed to status events", "channel", m.Channel)
		case *redis.Message:
			ev, err := Unmarshal([]byte(m.Payload))
			if err != nil {
				h.log.Warn("ignoring malformed status event", "error", err)
				continue
			}
			h.Dispatch(ev)
		}
	}
}
