package ratelimit

import (
	"testing"
	"time"
)

func TestNew_MarginStretchesTheSlotInterval(t *testing.T) {
	tests := []struct {
		name   string
		rate   float64
		margin time.Duration
		want   time.Duration
	}{
		{"no margin", 100, 0, 10 * time.Millisecond},
		{"10 ms margin", 100, 10 * time.Millisecond, 10100 * time.Microsecond},
		{"50 ms margin: 95.2/s", 100, 50 * time.Millisecond, 10500 * time.Microsecond},
		{"negative margin counts as none", 100, -time.Millisecond, 10 * time.Millisecond},
		{"rounded up, never below the limit", 3, 0, 333334 * time.Microsecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := New(nil, tt.rate, 1, tt.margin)
			if got := time.Duration(l.interval) * time.Microsecond; got != tt.want {
				t.Errorf("interval = %v, want %v", got, tt.want)
			}
			// 1 + rate slots must span at least 1 s + margin, whatever the rounding.
			if span := time.Duration(int64(tt.rate)*l.interval) * time.Microsecond; span < time.Second+max(tt.margin, 0) {
				t.Errorf("%v slots span %v, want at least %v", tt.rate+1, span, time.Second+tt.margin)
			}
		})
	}
}
