package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPercentileUsesNearestRank(t *testing.T) {
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(i+1) * time.Millisecond
	}
	if got := percentile(samples, 0.99); got != 99*time.Millisecond {
		t.Fatalf("p99 = %s, want 99ms", got)
	}
	if got := percentile(samples, 0.50); got != 50*time.Millisecond {
		t.Fatalf("p50 = %s, want 50ms", got)
	}
	if got := percentile(nil, 0.99); got != 0 {
		t.Fatalf("empty p99 = %s, want 0", got)
	}
}

func TestOpenLoopHoldsTheRequestedRate(t *testing.T) {
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := openLoop(t.Context(), newDataPlaneClient(), server.URL, "secret", 200, 2*time.Second)
	if result.errors != 0 || result.samples != 400 || served.Load() != 400 {
		t.Fatalf("open loop = %+v, served %d, want 400 successful requests", result, served.Load())
	}
	if result.p50 < 5*time.Millisecond {
		t.Fatalf("p50 = %s, want at least the handler delay", result.p50)
	}
}

func TestBenchChecksApplyThePRDTargets(t *testing.T) {
	single := throughputResult{rps: 1000}
	fast := latencyResult{p50: 50 * time.Millisecond, p99: 60 * time.Millisecond}
	checks := benchChecks(single, throughputResult{rps: 2500}, fast, latencyResult{p50: 51 * time.Millisecond, p99: 63 * time.Millisecond})
	if !allPass(checks) {
		t.Fatalf("targets exactly met should pass: %+v", checks)
	}
	checks = benchChecks(single, throughputResult{rps: 2499}, fast, latencyResult{p99: 63*time.Millisecond + time.Microsecond})
	if checks[0].Pass || checks[2].Pass {
		t.Fatalf("missed targets should fail: %+v", checks)
	}
}
