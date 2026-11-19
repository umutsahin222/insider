package delivery

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/umutsahin222/insider/internal/domain"
	"github.com/umutsahin222/insider/internal/observability"
)

// Queue is the work queue the worker consumes; implemented by store/postgres.
// Durations (not absolute times) are passed so the queue runs on the database clock.
type Queue interface {
	Claim(ctx context.Context, ch domain.Channel, limit int, lease time.Duration) ([]domain.Notification, error)
	MarkSent(ctx context.Context, n domain.Notification, providerMessageID *string, a domain.DeliveryAttempt) error
	ScheduleRetry(ctx context.Context, n domain.Notification, delay time.Duration, a domain.DeliveryAttempt) error
	MarkFailed(ctx context.Context, n domain.Notification, a domain.DeliveryAttempt) error
	Release(ctx context.Context, n domain.Notification, delay time.Duration) error
}

// Sender delivers one notification to the external provider; implemented by provider.Client.
type Sender interface {
	Send(ctx context.Context, n domain.Notification) (domain.ProviderReply, error)
}

// Limiter blocks until the caller may send one message for key; implemented by ratelimit.
type Limiter interface {
	Wait(ctx context.Context, key string) error
}

// Config tunes one channel's worker pool.
type Config struct {
	Channel      domain.Channel
	Concurrency  int           // claim/send loops for this channel
	BatchSize    int           // rows claimed per loop iteration
	Lease        time.Duration // how long a claim is owned before the reaper may take it back
	PollInterval time.Duration // idle wait when the queue is empty
	SendTimeout  time.Duration // per provider call
	Retry        RetryPolicy
	Breaker      BreakerPolicy
}

// Worker delivers the notifications of a single channel. Each channel has its own worker,
// limiter bucket and circuit breaker, so a slow or failing channel never blocks the others.
type Worker struct {
	cfg     Config
	queue   Queue
	sender  Sender
	limiter Limiter
	metrics Metrics
	log     *slog.Logger
	breaker *gobreaker.CircuitBreaker[domain.ProviderReply]
	randN   func(int64) int64
}

// errRetryable marks a send the breaker should count as a failure.
var errRetryable = errors.New("retryable provider failure")

// NewWorker wires a channel worker. A nil m disables metrics.
func NewWorker(cfg Config, q Queue, s Sender, l Limiter, m Metrics, log *slog.Logger) *Worker {
	if m == nil {
		m = NopMetrics{}
	}
	log = log.With("channel", string(cfg.Channel))
	w := &Worker{cfg: cfg, queue: q, sender: s, limiter: l, metrics: m, log: log, randN: rand.Int64N}
	w.breaker = gobreaker.NewCircuitBreaker[domain.ProviderReply](gobreaker.Settings{
		Name:        string(cfg.Channel),
		MaxRequests: 1, // a single probe in half-open state
		Interval:    cfg.Breaker.Window,
		Timeout:     cfg.Breaker.OpenTimeout,
		ReadyToTrip: cfg.Breaker.shouldTrip,
		OnStateChange: func(name string, from, to gobreaker.State) {
			log.Warn("circuit breaker state changed", "from", from.String(), "to", to.String())
			m.BreakerStateChanged(cfg.Channel, breakerState(to))
		},
	})
	m.BreakerStateChanged(cfg.Channel, BreakerClosed)
	return w
}

func breakerState(s gobreaker.State) BreakerState {
	switch s {
	case gobreaker.StateOpen:
		return BreakerOpen
	case gobreaker.StateHalfOpen:
		return BreakerHalfOpen
	default:
		return BreakerClosed
	}
}

// Run processes the channel until ctx is cancelled, then waits for in-flight sends to finish.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("worker started", "concurrency", w.cfg.Concurrency)
	var wg sync.WaitGroup
	for range w.cfg.Concurrency {
		wg.Go(func() { w.loop(ctx) })
	}
	wg.Wait()
	w.log.Info("worker stopped")
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		// While the breaker is open, leave work in the queue instead of burning attempts.
		if w.breaker.State() == gobreaker.StateOpen {
			w.sleep(ctx, w.cfg.PollInterval)
			continue
		}
		batch, err := w.queue.Claim(ctx, w.cfg.Channel, w.cfg.BatchSize, w.cfg.Lease)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Error("claim failed", "error", err)
				w.sleep(ctx, time.Second)
			}
			continue
		}
		if len(batch) == 0 {
			w.sleep(ctx, w.cfg.PollInterval)
			continue
		}
		for i, n := range batch {
			if ctx.Err() != nil {
				w.releaseAll(batch[i:], 0, ReleaseShuttingDown)
				return
			}
			w.process(ctx, n)
		}
	}
}

// process delivers one claimed notification and records the outcome.
func (w *Worker) process(ctx context.Context, n domain.Notification) {
	ctx = observability.WithNotificationID(observability.WithCorrelationID(ctx, n.CorrelationID), n.ID.String())

	// The breaker may have opened while this claimed batch was being worked through: hand the
	// rest back before taking a rate-limit token that another replica could have used.
	if w.breaker.State() == gobreaker.StateOpen {
		w.release(ctx, n, w.cfg.Breaker.OpenTimeout, ReleaseBreakerOpen)
		return
	}

	waitStart := time.Now()
	if err := w.limiter.Wait(ctx, string(w.cfg.Channel)); err != nil {
		// Fail closed: without the shared limiter we cannot prove we're under the provider limit.
		delay := time.Second
		if ctx.Err() != nil {
			delay = 0
		} else {
			w.log.ErrorContext(ctx, "rate limiter unavailable", "error", err)
		}
		w.release(ctx, n, delay, ReleaseLimiterDown)
		return
	}
	w.metrics.RateLimitWaited(w.cfg.Channel, time.Since(waitStart))

	// From here on, finish the attempt even if shutdown starts: the provider call and the
	// state change must not be cut in half.
	workCtx := context.WithoutCancel(ctx)
	sendCtx, cancel := context.WithTimeout(workCtx, w.cfg.SendTimeout)
	start := time.Now()
	var sendErr error
	reply, err := w.breaker.Execute(func() (domain.ProviderReply, error) {
		r, err := w.sender.Send(sendCtx, n)
		sendErr = err
		if Classify(r, err) == domain.OutcomeRetryableError {
			return r, errRetryable
		}
		return r, nil
	})
	duration := time.Since(start)
	cancel()

	switch {
	case errors.Is(err, gobreaker.ErrOpenState):
		w.release(workCtx, n, w.cfg.Breaker.OpenTimeout, ReleaseBreakerOpen)
		return
	case errors.Is(err, gobreaker.ErrTooManyRequests): // half-open, and its single probe is in flight
		w.release(workCtx, n, w.cfg.PollInterval, ReleaseBreakerHalfOpen)
		return
	}

	outcome := Classify(reply, sendErr)
	attempt := domain.DeliveryAttempt{NotificationID: n.ID, Attempt: n.Attempts, Outcome: outcome, Duration: duration}
	if sendErr == nil {
		code := reply.StatusCode
		attempt.StatusCode = &code
	}
	if outcome != domain.OutcomeSuccess {
		msg := describe(reply, sendErr)
		attempt.Error = &msg
	}

	var (
		storeErr error
		result   Result
	)
	switch {
	case outcome == domain.OutcomeSuccess:
		result = ResultSent
		var msgID *string
		if reply.MessageID != "" {
			msgID = &reply.MessageID
		}
		storeErr = w.queue.MarkSent(workCtx, n, msgID, attempt)
		w.log.InfoContext(ctx, "notification sent", "attempt", n.Attempts, "duration_ms", duration.Milliseconds())

	case outcome == domain.OutcomeRetryableError && n.Attempts < n.MaxAttempts:
		result = ResultRetry
		delay := w.cfg.Retry.Delay(n.Attempts, reply.RetryAfter, w.randN)
		storeErr = w.queue.ScheduleRetry(workCtx, n, delay, attempt)
		w.log.WarnContext(ctx, "delivery failed, retry scheduled",
			"attempt", n.Attempts, "max_attempts", n.MaxAttempts, "retry_in", delay.String(), "reason", *attempt.Error)

	default: // permanent error, or retryable with no attempts left
		result = ResultFailed
		storeErr = w.queue.MarkFailed(workCtx, n, attempt)
		w.log.ErrorContext(ctx, "delivery failed permanently",
			"attempt", n.Attempts, "max_attempts", n.MaxAttempts, "outcome", string(outcome), "reason", *attempt.Error)
	}

	switch {
	case errors.Is(storeErr, domain.ErrLeaseLost):
		result = ResultLeaseLost
		w.log.WarnContext(ctx, "lease lost before recording the outcome; newer attempt owns the notification")
	case storeErr != nil:
		result = ResultStoreError
		// The lease will expire and the reaper will return the row to the queue.
		w.log.ErrorContext(ctx, "recording outcome failed", "error", storeErr)
	case result == ResultSent:
		// Worker clock vs. database timestamps: clamp the rare, small skew to zero.
		w.metrics.Delivered(w.cfg.Channel, max(time.Since(dueAt(n)), 0))
	}
	w.metrics.AttemptFinished(w.cfg.Channel, outcome, result, duration)
}

func (w *Worker) release(ctx context.Context, n domain.Notification, delay time.Duration, reason ReleaseReason) {
	w.metrics.Released(w.cfg.Channel, reason)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := w.queue.Release(ctx, n, delay); err != nil && !errors.Is(err, domain.ErrLeaseLost) {
		w.log.ErrorContext(ctx, "release failed", "error", err, "notification_id", n.ID.String(), "reason", string(reason))
	}
}

func (w *Worker) releaseAll(ns []domain.Notification, delay time.Duration, reason ReleaseReason) {
	for _, n := range ns {
		w.release(context.Background(), n, delay, reason)
	}
}

// sleep waits for d plus up to 20% jitter, or until ctx is done.
func (w *Worker) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d + time.Duration(w.randN(int64(d)/5+1)))
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
