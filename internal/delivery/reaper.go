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
// lease. It is safe to run in every worker replica: the UPDATE is idempotent. The database
// records the status events of what it reclaims (ADR-012). A nil m disables metrics.
func RunReaper(ctx context.Context, r LeaseReaper, interval time.Duration, m Metrics, log *slog.Logger) {
	if m == nil {
		m = NopMetrics{}
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
				log.Warn("reclaimed notifications with expired leases", "count", len(reaped))
			}
		}
	}
}
