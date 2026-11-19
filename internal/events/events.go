// Package events carries notification status events over Redis pub/sub: workers and the
// API publish, and every API process runs one Hub that fans events out to its local
// subscribers (WebSocket connections). See ADR-008.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
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

const (
	publishQueueSize = 8192
	publishBatchMax  = 256
	publishTimeout   = time.Second
)

// Publisher publishes events. It is best-effort by design: a lost event never fails or
// stalls a delivery or a request, because subscribers resynchronise from the database
// (ADR-008). Publish only enqueues; one background goroutine sends queued events to Redis
// in pipelined batches, in the order they were published. When Redis is slow or down and
// the queue is full, new events are dropped (and counted in a log line).
type Publisher struct {
	rdb     redis.UniversalClient
	log     *slog.Logger
	queue   chan domain.StatusEvent
	done    chan struct{}
	dropped atomic.Int64

	mu     sync.RWMutex // guards closed against Publish racing Close
	closed bool
}

// NewPublisher starts a publisher on rdb. Call Close to flush and stop it.
func NewPublisher(rdb redis.UniversalClient, log *slog.Logger) *Publisher {
	p := &Publisher{rdb: rdb, log: log, queue: make(chan domain.StatusEvent, publishQueueSize), done: make(chan struct{})}
	go p.run()
	return p
}

// Publish enqueues evs without blocking. Events published after Close are discarded.
func (p *Publisher) Publish(_ context.Context, evs ...domain.StatusEvent) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return
	}
	for _, ev := range evs {
		select {
		case p.queue <- ev:
		default:
			p.dropped.Add(1)
		}
	}
}

// Close stops accepting events and waits until the queued ones are sent or ctx ends.
func (p *Publisher) Close(ctx context.Context) error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.queue)
	}
	p.mu.Unlock()
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("flushing status events: %w", ctx.Err())
	}
}

func (p *Publisher) run() {
	defer close(p.done)
	batch := make([]domain.StatusEvent, 0, publishBatchMax)
	for ev := range p.queue {
		batch = append(batch[:0], ev)
	fill:
		for len(batch) < publishBatchMax {
			select {
			case ev, ok := <-p.queue:
				if !ok {
					break fill
				}
				batch = append(batch, ev)
			default:
				break fill
			}
		}
		p.send(batch)
	}
}

// send publishes evs in one pipelined round trip. Errors are logged, not returned.
func (p *Publisher) send(evs []domain.StatusEvent) {
	if n := p.dropped.Swap(0); n > 0 {
		p.log.Warn("status events dropped: publish queue full", "count", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
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
		p.log.Warn("publishing status events failed", "error", err, "count", len(evs))
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
