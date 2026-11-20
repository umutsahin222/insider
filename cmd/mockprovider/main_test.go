package main

import (
	"testing"
	"time"
)

func TestPeakTracker_CountsTheBusiestSecond(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		spacing time.Duration
		want    int
	}{
		{"10 ms apart: 100 in any second", 10 * time.Millisecond, 100},
		{"9 ms apart: 112 in the busiest second", 9 * time.Millisecond, 112},
		{"20 ms apart: 50", 20 * time.Millisecond, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newPeakTracker()
			for i := range 300 {
				p.observe("sms", start.Add(time.Duration(i)*tt.spacing))
			}
			p.observe("email", start)
			got := p.snapshot()
			if got["sms"] != tt.want || got["email"] != 1 {
				t.Errorf("peaks = %v, want sms %d and email 1", got, tt.want)
			}
		})
	}
}
