package cluster

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

var testAuthPolicy = AuthFailurePolicy{Window: 30 * time.Minute, Limit: 5, LockDuration: 30 * time.Minute}

func newAuthFailuresPair(t *testing.T) (*miniredis.Miniredis, *AuthFailures, *AuthFailures) {
	t.Helper()
	server := miniredis.RunT(t)
	return server,
		NewAuthFailures(newClientForServer(t, server, "node-a")),
		NewAuthFailures(newClientForServer(t, server, "node-b"))
}

func evaluateAuth(t *testing.T, failures *AuthFailures, peer string, valid bool, now time.Time) AuthFailureDecision {
	t.Helper()
	decision, err := failures.Evaluate(t.Context(), peer, valid, testAuthPolicy, now)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	return decision
}

func TestAuthFailuresLockOnFifthFailureAcrossInstances(t *testing.T) {
	server, first, second := newAuthFailuresPair(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	instances := []*AuthFailures{first, second, first, second}
	for attempt, failures := range instances {
		if decision := evaluateAuth(t, failures, "192.0.2.1", false, now); decision != (AuthFailureDecision{}) {
			t.Fatalf("failure %d decision = %#v, want no lock", attempt+1, decision)
		}
	}
	locked := evaluateAuth(t, first, "192.0.2.1", false, now)
	if !locked.NewlyLocked || locked.RetryAfter != 30*time.Minute {
		t.Fatalf("fifth failure decision = %#v, want new 30m lock", locked)
	}
	still := evaluateAuth(t, second, "192.0.2.1", false, now)
	if still.NewlyLocked || still.RetryAfter <= 0 {
		t.Fatalf("locked retry on the other instance = %#v, want remaining lock", still)
	}
	if server.Exists("gl:{auth:192.0.2.1}:fail") {
		t.Fatal("failures survived the lock transition")
	}

	server.FastForward(30 * time.Minute)
	if decision := evaluateAuth(t, second, "192.0.2.1", false, now.Add(30*time.Minute)); decision != (AuthFailureDecision{}) {
		t.Fatalf("failure after the lock expired = %#v, want a fresh count", decision)
	}
}

func TestAuthFailuresUseRollingWindow(t *testing.T) {
	_, first, second := newAuthFailuresPair(t)
	start := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for minute := range 4 {
		evaluateAuth(t, first, "192.0.2.1", false, start.Add(time.Duration(minute)*time.Minute))
	}
	if decision := evaluateAuth(t, second, "192.0.2.1", false, start.Add(30*time.Minute)); decision.RetryAfter != 0 {
		t.Fatalf("failure at the exact cutoff = %#v, want the first failure expired", decision)
	}
	if decision := evaluateAuth(t, second, "192.0.2.1", false, start.Add(30*time.Minute)); !decision.NewlyLocked {
		t.Fatalf("next failure inside the window = %#v, want lock", decision)
	}
}

func TestAuthFailuresValidCredentialClearsPeerEverywhere(t *testing.T) {
	server, first, second := newAuthFailuresPair(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for range 5 {
		evaluateAuth(t, first, "192.0.2.1", false, now)
	}
	for range 5 {
		evaluateAuth(t, first, "192.0.2.2", false, now)
	}
	if decision := evaluateAuth(t, second, "192.0.2.1", true, now); decision != (AuthFailureDecision{}) {
		t.Fatalf("valid credential decision = %#v, want clear", decision)
	}
	if decision := evaluateAuth(t, first, "192.0.2.1", false, now); decision != (AuthFailureDecision{}) {
		t.Fatalf("failure after a valid credential = %#v, want a fresh count", decision)
	}
	if decision := evaluateAuth(t, first, "192.0.2.2", false, now); decision.RetryAfter <= 0 {
		t.Fatalf("other peer = %#v, want its lock preserved", decision)
	}
	if ttl := server.TTL("gl:{auth:192.0.2.1}:fail"); ttl <= 0 || ttl > 30*time.Minute {
		t.Fatalf("failure set TTL = %s, want bounded by the window", ttl)
	}
}

func TestAuthFailuresReportRedisErrors(t *testing.T) {
	server, first, _ := newAuthFailuresPair(t)
	server.Close()
	if _, err := first.Evaluate(t.Context(), "192.0.2.1", false, testAuthPolicy, time.Now()); err == nil {
		t.Fatal("Evaluate() with Redis down succeeded")
	}
}
