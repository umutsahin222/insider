package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

// readyChannel is the PostgreSQL NOTIFY channel on which migration 000006's triggers announce
// claimable work; the payload is the notification channel (sms, email, push).
const readyChannel = "notifications_ready"

const (
	listenRetryMin = time.Second
	listenRetryMax = 30 * time.Second
)

// ListenForReadyWork calls onReady with a channel whenever notifications of that channel
// become claimable right away: created, manually retried, released or reaped (ADR-011).
//
// It holds one dedicated connection and returns when ctx is done. If the connection breaks it
// reconnects with backoff; announcements made while disconnected are lost, so after every
// (re)connect it reports every channel once. Announcements are only hints: workers keep
// polling for work nobody announces, so a dead listener delays work but never loses it.
func (s *Store) ListenForReadyWork(ctx context.Context, onReady func(domain.Channel), log *slog.Logger) {
	retry := listenRetryMin
	for {
		connected, err := s.listen(ctx, onReady)
		if ctx.Err() != nil {
			return
		}
		if connected {
			retry = listenRetryMin
		}
		log.Warn("listening for ready work failed; workers rely on polling until it reconnects",
			"error", err, "retry_in", retry.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
		retry = min(retry*2, listenRetryMax)
	}
}

// listen runs one LISTEN session until it fails or ctx is done. connected reports whether
// LISTEN succeeded, so the caller can reset its backoff.
func (s *Store) listen(ctx context.Context, onReady func(domain.Channel)) (connected bool, err error) {
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquiring a connection: %w", err)
	}
	// LISTEN binds to the session: take the connection out of the pool so no query borrows it,
	// and close it when done instead of handing a listening session back.
	conn := pooled.Hijack()
	defer closeConn(conn)

	if _, err := conn.Exec(ctx, "LISTEN "+readyChannel); err != nil {
		return false, fmt.Errorf("listening on %s: %w", readyChannel, err)
	}
	for _, ch := range domain.Channels {
		onReady(ch) // anything announced before LISTEN took effect
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, fmt.Errorf("waiting for notifications: %w", err)
		}
		if ch, err := domain.ParseChannel(n.Payload); err == nil {
			onReady(ch)
		}
	}
}
