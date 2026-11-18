// Package delivery runs the per-channel workers: claim → rate-limit → send → record (ADR-002).
package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

// Classify maps the provider's answer to an attempt outcome (ADR-002):
//
//	2xx                      → success
//	transport error / timeout → retryable
//	408, 429, 5xx            → retryable
//	any other status         → permanent (the request itself is wrong; retrying won't help)
func Classify(reply domain.ProviderReply, err error) domain.AttemptOutcome {
	if err != nil {
		return domain.OutcomeRetryableError
	}
	switch s := reply.StatusCode; {
	case s >= 200 && s < 300:
		return domain.OutcomeSuccess
	case s == http.StatusRequestTimeout, s == http.StatusTooManyRequests, s >= 500:
		return domain.OutcomeRetryableError
	default:
		return domain.OutcomePermanentError
	}
}

// describe renders a short, human-readable reason for last_error / the attempt log.
func describe(reply domain.ProviderReply, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "provider timed out"
	case err != nil:
		return err.Error()
	default:
		return fmt.Sprintf("provider responded %d %s", reply.StatusCode, http.StatusText(reply.StatusCode))
	}
}

// RetryPolicy computes the delay before the next attempt.
type RetryPolicy struct {
	Base time.Duration // delay cap for the first retry
	Max  time.Duration // overall cap
}

// Delay returns the wait before retrying after the given (1-based) attempt failed.
// It uses exponential backoff with full jitter: rand(0, min(Base·2^(attempt-1), Max)).
// A provider-requested Retry-After wins over the computed backoff, still capped at Max.
// randN must return a uniform value in [0, n).
func (p RetryPolicy) Delay(attempt int, retryAfter time.Duration, randN func(n int64) int64) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, p.Max)
	}
	ceiling := p.Max
	if shift := attempt - 1; shift >= 0 && shift < 32 {
		if d := p.Base << shift; d > 0 && d < p.Max {
			ceiling = d
		}
	}
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(randN(int64(ceiling)) + 1)
}
