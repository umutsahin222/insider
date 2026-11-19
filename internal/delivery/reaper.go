package delivery

import (
	"context"
	"log/slog"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

// LeaseReaper returns expired leases to the queue and reports the rows it changed;
// implemented by store/postgres.
type LeaseReaper interface {
	ReapExpired(ctx context.Context) ([]domain.Notification, error)
}

// RunReaper periodically reclaims notifications whose worker crashed or stalled past its
// lease. It is safe to run in every worker replica: the UPDATE is idempotent.
// Nil m and ev disable metrics and events.
func RunReaper(ctx context.Context, r LeaseReaper, interval time.Duration, m Metrics, ev Events, log *slog.Logger) {
	if m == nil {
		m = NopMetrics{}
	}
	if ev == nil {
		ev = NopEvents{}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reaped, err := r.ReapExpired(ctx)
			switch {
			case err != nil && ctx.Err() == nil:
				log.Error("reaping expired leases failed", "error", err)
			case len(reaped) > 0:
				m.Reclaimed(len(reaped))
				now := time.Now()
				evs := make([]domain.StatusEvent, len(reaped))
				for i, n := range reaped {
					evs[i] = domain.EventOf(n, now)
				}
				ev.Publish(ctx, evs...)
				log.Warn("reclaimed notifications with expired leases", "count", len(reaped))
			}
		}
	}
}
