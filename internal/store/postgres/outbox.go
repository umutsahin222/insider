package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/umutsahin222/insider/internal/domain"
)

// relayLockKey is the advisory lock that elects the one process relaying the status event
// outbox (ADR-012). Any constant no other code locks works.
const relayLockKey int64 = 7_011_012

// OutboxRelay is one process's handle on the status event outbox (migration 000007). It is
// used by a single goroutine (the relay).
//
// TryLead takes a session-level advisory lock on a connection of its own, and Pending and
// Remove run on that same session. Leadership therefore cannot outlive the session: if the
// connection breaks, the server releases the lock, and this handle's next call fails instead
// of acting as a leader that no longer is.
type OutboxRelay struct {
	store *Store
	conn  *pgx.Conn // the leader session; nil when not leading
}

// NewOutboxRelay returns a handle that does not lead yet.
func (s *Store) NewOutboxRelay() *OutboxRelay { return &OutboxRelay{store: s} }

// TryLead makes this process the relay if no other process is; it reports whether it leads.
func (r *OutboxRelay) TryLead(ctx context.Context) (bool, error) {
	if r.conn != nil {
		return true, nil
	}
	pooled, err := r.store.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquiring a connection: %w", err)
	}
	conn := pooled.Hijack() // the lock lives as long as this session; keep it out of the pool
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, relayLockKey).Scan(&locked); err != nil || !locked {
		closeConn(conn)
		if err != nil {
			return false, fmt.Errorf("taking the relay lock: %w", err)
		}
		return false, nil
	}
	r.conn = conn
	return true, nil
}

// Resign gives up leadership by closing the leader session (which releases the lock).
func (r *OutboxRelay) Resign() {
	if r.conn != nil {
		closeConn(r.conn)
		r.conn = nil
	}
}

var errNotLeading = errors.New("outbox relay is not leading")

// Pending returns up to limit unrelayed events, oldest first.
func (r *OutboxRelay) Pending(ctx context.Context, limit int) ([]domain.OutboxEvent, error) {
	if r.conn == nil {
		return nil, errNotLeading
	}
	rows, err := r.conn.Query(ctx, `
		SELECT id, notification_id, batch_id, status::text, attempts, error, occurred_at
		FROM status_event_outbox ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("reading the outbox: %w", err)
	}
	evs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.OutboxEvent, error) {
		var (
			e      domain.OutboxEvent
			status string
		)
		err := row.Scan(&e.Seq, &e.Event.NotificationID, &e.Event.BatchID, &status, &e.Event.Attempts,
			&e.Event.Error, &e.Event.OccurredAt)
		e.Event.Status = domain.Status(status)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("scanning the outbox: %w", err)
	}
	return evs, nil
}

// Remove deletes relayed events. It names every event, never a range: a transaction that
// committed after Pending read may hold a lower id, and it has not been relayed yet.
func (r *OutboxRelay) Remove(ctx context.Context, seqs []int64) error {
	if r.conn == nil {
		return errNotLeading
	}
	if _, err := r.conn.Exec(ctx, `DELETE FROM status_event_outbox WHERE id = ANY($1)`, seqs); err != nil {
		return fmt.Errorf("removing relayed events: %w", err)
	}
	return nil
}

// OutboxBacklog reports how many status events wait to be relayed and for how long, on the
// database clock.
func (s *Store) OutboxBacklog(ctx context.Context) (domain.OutboxBacklog, error) {
	var (
		b      domain.OutboxBacklog
		count  int64
		oldest float64
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(extract(epoch FROM now() - min(occurred_at)), 0)
		FROM status_event_outbox`).Scan(&count, &oldest); err != nil {
		return b, fmt.Errorf("reading the outbox backlog: %w", err)
	}
	b.Pending, b.OldestAge = int(count), max(seconds(oldest), 0)
	return b, nil
}

// closeConn closes a connection taken out of the pool, ending its session.
func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}
