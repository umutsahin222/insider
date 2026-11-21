package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/umutsahin222/insider/internal/domain"
)

// DropReason says why the hub closed a subscription: always a closed set (metric label).
type DropReason string

const (
	// DropSlowConsumer: the subscriber's buffer was full.
	DropSlowConsumer DropReason = "slow_consumer"
	// DropResync: the hub may have missed events (Redis restarted or trimmed the stream past
	// the hub's position) and restarted from the stream's end.
	DropResync DropReason = "resync"
	// DropShutdown: the hub stopped.
	DropShutdown DropReason = "shutdown"
)

// HubMetrics receives hub measurements; implemented by metrics.HTTPMetrics.
type HubMetrics interface {
	SubscriberDropped(reason DropReason)
}

type nopHubMetrics struct{}

func (nopHubMetrics) SubscriberDropped(DropReason) {}

// Subscription receives the events matching its filter. Its channel is closed when the hub
// can no longer guarantee it saw every event (slow subscriber, stream gap, shutdown): the
// consumer must resynchronise, e.g. a WebSocket client reconnects and gets a snapshot.
type Subscription struct {
	ch    chan domain.StatusEvent
	match func(domain.StatusEvent) bool
	hub   *Hub
}

// Events returns the event stream.
func (s *Subscription) Events() <-chan domain.StatusEvent { return s.ch }

// Close stops the subscription. It is safe to call more than once.
func (s *Subscription) Close() { s.hub.remove(s) }

const (
	hubReadBlock   = time.Second // XREAD BLOCK; also bounds how long Run takes to notice ctx
	hubReadCount   = 512
	hubRetry       = time.Second
	replayMaxCount = 10_000
)

// ErrReplayUnavailable means the stream cannot prove it still holds every event after the
// requested position; the caller should fall back to a snapshot.
var ErrReplayUnavailable = errors.New("events after this position are not available")

// Hub reads the event stream once per process and fans events out to local subscribers.
// It never blocks on a subscriber.
//
// A broken Redis connection costs nothing: the hub resumes reading after the last entry it
// dispatched, so subscribers stay connected. Only a gap ends subscriptions: Redis restarted
// (it may have lost acknowledged entries) or trimmed the stream past the hub's position.
type Hub struct {
	rdb     *redis.Client
	log     *slog.Logger
	metrics HubMetrics

	mu   sync.Mutex
	subs map[*Subscription]struct{}
	// since is the stream position after which this hub has dispatched every event, and
	// runID the Redis server it read them from. Replay serves only that history.
	since, runID string
}

// NewHub returns a hub reading from the Redis described by opts; call Run to start it and
// Close when done. A nil opts makes a hub without Redis (events come only from Dispatch); a
// nil m disables metrics.
//
// The hub opens its own client with retries disabled. go-redis would otherwise retry a
// failed XREAD on a new connection, even to a restarted server, and the hub would never see
// the failure that makes it check for a gap.
func NewHub(opts *redis.Options, log *slog.Logger, m HubMetrics) *Hub {
	if m == nil {
		m = nopHubMetrics{}
	}
	h := &Hub{log: log, metrics: m, subs: map[*Subscription]struct{}{}}
	if opts != nil {
		o := *opts
		o.MaxRetries = -1
		h.rdb = redis.NewClient(&o)
	}
	return h
}

// Close releases the hub's Redis client. Call it after Run has returned.
func (h *Hub) Close() error {
	if h.rdb == nil {
		return nil
	}
	return h.rdb.Close()
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
			h.metrics.SubscriberDropped(DropSlowConsumer)
		}
	}
}

// DropAll closes every subscription, forcing consumers to resynchronise.
func (h *Hub) DropAll(reason DropReason) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		h.removeLocked(s)
		h.metrics.SubscriberDropped(reason)
	}
}

// Run reads the stream until ctx is done, then drops every subscription and returns.
func (h *Hub) Run(ctx context.Context) {
	if h.rdb == nil {
		<-ctx.Done()
		h.DropAll(DropShutdown)
		return
	}
	var pos, runID string // pos: last entry dispatched; "" until positioned
	for {
		if ctx.Err() != nil {
			h.DropAll(DropShutdown)
			return
		}
		id, err := h.serverRunID(ctx)
		if err == nil && pos != "" && id != runID {
			h.log.Warn("redis restarted; resynchronising event subscribers")
			pos = ""
		}
		if err == nil && pos != "" {
			var gap bool
			if gap, err = h.trimmedPast(ctx, pos); gap {
				h.log.Warn("event stream trimmed past the hub's position; resynchronising event subscribers")
				pos = ""
			}
		}
		if err == nil && pos == "" {
			if pos, err = h.lastEntryID(ctx); err == nil {
				h.positioned(pos, id)
			}
		}
		if err == nil {
			runID = id
			err = h.read(ctx, &pos)
		}
		if ctx.Err() != nil {
			continue // drops subscribers and returns at the top
		}
		h.log.Warn("reading status events failed; resuming after the last event read", "error", err)
		sleep(ctx, hubRetry)
	}
}

// positioned records that the hub now reads every event after pos from server runID. Anyone
// subscribed before may have missed events, so they are dropped.
func (h *Hub) positioned(pos, runID string) {
	h.mu.Lock()
	first := h.since == ""
	h.since, h.runID = pos, runID
	h.mu.Unlock()
	if !first {
		h.DropAll(DropResync)
	}
}

// read dispatches events after *pos, advancing it, until Redis fails or ctx is done.
func (h *Hub) read(ctx context.Context, pos *string) error {
	for {
		res, err := h.rdb.XRead(ctx, &redis.XReadArgs{
			Streams: []string{Stream, *pos}, Count: hubReadCount, Block: hubReadBlock,
		}).Result()
		switch {
		case errors.Is(err, redis.Nil): // nothing new within the block time
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		case err != nil:
			return err
		}
		for _, s := range res {
			for _, m := range s.Messages {
				*pos = m.ID
				ev, err := decodeEntry(m)
				if err != nil {
					h.log.Warn("ignoring malformed status event", "event_id", m.ID, "error", err)
					continue
				}
				h.Dispatch(ev)
			}
		}
	}
}

// Replay returns the events after streamID accepted by match, oldest first. It fails with
// ErrReplayUnavailable unless the stream provably still holds all of them: streamID lies
// within this hub's uninterrupted history on the current Redis server, the entry itself has
// not been trimmed, and no more than replayMaxCount entries follow it.
func (h *Hub) Replay(ctx context.Context, after string, match func(domain.StatusEvent) bool) ([]domain.StatusEvent, error) {
	a, ok := parseStreamID(after)
	if !ok {
		return nil, ErrReplayUnavailable
	}
	h.mu.Lock()
	since, runID := h.since, h.runID
	h.mu.Unlock()
	if s, ok := parseStreamID(since); !ok || a.less(s) {
		return nil, ErrReplayUnavailable
	}
	if id, err := h.serverRunID(ctx); err != nil || id != runID {
		return nil, ErrReplayUnavailable
	}
	if anchor, err := h.rdb.XRange(ctx, Stream, after, after).Result(); err != nil || len(anchor) == 0 {
		return nil, ErrReplayUnavailable // trimmed: entries right after it may be gone too
	}
	msgs, err := h.rdb.XRangeN(ctx, Stream, "("+after, "+", replayMaxCount+1).Result()
	if err != nil || len(msgs) > replayMaxCount {
		return nil, ErrReplayUnavailable
	}
	var out []domain.StatusEvent
	for _, m := range msgs {
		if ev, err := decodeEntry(m); err == nil && match(ev) {
			out = append(out, ev)
		}
	}
	return out, nil
}

func decodeEntry(m redis.XMessage) (domain.StatusEvent, error) {
	raw, ok := m.Values["event"].(string)
	if !ok {
		return domain.StatusEvent{}, errors.New("entry without an event field")
	}
	ev, err := Unmarshal([]byte(raw))
	ev.StreamID = m.ID
	return ev, err
}

// lastEntryID is the stream's newest entry ID, or "0-0" for an empty or missing stream.
func (h *Hub) lastEntryID(ctx context.Context) (string, error) {
	msgs, err := h.rdb.XRevRangeN(ctx, Stream, "+", "-", 1).Result()
	if err != nil {
		return "", fmt.Errorf("reading the stream's last entry: %w", err)
	}
	if len(msgs) == 0 {
		return "0-0", nil
	}
	return msgs[0].ID, nil
}

// trimmedPast reports whether entries after pos may have been trimmed: pos is no longer in
// the stream (or the stream is gone).
func (h *Hub) trimmedPast(ctx context.Context, pos string) (bool, error) {
	if pos == "0-0" {
		return false, nil
	}
	msgs, err := h.rdb.XRange(ctx, Stream, pos, pos).Result()
	if err != nil {
		return false, fmt.Errorf("checking the stream position: %w", err)
	}
	return len(msgs) == 0, nil
}

// serverRunID identifies the Redis server process; it changes when Redis restarts.
func (h *Hub) serverRunID(ctx context.Context) (string, error) {
	info, err := h.rdb.Info(ctx, "server").Result()
	if err != nil {
		return "", fmt.Errorf("reading redis server info: %w", err)
	}
	for line := range strings.SplitSeq(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "run_id:"); ok {
			return v, nil
		}
	}
	return "", errors.New("redis server info has no run_id")
}
