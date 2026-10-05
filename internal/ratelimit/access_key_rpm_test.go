package ratelimit

import (
	"testing"
	"time"
)

func TestRetryAfterRoundsUpAndClampsToOneMinuteWindow(t *testing.T) {
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		target time.Time
		want   time.Duration
	}{
		{name: "rounds up partial seconds", target: now.Add(-30*time.Second - 500*time.Millisecond), want: 30 * time.Second},
		{name: "entry admitted now", target: now, want: time.Minute},
		{name: "entry already expired", target: now.Add(-time.Minute), want: time.Second},
		{name: "sub-second remainder", target: now.Add(-59*time.Second - 900*time.Millisecond), want: time.Second},
		{name: "future entry", target: now.Add(10 * time.Second), want: time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := RetryAfter(test.target, now); got != test.want {
				t.Fatalf("RetryAfter() = %v, want %v", got, test.want)
			}
		})
	}
}
