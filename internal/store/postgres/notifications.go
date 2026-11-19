// Package postgres implements persistence on PostgreSQL (pgx/v5).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/umutsahin222/insider/internal/domain"
)

// Store is the PostgreSQL-backed notification repository.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store using pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Connect opens a connection pool and verifies connectivity.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	return pool, nil
}

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const notificationColumns = `id, batch_id, idempotency_key, channel::text, recipient, content, template_id,
	priority, status::text, scheduled_at, next_attempt_at, attempts, max_attempts, locked_until,
	last_error, provider_message_id, correlation_id, trace_context, coalesce(request_hash, ''),
	created_at, updated_at, sent_at`

func scanNotification(row pgx.Row) (domain.Notification, error) {
	var (
		n                domain.Notification
		channel, status  string
		priority         int16
		attempts, maxAtt int32
	)
	err := row.Scan(&n.ID, &n.BatchID, &n.IdempotencyKey, &channel, &n.Recipient, &n.Content, &n.TemplateID,
		&priority, &status, &n.ScheduledAt, &n.NextAttemptAt, &attempts, &maxAtt, &n.LockedUntil,
		&n.LastError, &n.ProviderMessageID, &n.CorrelationID, &n.TraceContext, &n.RequestHash,
		&n.CreatedAt, &n.UpdatedAt, &n.SentAt)
	if err != nil {
		return domain.Notification{}, err
	}
	n.Channel, n.Status, n.Priority = domain.Channel(channel), domain.Status(status), domain.Priority(priority)
	n.Attempts, n.MaxAttempts = int(attempts), int(maxAtt)
	return n, nil
}

// traceParam stores an empty trace context as SQL NULL rather than JSON null.
func traceParam(tc map[string]string) any {
	if len(tc) == 0 {
		return nil
	}
	return tc
}

// next_attempt_at is computed by the database (GREATEST(scheduled_at, now())) so the queue
// runs on a single clock; see queue.go.

// CreateNotification inserts n. If n carries an idempotency key that already exists,
// nothing is inserted and the existing notification is returned with created=false;
// whether the replay matches the original request is the caller's decision
// (domain.VerifyReplay). The check is race-free: concurrent requests with the same key
// resolve through the unique index, not through a prior read.
func (s *Store) CreateNotification(ctx context.Context, n domain.Notification) (domain.Notification, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO notifications (id, batch_id, idempotency_key, channel, recipient, content, template_id,
			priority, scheduled_at, next_attempt_at, max_attempts, correlation_id, trace_context, request_hash)
		VALUES ($1, $2, $3, $4::text::notification_channel, $5, $6, $7, $8, $9, GREATEST($9::timestamptz, now()),
			$10, $11, $12, NULLIF($13, ''))
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING `+notificationColumns,
		n.ID, n.BatchID, n.IdempotencyKey, string(n.Channel), n.Recipient, n.Content, n.TemplateID,
		int16(n.Priority), n.ScheduledAt, n.MaxAttempts, n.CorrelationID, traceParam(n.TraceContext), n.RequestHash)

	created, err := scanNotification(row)
	if err == nil {
		return created, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) || n.IdempotencyKey == nil {
		return domain.Notification{}, false, fmt.Errorf("inserting notification: %w", err)
	}

	existing, err := scanNotification(s.pool.QueryRow(ctx,
		`SELECT `+notificationColumns+` FROM notifications WHERE idempotency_key = $1`, *n.IdempotencyKey))
	if err != nil {
		return domain.Notification{}, false, fmt.Errorf("loading notification for idempotency key: %w", err)
	}
	return existing, false, nil
}

// CreateBatch records the batch and inserts all of its notifications in one transaction.
//
// If b carries an idempotency key that already exists, nothing is inserted and the original
// batch (with its notifications in request order) is returned with created=false; the caller
// verifies that the replay matches (domain.VerifyReplay). Concurrent requests with the same
// key serialise on the unique index. A per-item idempotency key that already exists aborts
// the whole batch with ErrConflict.
func (s *Store) CreateBatch(ctx context.Context, b domain.Batch) (domain.Batch, bool, error) {
	if len(b.Notifications) == 0 {
		return domain.Batch{}, false, errors.New("creating batch: no notifications")
	}
	var (
		out     domain.Batch
		created bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO batches (id, idempotency_key, request_hash, size) VALUES ($1, $2, NULLIF($3, ''), $4)
			ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
			RETURNING id`,
			b.ID, b.IdempotencyKey, b.RequestHash, len(b.Notifications)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) && b.IdempotencyKey != nil {
			out, err = loadBatchByKey(ctx, tx, *b.IdempotencyKey)
			return err
		}
		if err != nil {
			return fmt.Errorf("inserting batch: %w", err)
		}
		if err := insertNotifications(ctx, tx, b.Notifications); err != nil {
			return err
		}
		out, created = b, true
		return nil
	})
	if err != nil {
		return domain.Batch{}, false, err
	}
	return out, created, nil
}

func loadBatchByKey(ctx context.Context, tx pgx.Tx, key string) (domain.Batch, error) {
	b := domain.Batch{IdempotencyKey: &key}
	if err := tx.QueryRow(ctx,
		`SELECT id, coalesce(request_hash, '') FROM batches WHERE idempotency_key = $1`, key,
	).Scan(&b.ID, &b.RequestHash); err != nil {
		return domain.Batch{}, fmt.Errorf("loading batch for idempotency key: %w", err)
	}
	// UUIDv7 IDs are generated in item order (monotonic within a process), so ORDER BY id
	// restores the order of the original request.
	rows, err := tx.Query(ctx,
		`SELECT `+notificationColumns+` FROM notifications WHERE batch_id = $1 ORDER BY id`, b.ID)
	if err != nil {
		return domain.Batch{}, fmt.Errorf("loading batch notifications: %w", err)
	}
	b.Notifications, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Notification, error) {
		return scanNotification(row)
	})
	if err != nil {
		return domain.Batch{}, fmt.Errorf("scanning batch notifications: %w", err)
	}
	return b, nil
}

// insertNotifications inserts ns with a single statement: one array per column, zipped back
// into rows server-side by unnest().
func insertNotifications(ctx context.Context, tx pgx.Tx, ns []domain.Notification) error {
	size := len(ns)
	ids, batchIDs := make([]string, size), make([]string, size)
	channels, recipients, contents, corrIDs := make([]string, size), make([]string, size), make([]string, size), make([]string, size)
	idemKeys, templateIDs, hashes := make([]*string, size), make([]*string, size), make([]string, size)
	priorities, maxAttempts := make([]int16, size), make([]int32, size)
	scheduled := make([]*time.Time, size)
	for i, n := range ns {
		ids[i] = n.ID.String()
		if n.BatchID != nil {
			batchIDs[i] = n.BatchID.String()
		}
		channels[i], recipients[i], contents[i], corrIDs[i] = string(n.Channel), n.Recipient, n.Content, n.CorrelationID
		idemKeys[i], hashes[i] = n.IdempotencyKey, n.RequestHash
		if n.TemplateID != nil {
			s := n.TemplateID.String()
			templateIDs[i] = &s
		}
		priorities[i], scheduled[i], maxAttempts[i] = int16(n.Priority), n.ScheduledAt, int32(n.MaxAttempts)
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO notifications (id, batch_id, idempotency_key, channel, recipient, content, template_id,
			priority, scheduled_at, next_attempt_at, max_attempts, correlation_id, trace_context, request_hash)
		SELECT u.id, NULLIF(u.batch_id, '')::uuid, u.idem, u.channel::notification_channel, u.recipient, u.content,
			u.template_id, u.priority, u.scheduled_at, GREATEST(u.scheduled_at, now()), u.max_attempts, u.corr,
			$12::jsonb, NULLIF(u.hash, '')
		FROM unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::uuid[],
			$8::smallint[], $9::timestamptz[], $10::int[], $11::text[], $13::text[])
			AS u(id, batch_id, idem, channel, recipient, content, template_id, priority, scheduled_at,
				max_attempts, corr, hash)`,
		ids, batchIDs, idemKeys, channels, recipients, contents, templateIDs,
		priorities, scheduled, maxAttempts, corrIDs, traceParam(ns[0].TraceContext), hashes)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "notifications_idempotency_key_uq" {
		return domain.Conflict("idempotency key already used: " + pgErr.Detail)
	}
	if err != nil {
		return fmt.Errorf("inserting batch notifications: %w", err)
	}
	return nil
}

// GetNotification returns the notification with the given ID.
func (s *Store) GetNotification(ctx context.Context, id uuid.UUID) (domain.Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx,
		`SELECT `+notificationColumns+` FROM notifications WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, domain.NotFound("notification not found")
	}
	if err != nil {
		return domain.Notification{}, fmt.Errorf("getting notification: %w", err)
	}
	return n, nil
}

// ListAttempts returns the delivery attempts of a notification, oldest first.
func (s *Store) ListAttempts(ctx context.Context, id uuid.UUID) ([]domain.DeliveryAttempt, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, notification_id, attempt, outcome, status_code, error, duration_ms, created_at
		FROM delivery_attempts WHERE notification_id = $1 ORDER BY attempt`, id)
	if err != nil {
		return nil, fmt.Errorf("listing attempts: %w", err)
	}
	attempts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.DeliveryAttempt, error) {
		var (
			a          domain.DeliveryAttempt
			attempt    int32
			outcome    string
			statusCode *int32
			durationMs int32
		)
		if err := row.Scan(&a.ID, &a.NotificationID, &attempt, &outcome, &statusCode, &a.Error, &durationMs, &a.CreatedAt); err != nil {
			return a, err
		}
		a.Attempt, a.Outcome, a.Duration = int(attempt), domain.AttemptOutcome(outcome), time.Duration(durationMs)*time.Millisecond
		if statusCode != nil {
			c := int(*statusCode)
			a.StatusCode = &c
		}
		return a, nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning attempts: %w", err)
	}
	return attempts, nil
}

// ListNotifications returns one page, newest first, using keyset pagination.
func (s *Store) ListNotifications(ctx context.Context, f domain.ListFilter) (domain.Page, error) {
	var (
		where []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.Status != nil {
		where = append(where, "status = "+arg(string(*f.Status))+"::text::notification_status")
	}
	if f.Channel != nil {
		where = append(where, "channel = "+arg(string(*f.Channel))+"::text::notification_channel")
	}
	if f.BatchID != nil {
		where = append(where, "batch_id = "+arg(*f.BatchID))
	}
	if f.From != nil {
		where = append(where, "created_at >= "+arg(*f.From))
	}
	if f.To != nil {
		where = append(where, "created_at < "+arg(*f.To))
	}
	if f.After != nil {
		where = append(where, "(created_at, id) < ("+arg(f.After.CreatedAt)+", "+arg(f.After.ID)+")")
	}
	query := `SELECT ` + notificationColumns + ` FROM notifications`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// Fetch one extra row to learn whether another page exists.
	query += " ORDER BY created_at DESC, id DESC LIMIT " + arg(f.Limit+1)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return domain.Page{}, fmt.Errorf("listing notifications: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Notification, error) {
		return scanNotification(row)
	})
	if err != nil {
		return domain.Page{}, fmt.Errorf("scanning notifications: %w", err)
	}

	page := domain.Page{Items: items}
	if len(items) > f.Limit {
		page.Items = items[:f.Limit]
		last := page.Items[f.Limit-1]
		page.Next = &domain.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

// CancelNotification moves a pending notification to cancelled.
// The UPDATE is conditional on status='pending', so it cannot race a worker's claim.
func (s *Store) CancelNotification(ctx context.Context, id uuid.UUID) (domain.Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx, `
		UPDATE notifications SET status = 'cancelled'
		WHERE id = $1 AND status = 'pending'
		RETURNING `+notificationColumns, id))
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, fmt.Errorf("cancelling notification: %w", err)
	}
	// Nothing was updated: explain why. This read only shapes the error; it never gates a write.
	current, err := s.GetNotification(ctx, id)
	if err != nil {
		return domain.Notification{}, err
	}
	return domain.Notification{}, domain.Conflict(
		"notification is " + string(current.Status) + "; only pending notifications can be cancelled")
}

// RetryNotification moves a failed notification back to the queue, due now, with
// extraAttempts more attempts. attempts is deliberately not reset: it is the lease fencing
// token (ADR-005) and part of the delivery_attempts key, so restarting it at 0 would let a
// stale worker's write match a new attempt and would collide with the existing audit rows.
func (s *Store) RetryNotification(ctx context.Context, id uuid.UUID, extraAttempts int) (domain.Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx, `
		UPDATE notifications SET status = 'pending', max_attempts = attempts + $2,
			next_attempt_at = now(), locked_until = NULL
		WHERE id = $1 AND status = 'failed'
		RETURNING `+notificationColumns, id, extraAttempts))
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, fmt.Errorf("retrying notification: %w", err)
	}
	current, err := s.GetNotification(ctx, id)
	if err != nil {
		return domain.Notification{}, err
	}
	return domain.Notification{}, domain.Conflict(
		"notification is " + string(current.Status) + "; only failed notifications can be retried")
}

// BatchSummary counts a batch's notifications by status.
func (s *Store) BatchSummary(ctx context.Context, batchID uuid.UUID) (domain.BatchSummary, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT status::text, count(*) FROM notifications WHERE batch_id = $1 GROUP BY status`, batchID)
	if err != nil {
		return domain.BatchSummary{}, fmt.Errorf("summarising batch: %w", err)
	}
	sum := domain.BatchSummary{BatchID: batchID, Counts: map[domain.Status]int{}}
	var (
		status string
		count  int64
	)
	_, err = pgx.ForEachRow(rows, []any{&status, &count}, func() error {
		sum.Counts[domain.Status(status)] = int(count)
		sum.Total += int(count)
		return nil
	})
	if err != nil {
		return domain.BatchSummary{}, fmt.Errorf("scanning batch summary: %w", err)
	}
	if sum.Total == 0 {
		return domain.BatchSummary{}, domain.NotFound("batch not found")
	}
	return sum, nil
}

// CancelBatch cancels every still-pending notification of a batch and returns how many.
func (s *Store) CancelBatch(ctx context.Context, batchID uuid.UUID) ([]domain.Notification, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE notifications SET status = 'cancelled' WHERE batch_id = $1 AND status = 'pending'
		RETURNING `+notificationColumns, batchID)
	if err != nil {
		return nil, fmt.Errorf("cancelling batch: %w", err)
	}
	cancelled, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Notification, error) { return scanNotification(row) })
	if err != nil {
		return nil, fmt.Errorf("cancelling batch: %w", err)
	}
	if len(cancelled) > 0 {
		return cancelled, nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM batches WHERE id = $1)`, batchID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("checking batch: %w", err)
	}
	if !exists {
		return nil, domain.NotFound("batch not found")
	}
	return nil, nil
}
