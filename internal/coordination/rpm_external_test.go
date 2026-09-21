package coordination

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gpt-load/internal/ratelimit"
)

// TestExternalRedisAccessKeyRPMSharesOneWindow is opt-in so ordinary unit
// tests stay hermetic. It proves the property the whole type exists for: a
// limit of N stays N no matter how many instances enforce it.
func TestExternalRedisAccessKeyRPMSharesOneWindow(t *testing.T) {
	client := externalLeaseClient(t)
	accessKeyID := externalAccessKeyID(t, client)

	first := NewAccessKeyRPM(client, "instance-a")
	second := NewAccessKeyRPM(client, "instance-b")

	const limit = 5
	admitted := 0
	// Alternating instances is what makes the count meaningful: with a
	// per-instance window each would admit `limit` on its own.
	for range limit * 2 {
		for _, limiter := range []*AccessKeyRPM{first, second} {
			if limiter.Allow(accessKeyID, limit).Allowed {
				admitted++
			}
		}
	}
	if admitted != limit {
		t.Fatalf("admitted = %d across two instances, want %d", admitted, limit)
	}
}

// Concurrency is where a read-then-write limiter breaks: two instances both
// read `limit-1` and both admit. The script has to make that impossible.
func TestExternalRedisAccessKeyRPMAdmitsExactlyTheLimitUnderConcurrency(t *testing.T) {
	client := externalLeaseClient(t)
	accessKeyID := externalAccessKeyID(t, client)

	const (
		limit     = 20
		instances = 4
		attempts  = 25
	)
	limiters := make([]*AccessKeyRPM, 0, instances)
	for index := range instances {
		limiters = append(limiters, NewAccessKeyRPM(client, string(rune('a'+index))))
	}

	var admitted atomic.Int64
	start := make(chan struct{})
	done := make(chan struct{})
	for _, limiter := range limiters {
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			for range attempts {
				if limiter.Allow(accessKeyID, limit).Allowed {
					admitted.Add(1)
				}
			}
		}()
	}
	close(start)
	for range limiters {
		<-done
	}
	if got := admitted.Load(); got != limit {
		t.Fatalf("admitted = %d under %d concurrent instances, want %d", got, instances, limit)
	}
}

// A rejection has to date itself by the request that must age out, or clients
// would be told to retry at a moment when no slot has freed up.
func TestExternalRedisAccessKeyRPMDatesItsRejection(t *testing.T) {
	client := externalLeaseClient(t)
	accessKeyID := externalAccessKeyID(t, client)

	limiter := NewAccessKeyRPM(client, "instance-a")
	base := time.Now()
	limiter.now = func() time.Time { return base }
	if decision := limiter.Allow(accessKeyID, 1); !decision.Allowed {
		t.Fatalf("first request = %#v, want admitted", decision)
	}

	// Half the window later, the only counted request still has 30s to go.
	limiter.now = func() time.Time { return base.Add(30 * time.Second) }
	decision := limiter.Allow(accessKeyID, 1)
	if decision.Allowed || decision.Unavailable {
		t.Fatalf("second request = %#v, want a decided rejection", decision)
	}
	if decision.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want 30s", decision.RetryAfter)
	}

	// Once it ages out the slot is free again, without any cleanup pass.
	limiter.now = func() time.Time { return base.Add(ratelimit.Window + time.Millisecond) }
	if decision := limiter.Allow(accessKeyID, 1); !decision.Allowed {
		t.Fatalf("request after the window = %#v, want admitted", decision)
	}
}

// An unreachable Redis must refuse rather than admit: the count lives only
// there, and admitting would restore the per-instance multiplication silently.
func TestExternalRedisAccessKeyRPMFailsClosedWhenRedisIsGone(t *testing.T) {
	client := externalLeaseClient(t)
	accessKeyID := externalAccessKeyID(t, client)
	if decision := NewAccessKeyRPM(client, "instance-a").Allow(accessKeyID, 10); !decision.Allowed {
		t.Fatalf("request against a live Redis = %#v, want admitted", decision)
	}

	// A second connection is severed so the live one still cleans up the key.
	// It owns its own close, so it is opened without the shared helper's.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	severed, err := Open(ctx, redisTestDSN(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := severed.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	limiter := NewAccessKeyRPM(severed, "instance-b")
	decision := limiter.Allow(accessKeyID, 10)
	if decision.Allowed {
		t.Fatal("Allow() admitted against a closed client; the shared count is unknown")
	}
	if !decision.Unavailable {
		t.Fatal("Unavailable = false; the caller cannot tell an outage from a real limit")
	}

	// An unlimited key never needed Redis, so an outage must not touch it.
	if decision := limiter.Allow(accessKeyID, 0); !decision.Allowed || decision.Unavailable {
		t.Fatalf("unlimited key = %#v, want admitted without consulting Redis", decision)
	}
}

// externalAccessKeyID namespaces the window so parallel CI jobs cannot consume
// each other's limits.
func externalAccessKeyID(t *testing.T, client *Client) uint {
	t.Helper()
	accessKeyID := uint(time.Now().UnixNano() % 1_000_000_007)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Redis().Del(ctx, accessKeyRPMKey(accessKeyID)).Err(); err != nil {
			t.Logf("delete test window key: %v", err)
		}
	})
	return accessKeyID
}
