package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/umutsahin222/insider/internal/domain"
)

// Outbox is the transactional outbox the relay drains; implemented by
// store/postgres.OutboxRelay. Pending and Remove only work while TryLead holds, and fail
// once leadership is lost.
type Outbox interface {
	TryLead(ctx context.Context) (bool, error)
	Resign()
	Pending(ctx context.Context, limit int) ([]domain.OutboxEvent, error)
	Remove(ctx context.Context, seqs []int64) error
}

// RelayMetrics receives relay measurements; implemented by metrics.RelayMetrics.
type RelayMetrics interface {
	RelayLeading(leading bool)
	EventsRelayed(n int)
	RelayPublishFailed()
}

type nopRelayMetrics struct{}

func (nopRelayMetrics) RelayLeading(bool)   {}
func (nopRelayMetrics) EventsRelayed(int)   {}
func (nopRelayMetrics) RelayPublishFailed() {}

// RelayConfig tunes the relay.
type RelayConfig struct {
	// MaxLen is roughly how many events the stream keeps (XADD MAXLEN ~); older ones are
	// trimmed, which limits how far back a WebSocket client can resume.
	MaxLen int64
	// PollInterval is the wait when the outbox is empty.
	PollInterval time.Duration
	// BatchSize is how many events one round moves.
	BatchSize int
}

const (
	relayLeaderRetry   = 2 * time.Second
	relayPublishRetry  = time.Second
	relayPublishBudget = 2 * time.Second
	relayRemoveBudget  = 5 * time.Second
)

// Relay copies the outbox to the stream, oldest first, then deletes what it copied. Every
// worker process runs one; an advisory lock lets exactly one of them work at a time, so
// events of one notification reach the stream in the order they happened.
//
// Delivery is at least once: a crash between XADD and the delete publishes those events
// again. Consumers order a notification's events by attempts and may see one twice.
type Relay struct {
	outbox  Outbox
	rdb     redis.Cmdable
	cfg     RelayConfig
	metrics RelayMetrics
	log     *slog.Logger
}

// NewRelay returns a relay; a nil m disables metrics.
func NewRelay(outbox Outbox, rdb redis.Cmdable, cfg RelayConfig, m RelayMetrics, log *slog.Logger) *Relay {
	if m == nil {
		m = nopRelayMetrics{}
	}
	return &Relay{outbox: outbox, rdb: rdb, cfg: cfg, metrics: m, log: log}
}

// Run relays while this process leads, and keeps trying to lead otherwise, until ctx is done.
func (r *Relay) Run(ctx context.Context) {
	defer r.outbox.Resign()
	for ctx.Err() == nil {
		leading, err := r.outbox.TryLead(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.Warn("cannot take the status event relay lock", "error", err)
		}
		if !leading {
			r.metrics.RelayLeading(false)
			sleep(ctx, relayLeaderRetry)
			continue
		}
		r.metrics.RelayLeading(true)
		r.log.Info("relaying status events from the outbox")
		err = r.drain(ctx)
		r.outbox.Resign()
		r.metrics.RelayLeading(false)
		if ctx.Err() != nil {
			return
		}
		r.log.Warn("stopped relaying status events; trying to lead again", "error", err)
		sleep(ctx, relayPublishRetry)
	}
}

// drain relays until the leader session fails or ctx is done. Redis failures are retried
// without giving up leadership: another process would hit the same Redis.
func (r *Relay) drain(ctx context.Context) error {
	for {
		evs, err := r.outbox.Pending(ctx, r.cfg.BatchSize)
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			sleep(ctx, r.cfg.PollInterval)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		if err := r.publish(ctx, evs); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.metrics.RelayPublishFailed()
			r.log.Warn("publishing status events failed; retrying", "error", err, "count", len(evs))
			sleep(ctx, relayPublishRetry)
			continue
		}
		seqs := make([]int64, len(evs))
		for i, e := range evs {
			seqs[i] = e.Seq
		}
		// Published events are removed even if shutdown has begun, so a restart does not
		// publish them again.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relayRemoveBudget)
		err = r.outbox.Remove(rctx, seqs)
		cancel()
		if err != nil {
			return err
		}
		r.metrics.EventsRelayed(len(evs))
		if len(evs) < r.cfg.BatchSize {
			sleep(ctx, r.cfg.PollInterval)
		}
	}
}

// publish appends evs to the stream in one pipelined round trip, in order.
func (r *Relay) publish(ctx context.Context, evs []domain.OutboxEvent) error {
	ctx, cancel := context.WithTimeout(ctx, relayPublishBudget)
	defer cancel()
	_, err := r.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, e := range evs {
			b, err := Marshal(e.Event)
			if err != nil {
				return err
			}
			pipe.XAdd(ctx, &redis.XAddArgs{Stream: Stream, MaxLen: r.cfg.MaxLen, Approx: true, Values: []any{"event", b}})
		}
		return nil
	})
	return err
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
