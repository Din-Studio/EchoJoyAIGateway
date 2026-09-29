package main

import (
	"net/http"
	"slices"
	"testing"
	"time"
)

func statuses(ok, limited, other int) []int {
	out := slices.Repeat([]int{http.StatusOK}, ok)
	out = append(out, slices.Repeat([]int{http.StatusTooManyRequests}, limited)...)
	return append(out, slices.Repeat([]int{http.StatusServiceUnavailable}, other)...)
}

func TestJudgeRPMRequiresExactlyTheLimit(t *testing.T) {
	cases := []struct {
		name     string
		statuses []int
		pass     bool
	}{
		{"exact", statuses(60, 140, 0), true},
		{"one over", statuses(61, 139, 0), false},
		{"one under", statuses(59, 141, 0), false},
		{"redis timeout counted as failure", statuses(60, 139, 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := judgeRPM(tc.statuses, 60); got.Pass != tc.pass {
				t.Fatalf("judgeRPM pass = %v, want %v (%s)", got.Pass, tc.pass, got.Measured)
			}
		})
	}
}

func TestJudgeQuotaRequiresRejectionRightAfterTheLimit(t *testing.T) {
	cases := []struct {
		name     string
		statuses []int
		used     float64
		pass     bool
	}{
		{"exact", statuses(11, 1, 0), 0.055, true},
		{"overshoot", statuses(12, 1, 0), 0.06, false},
		{"early rejection", statuses(10, 1, 0), 0.05, false},
		{"never rejected", statuses(11, 0, 0), 0.055, false},
		{"rejected with wrong status", statuses(11, 0, 1), 0.055, false},
		{"usage not recorded", statuses(11, 1, 0), 0.05, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := judgeQuota(tc.statuses, 11, tc.used, 0.0525); got.Pass != tc.pass {
				t.Fatalf("judgeQuota pass = %v, want %v (%s)", got.Pass, tc.pass, got.Measured)
			}
		})
	}
}

func TestJudgePropagationUsesTheSlowestInstance(t *testing.T) {
	if !judgePropagation([]time.Duration{10 * time.Millisecond, time.Second}, time.Second).Pass {
		t.Fatal("propagation at exactly the budget should pass")
	}
	if judgePropagation([]time.Duration{10 * time.Millisecond, 1001 * time.Millisecond}, time.Second).Pass {
		t.Fatal("one slow instance should fail propagation")
	}
	if judgePropagation(nil, time.Second).Pass {
		t.Fatal("no observations should fail propagation")
	}
}
