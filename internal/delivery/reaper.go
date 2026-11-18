package delivery

import (
	"context"
	"log/slog"
	"time"
)

// LeaseReaper returns expired leases to the queue; implemented by store/postgres.
type LeaseReaper interface {
	ReapExpired(ctx context.Context) (int, error)
}

// RunReaper periodically reclaims notifications whose worker crashed or stalled past its
// lease. It is safe to run in every worker replica: the UPDATE is idempotent.
func RunReaper(ctx context.Context, r LeaseReaper, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := r.ReapExpired(ctx)
			switch {
			case err != nil && ctx.Err() == nil:
				log.Error("reaping expired leases failed", "error", err)
			case n > 0:
				log.Warn("reclaimed notifications with expired leases", "count", n)
			}
		}
	}
}
