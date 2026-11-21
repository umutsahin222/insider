package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

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
	Channel     domain.Channel
	Concurrency int           // claim/send loops for this channel
	BatchSize   int           // rows claimed per loop iteration
	Lease       time.Duration // how long a claim is owned before the reaper may take it back
	// PollInterval is the idle wait when the queue is empty. It bounds how late work that
	// nobody announces is picked up (a scheduled time or retry backoff falling due); new and
	// re-queued work wakes idle loops at once through Deps.Wake.
	PollInterval time.Duration
	SendTimeout  time.Duration // per provider call
	Retry        RetryPolicy
	Breaker      BreakerPolicy
}

// StoreTimeout bounds each queue write after a send (MarkSent, ScheduleRetry, MarkFailed,
// Release). Those run detached from shutdown so an attempt is never cut in half, so they need
// their own deadline: a stalled database must not keep the process from exiting.
const StoreTimeout = 10 * time.Second

// Validate checks the invariants the worker relies on.
func (c Config) Validate() error {
	var errs []error
	if c.Concurrency < 1 || c.BatchSize < 1 {
		errs = append(errs, errors.New("concurrency and batch size must be positive"))
	}
	if c.PollInterval <= 0 || c.SendTimeout <= 0 || c.Retry.Base <= 0 || c.Retry.Max < c.Retry.Base {
		errs = append(errs, errors.New("poll interval, send timeout and retry base must be positive, and retry max >= retry base"))
	}
	// Each send must be able to start, finish and be recorded within the claim's lease.
	if c.Lease <= c.SendTimeout+StoreTimeout {
		errs = append(errs, fmt.Errorf("lease (%s) must exceed send timeout + %s", c.Lease, StoreTimeout))
	}
	return errors.Join(errs...)
}

// Worker delivers the notifications of a single channel. Each channel has its own worker,
// limiter bucket and circuit breaker, so a slow or failing channel never blocks the others.
type Worker struct {
	cfg        Config
	queue      Queue
	sender     Sender
	limiter    Limiter
	metrics    Metrics
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	log        *slog.Logger
	wake       *Signal
	breaker    *gobreaker.CircuitBreaker[domain.ProviderReply]
	randN      func(int64) int64
	// probing is set while this process's single half-open probe is between booking a
	// rate-limit slot and finishing its send; other loops release instead of booking slots.
	probing atomic.Bool
}

// Deps are a worker's collaborators. Metrics and Tracing are optional. There is no event
// publisher: the database records a status event with every transition (ADR-012).
type Deps struct {
	Queue   Queue
	Sender  Sender
	Limiter Limiter
	Metrics Metrics
	// Tracing continues the trace stored with each notification; nil records nothing.
	Tracing *observability.Tracing
	Logger  *slog.Logger
	// Wake fires when this channel has work that is claimable right away (ADR-011); idle
	// loops then claim at once instead of waiting out PollInterval. Nil relies on polling.
	Wake *Signal
}

// errRetryable marks a send the breaker should count as a failure.
var errRetryable = errors.New("retryable provider failure")

// NewWorker wires a channel worker.
func NewWorker(cfg Config, d Deps) *Worker {
	m := d.Metrics
	if m == nil {
		m = NopMetrics{}
	}
	tracing := d.Tracing
	if tracing == nil {
		tracing = observability.DisabledTracing()
	}
	log := d.Logger.With("channel", string(cfg.Channel))
	w := &Worker{
		cfg: cfg, queue: d.Queue, sender: d.Sender, limiter: d.Limiter, metrics: m,
		tracer: tracing.Provider.Tracer(observability.TracerName), propagator: tracing.Propagator,
		log: log, wake: d.Wake, randN: rand.Int64N,
	}
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
		// Taken before the claim: a wake-up that arrives while the claim runs is not missed.
		wake := w.wake.C()
		// Taken before the claim, so it is never later than the lease's start on the database.
		claimedAt := time.Now()
		batch, err := w.queue.Claim(ctx, w.cfg.Channel, w.cfg.BatchSize, w.cfg.Lease)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Error("claim failed", "error", err)
				w.sleep(ctx, time.Second)
			}
			continue
		}
		if len(batch) == 0 {
			w.idle(ctx, wake)
			continue
		}
		sendBy := w.sendDeadline(claimedAt)
		for i, n := range batch {
			if ctx.Err() != nil {
				w.releaseAll(batch[i:], 0, ReleaseShuttingDown)
				return
			}
			w.deliver(ctx, n, sendBy)
		}
	}
}

// sendDeadline is the latest moment a row claimed at claimedAt may start its provider call:
// the send and the write that records it must both finish before the lease expires, or the
// reaper may already have handed the row to another worker (a duplicate send). Rows of a
// batch wait for rate-limit slots one after another, so late rows can reach it.
func (w *Worker) sendDeadline(claimedAt time.Time) time.Time {
	return claimedAt.Add(w.cfg.Lease - w.cfg.SendTimeout - StoreTimeout)
}

// process delivers one notification claimed just now.
func (w *Worker) process(ctx context.Context, n domain.Notification) {
	w.deliver(ctx, n, w.sendDeadline(time.Now()))
}

func notificationContext(ctx context.Context, n domain.Notification) context.Context {
	return observability.WithNotificationID(observability.WithCorrelationID(ctx, n.CorrelationID), n.ID.String())
}

// deliver sends one claimed notification, unless its lease could expire first (sendBy),
// and records the outcome.
func (w *Worker) deliver(ctx context.Context, n domain.Notification, sendBy time.Time) {
	ctx = notificationContext(ctx, n)

	// Continue the trace of the request that created the notification: every attempt is a
	// consumer span under it, so retries line up in one trace.
	ctx = observability.ExtractTraceContext(ctx, w.propagator, n.TraceContext)
	ctx, span := w.tracer.Start(ctx, "deliver "+string(w.cfg.Channel),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("notification.id", n.ID.String()),
			attribute.String("notification.channel", string(n.Channel)),
			attribute.String("notification.priority", n.Priority.String()),
			attribute.Int("notification.attempt", n.Attempts),
			attribute.Int("notification.max_attempts", n.MaxAttempts),
		))
	defer span.End()

	// The breaker may have opened while this claimed batch was being worked through: hand the
	// rest back before booking a rate-limit slot that another replica could have used.
	switch w.breaker.State() {
	case gobreaker.StateOpen:
		w.release(ctx, n, w.cfg.Breaker.OpenTimeout, ReleaseBreakerOpen)
		return
	case gobreaker.StateHalfOpen:
		// The breaker admits one probe; only the loop that will make it books a slot.
		if !w.probing.CompareAndSwap(false, true) {
			w.release(ctx, n, w.cfg.PollInterval, ReleaseBreakerHalfOpen)
			return
		}
		defer w.probing.Store(false)
	}

	if !time.Now().Before(sendBy) {
		w.release(ctx, n, 0, ReleaseLeaseExpiring)
		return
	}
	waitStart := time.Now()
	waitCtx, cancelWait := context.WithDeadline(ctx, sendBy)
	_, waitSpan := w.tracer.Start(waitCtx, "ratelimit.wait")
	err := w.limiter.Wait(waitCtx, string(w.cfg.Channel))
	waitSpan.End()
	cancelWait()
	if err != nil {
		switch {
		case ctx.Err() != nil:
			w.release(ctx, n, 0, ReleaseShuttingDown)
		case waitCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
			// The next rate-limit slot comes too late: the lease could expire mid-send (the
			// limiter says so up front instead of waiting). Hand the row back.
			w.release(ctx, n, 0, ReleaseLeaseExpiring)
		default:
			// Fail closed: without the shared limiter we cannot prove we're under the provider limit.
			w.log.ErrorContext(ctx, "rate limiter unavailable", "error", err)
			w.release(ctx, n, time.Second, ReleaseLimiterDown)
		}
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
	storeCtx, cancelStore := context.WithTimeout(workCtx, StoreTimeout)
	defer cancelStore()
	switch {
	case outcome == domain.OutcomeSuccess:
		result = ResultSent
		var msgID *string
		if reply.MessageID != "" {
			msgID = &reply.MessageID
		}
		storeErr = w.queue.MarkSent(storeCtx, n, msgID, attempt)
		w.log.InfoContext(ctx, "notification sent", "attempt", n.Attempts, "duration_ms", duration.Milliseconds())

	case outcome == domain.OutcomeRetryableError && n.Attempts < n.MaxAttempts:
		result = ResultRetry
		delay := w.cfg.Retry.Delay(n.Attempts, reply.RetryAfter, w.randN)
		storeErr = w.queue.ScheduleRetry(storeCtx, n, delay, attempt)
		w.log.WarnContext(ctx, "delivery failed, retry scheduled",
			"attempt", n.Attempts, "max_attempts", n.MaxAttempts, "retry_in", delay.String(), "reason", *attempt.Error)

	default: // permanent error, or retryable with no attempts left
		result = ResultFailed
		storeErr = w.queue.MarkFailed(storeCtx, n, attempt)
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

	span.SetAttributes(attribute.String("delivery.outcome", string(outcome)), attribute.String("delivery.result", string(result)))
	if attempt.StatusCode != nil {
		span.SetAttributes(attribute.Int("http.response.status_code", *attempt.StatusCode))
	}
	if outcome != domain.OutcomeSuccess {
		span.SetStatus(codes.Error, *attempt.Error)
	}
}

func (w *Worker) release(ctx context.Context, n domain.Notification, delay time.Duration, reason ReleaseReason) {
	w.metrics.Released(w.cfg.Channel, reason)
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("delivery.result", "released"), attribute.String("delivery.release_reason", string(reason)))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StoreTimeout)
	defer cancel()
	if err := w.queue.Release(ctx, n, delay); err != nil && !errors.Is(err, domain.ErrLeaseLost) {
		w.log.ErrorContext(ctx, "release failed", "error", err, "reason", string(reason))
	}
}

func (w *Worker) releaseAll(ns []domain.Notification, delay time.Duration, reason ReleaseReason) {
	for _, n := range ns {
		w.release(notificationContext(context.Background(), n), n, delay, reason)
	}
}

// sleep waits for d plus up to 20% jitter, or until ctx is done.
func (w *Worker) sleep(ctx context.Context, d time.Duration) {
	w.waitFor(ctx, d, nil)
}

// idle waits for new work after an empty claim: a wake-up, or PollInterval for work that
// nobody announces (scheduled times and retry backoffs falling due).
func (w *Worker) idle(ctx context.Context, wake <-chan struct{}) {
	w.waitFor(ctx, w.cfg.PollInterval, wake)
}

// waitFor waits for d plus up to 20% jitter, until wake is closed, or until ctx is done.
func (w *Worker) waitFor(ctx context.Context, d time.Duration, wake <-chan struct{}) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d + time.Duration(w.randN(int64(d)/5+1)))
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	case <-wake:
	}
}
