package delivery

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		status int
		err    error
		want   domain.AttemptOutcome
	}{
		{202, nil, domain.OutcomeSuccess},
		{200, nil, domain.OutcomeSuccess},
		{0, errors.New("connection refused"), domain.OutcomeRetryableError},
		{0, context.DeadlineExceeded, domain.OutcomeRetryableError},
		{500, nil, domain.OutcomeRetryableError},
		{503, nil, domain.OutcomeRetryableError},
		{429, nil, domain.OutcomeRetryableError},
		{408, nil, domain.OutcomeRetryableError},
		{400, nil, domain.OutcomePermanentError},
		{401, nil, domain.OutcomePermanentError},
		{404, nil, domain.OutcomePermanentError},
		{422, nil, domain.OutcomePermanentError},
		{301, nil, domain.OutcomePermanentError},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d/%v", tt.status, tt.err), func(t *testing.T) {
			if got := Classify(domain.ProviderReply{StatusCode: tt.status}, tt.err); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestRetryPolicy_Delay(t *testing.T) {
	p := RetryPolicy{Base: 2 * time.Second, Max: 5 * time.Minute}
	maxRand := func(n int64) int64 { return n - 1 } // upper edge of the jitter range
	zeroRand := func(int64) int64 { return 0 }

	tests := []struct {
		name       string
		attempt    int
		retryAfter time.Duration
		randN      func(int64) int64
		want       time.Duration
	}{
		{"first retry ceiling", 1, 0, maxRand, 2 * time.Second},
		{"doubles", 2, 0, maxRand, 4 * time.Second},
		{"doubles again", 4, 0, maxRand, 16 * time.Second},
		{"capped at max", 10, 0, maxRand, 5 * time.Minute},
		{"huge attempt does not overflow", 200, 0, maxRand, 5 * time.Minute},
		{"jitter lower edge is never zero", 3, 0, zeroRand, 1},
		{"retry-after wins", 1, 30 * time.Second, maxRand, 30 * time.Second},
		{"retry-after capped", 1, time.Hour, maxRand, 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Delay(tt.attempt, tt.retryAfter, tt.randN); got != tt.want {
				t.Errorf("Delay = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryPolicy_DelayStaysWithinCeiling(t *testing.T) {
	p := RetryPolicy{Base: 100 * time.Millisecond, Max: time.Second}
	var calls int64
	randN := func(n int64) int64 { calls++; return (calls * 7919) % n }
	for attempt := 1; attempt <= 8; attempt++ {
		ceiling := min(p.Base<<(attempt-1), p.Max)
		for range 100 {
			if d := p.Delay(attempt, 0, randN); d <= 0 || d > ceiling {
				t.Fatalf("attempt %d: delay %v outside (0, %v]", attempt, d, ceiling)
			}
		}
	}
}
