package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/umutsahin222/insider/internal/domain"
)

// Queue operations (ADR-001). All queue timing (claim eligibility, leases, retry delays)
// uses the database clock, so clock skew between API/worker hosts and Postgres cannot
// delay or prematurely release work; callers pass durations, not absolute times.
// Every transition out of 'processing' is fenced on
// (status = 'processing' AND attempts = <attempt the worker claimed>): if the lease expired
// and another worker re-claimed the row, attempts has moved on and the stale worker's
// write matches nothing, so it gets domain.ErrLeaseLost instead of clobbering newer state.

// Claim atomically takes up to limit due notifications of one channel, highest priority
// first, and leases them for the given duration. Concurrent callers never receive the
// same row (FOR UPDATE SKIP LOCKED).
func (s *Store) Claim(ctx context.Context, ch domain.Channel, limit int, lease time.Duration) ([]domain.Notification, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE notifications SET status = 'processing', attempts = attempts + 1,
			locked_until = now() + $3 * interval '1 millisecond'
		WHERE id IN (
			SELECT id FROM notifications
			WHERE status = 'pending' AND channel = $1::text::notification_channel AND next_attempt_at <= now()
			ORDER BY priority, next_attempt_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		RETURNING `+notificationColumns,
		string(ch), limit, lease.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("claiming notifications: %w", err)
	}
	ns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Notification, error) { return scanNotification(row) })
	if err != nil {
		return nil, fmt.Errorf("scanning claimed notifications: %w", err)
	}
	return ns, nil
}

// MarkSent records a successful attempt and moves the notification to sent.
func (s *Store) MarkSent(ctx context.Context, n domain.Notification, providerMessageID *string, a domain.DeliveryAttempt) error {
	return s.finishAttempt(ctx, n, a, `
		UPDATE notifications SET status = 'sent', sent_at = now(), locked_until = NULL,
			provider_message_id = $3, last_error = NULL
		WHERE id = $1 AND status = 'processing' AND attempts = $2`,
		providerMessageID)
}

// ScheduleRetry records a failed attempt and returns the notification to the queue,
// due again after delay.
func (s *Store) ScheduleRetry(ctx context.Context, n domain.Notification, delay time.Duration, a domain.DeliveryAttempt) error {
	return s.finishAttempt(ctx, n, a, `
		UPDATE notifications SET status = 'pending', locked_until = NULL,
			next_attempt_at = now() + $3 * interval '1 millisecond', last_error = $4
		WHERE id = $1 AND status = 'processing' AND attempts = $2`,
		delay.Milliseconds(), a.Error)
}

// MarkFailed records a failed attempt and moves the notification to failed (dead letter).
func (s *Store) MarkFailed(ctx context.Context, n domain.Notification, a domain.DeliveryAttempt) error {
	return s.finishAttempt(ctx, n, a, `
		UPDATE notifications SET status = 'failed', locked_until = NULL, last_error = $3
		WHERE id = $1 AND status = 'processing' AND attempts = $2`,
		a.Error)
}

// finishAttempt writes the attempt audit row and the fenced transition in one transaction.
// The audit row is kept even when the lease was lost: the provider call did happen.
func (s *Store) finishAttempt(ctx context.Context, n domain.Notification, a domain.DeliveryAttempt, update string, args ...any) error {
	leaseLost := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO delivery_attempts (notification_id, attempt, outcome, status_code, error, duration_ms)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (notification_id, attempt) DO NOTHING`,
			n.ID, n.Attempts, string(a.Outcome), a.StatusCode, a.Error, a.Duration.Milliseconds()); err != nil {
			return fmt.Errorf("recording attempt: %w", err)
		}
		tag, err := tx.Exec(ctx, update, append([]any{n.ID, n.Attempts}, args...)...)
		if err != nil {
			return fmt.Errorf("updating notification: %w", err)
		}
		// Returning nil commits the audit row even when the fenced update matched nothing.
		leaseLost = tag.RowsAffected() == 0
		return nil
	})
	if err != nil {
		return err
	}
	if leaseLost {
		return domain.ErrLeaseLost
	}
	return nil
}

// Release returns a claimed notification to the queue without counting an attempt,
// e.g. when the circuit breaker is open or the worker is shutting down.
func (s *Store) Release(ctx context.Context, n domain.Notification, delay time.Duration) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications SET status = 'pending', attempts = attempts - 1, locked_until = NULL,
			next_attempt_at = now() + $3 * interval '1 millisecond'
		WHERE id = $1 AND status = 'processing' AND attempts = $2`,
		n.ID, n.Attempts, delay.Milliseconds())
	if err != nil {
		return fmt.Errorf("releasing notification: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrLeaseLost
	}
	return nil
}

// ReapExpired returns notifications whose lease expired (crashed or stuck worker) to the
// queue, or fails them if that was their last attempt. It returns how many rows it touched.
func (s *Store) ReapExpired(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications SET
			status = CASE WHEN attempts >= max_attempts THEN 'failed'::notification_status ELSE 'pending' END,
			locked_until = NULL, next_attempt_at = now(), last_error = 'lease expired before the attempt completed'
		WHERE status = 'processing' AND locked_until < now()`)
	if err != nil {
		return 0, fmt.Errorf("reaping expired leases: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
