package postgres

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/umutsahin222/insider/internal/domain"
)

// QueueDepths counts unfinished notifications by channel and priority. Only combinations
// that have at least one unfinished notification are returned. The query reads the two
// partial indexes on pending and processing rows, so its cost grows with the backlog,
// not with the size of the table.
func (s *Store) QueueDepths(ctx context.Context) ([]domain.QueueDepth, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT channel::text, priority,
			count(*) FILTER (WHERE status = 'pending' AND next_attempt_at <= now()),
			count(*) FILTER (WHERE status = 'pending' AND next_attempt_at > now()),
			count(*) FILTER (WHERE status = 'processing'),
			coalesce(extract(epoch FROM now() - min(next_attempt_at)
				FILTER (WHERE status = 'pending' AND next_attempt_at <= now())), 0)
		FROM notifications
		WHERE status IN ('pending', 'processing')
		GROUP BY channel, priority
		ORDER BY channel, priority`)
	if err != nil {
		return nil, fmt.Errorf("querying queue depths: %w", err)
	}
	depths, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.QueueDepth, error) {
		var (
			d                          domain.QueueDepth
			channel                    string
			priority                   int16
			ready, delayed, processing int64
			oldestSeconds              float64
		)
		if err := row.Scan(&channel, &priority, &ready, &delayed, &processing, &oldestSeconds); err != nil {
			return d, err
		}
		d.Channel, d.Priority = domain.Channel(channel), domain.Priority(priority)
		d.Ready, d.Delayed, d.Processing = int(ready), int(delayed), int(processing)
		d.OldestReadyAge = seconds(oldestSeconds)
		return d, nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning queue depths: %w", err)
	}
	return depths, nil
}

// DeliveryStats summarises delivery activity of the last window, per channel, using the
// database clock. Channels without activity in the window are omitted.
func (s *Store) DeliveryStats(ctx context.Context, window time.Duration) ([]domain.DeliveryStats, error) {
	byChannel := map[domain.Channel]*domain.DeliveryStats{}
	get := func(ch string) *domain.DeliveryStats {
		c := domain.Channel(ch)
		if byChannel[c] == nil {
			byChannel[c] = &domain.DeliveryStats{Channel: c}
		}
		return byChannel[c]
	}
	windowMs := window.Milliseconds()

	// Provider calls, by outcome, with per-call latency percentiles.
	rows, err := s.pool.Query(ctx, `
		SELECT n.channel::text, count(*),
			count(*) FILTER (WHERE a.outcome = 'success'),
			count(*) FILTER (WHERE a.outcome = 'retryable_error'),
			count(*) FILTER (WHERE a.outcome = 'permanent_error'),
			percentile_cont(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY a.duration_ms)
		FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id
		WHERE a.created_at >= now() - $1 * interval '1 millisecond'
		GROUP BY n.channel`, windowMs)
	if err != nil {
		return nil, fmt.Errorf("querying attempt stats: %w", err)
	}
	var (
		channel                                 string
		attempts, succeeded, retryable, perm, n int64
		pcts                                    []float64
	)
	_, err = pgx.ForEachRow(rows, []any{&channel, &attempts, &succeeded, &retryable, &perm, &pcts}, func() error {
		st := get(channel)
		st.Attempts, st.Succeeded, st.RetryableErrors, st.PermanentErrors = int(attempts), int(succeeded), int(retryable), int(perm)
		st.ProviderLatency = percentiles(pcts, time.Millisecond)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning attempt stats: %w", err)
	}

	// Sent notifications with end-to-end latency (due → sent).
	rows, err = s.pool.Query(ctx, `
		SELECT channel::text, count(*),
			percentile_cont(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY
				extract(epoch FROM sent_at - GREATEST(created_at, coalesce(scheduled_at, created_at))))
		FROM notifications
		WHERE status = 'sent' AND sent_at >= now() - $1 * interval '1 millisecond'
		GROUP BY channel`, windowMs)
	if err != nil {
		return nil, fmt.Errorf("querying sent stats: %w", err)
	}
	_, err = pgx.ForEachRow(rows, []any{&channel, &n, &pcts}, func() error {
		st := get(channel)
		st.Sent, st.EndToEndLatency = int(n), percentiles(pcts, time.Second)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning sent stats: %w", err)
	}

	// Notifications that failed permanently (dead-lettered) in the window.
	rows, err = s.pool.Query(ctx, `
		SELECT channel::text, count(*) FROM notifications
		WHERE status = 'failed' AND updated_at >= now() - $1 * interval '1 millisecond'
		GROUP BY channel`, windowMs)
	if err != nil {
		return nil, fmt.Errorf("querying failure stats: %w", err)
	}
	_, err = pgx.ForEachRow(rows, []any{&channel, &n}, func() error {
		get(channel).Failed = int(n)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning failure stats: %w", err)
	}

	out := make([]domain.DeliveryStats, 0, len(byChannel))
	for _, st := range byChannel {
		out = append(out, *st)
	}
	slices.SortFunc(out, func(a, b domain.DeliveryStats) int { return cmp.Compare(a.Channel, b.Channel) })
	return out, nil
}

// percentiles converts [p50, p95, p99] in the given unit to durations.
func percentiles(v []float64, unit time.Duration) domain.Percentiles {
	if len(v) != 3 {
		return domain.Percentiles{}
	}
	d := func(x float64) time.Duration { return time.Duration(x * float64(unit)) }
	return domain.Percentiles{P50: d(v[0]), P95: d(v[1]), P99: d(v[2])}
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
