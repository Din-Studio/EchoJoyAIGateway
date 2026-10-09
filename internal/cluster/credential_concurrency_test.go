package cluster

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newConcurrencyInstance(t *testing.T, server *miniredis.Miniredis, instanceID string) *CredentialConcurrency {
	t.Helper()
	cfg := clusterTestConfig(t, server.Addr())
	cfg.Cluster.InstanceID = instanceID
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	limiter := NewCredentialConcurrency(client)
	// Tests drive renewal explicitly through renewHeld.
	limiter.renewInterval = time.Hour
	return limiter
}

func mustAcquireSlot(t *testing.T, limiter *CredentialConcurrency, credentialID uint, limit int, want bool) func() {
	t.Helper()
	release, acquired, err := limiter.Acquire(t.Context(), credentialID, limit)
	if err != nil {
		t.Fatalf("Acquire(%d, %d) error = %v", credentialID, limit, err)
	}
	if acquired != want {
		t.Fatalf("Acquire(%d, %d) acquired = %v, want %v", credentialID, limit, acquired, want)
	}
	return release
}

func TestCredentialConcurrencyLimitIsSharedAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	first := newConcurrencyInstance(t, server, "node-a")
	second := newConcurrencyInstance(t, server, "node-b")

	releaseFirst := mustAcquireSlot(t, first, 7, 2, true)
	releaseSecond := mustAcquireSlot(t, second, 7, 2, true)
	mustAcquireSlot(t, first, 7, 2, false)
	mustAcquireSlot(t, second, 7, 2, false)
	// Other credentials are counted separately.
	mustAcquireSlot(t, second, 8, 1, true)

	releaseFirst()
	releaseFirst() // idempotent: must not free a second slot
	mustAcquireSlot(t, second, 7, 2, true)
	mustAcquireSlot(t, first, 7, 2, false)
	releaseSecond()
	mustAcquireSlot(t, first, 7, 2, true)
}

func TestCredentialConcurrencyExpiresUnrenewedSlotsAndKeepsRenewedOnes(t *testing.T) {
	server := miniredis.RunT(t)
	now := time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)
	server.SetTime(now)
	crashed := newConcurrencyInstance(t, server, "node-crashed")
	alive := newConcurrencyInstance(t, server, "node-alive")
	other := newConcurrencyInstance(t, server, "node-other")

	mustAcquireSlot(t, crashed, 1, 1, true)
	mustAcquireSlot(t, alive, 2, 1, true)

	for range 4 {
		now = now.Add(20 * time.Second)
		server.SetTime(now)
		if !alive.renewHeld() {
			t.Fatal("renewHeld() reported nothing held while a slot is in flight")
		}
	}

	// 80 s later: the crashed instance never renewed, so its slot is gone.
	mustAcquireSlot(t, other, 1, 1, true)
	// The live instance renewed every 20 s, so its slot is still held.
	mustAcquireSlot(t, other, 2, 1, false)
}

func TestCredentialConcurrencyRenewalStopsWhenNothingIsHeld(t *testing.T) {
	server := miniredis.RunT(t)
	limiter := newConcurrencyInstance(t, server, "node-a")
	release := mustAcquireSlot(t, limiter, 3, 1, true)
	release()
	if limiter.renewHeld() {
		t.Fatal("renewHeld() kept running with no slot held")
	}
	limiter.mu.Lock()
	renewing := limiter.renewing
	limiter.mu.Unlock()
	if renewing {
		t.Fatal("renewal loop still marked running after the last release")
	}
}

func TestCredentialConcurrencyUnlimitedSkipsRedis(t *testing.T) {
	server := miniredis.RunT(t)
	limiter := newConcurrencyInstance(t, server, "node-a")
	server.Close()
	release, acquired, err := limiter.Acquire(t.Context(), 4, 0)
	if err != nil || !acquired || release == nil {
		t.Fatalf("Acquire(unlimited, Redis down) = %v, %v", acquired, err)
	}
	release()
}

func TestCredentialConcurrencyFailsWhenRedisIsDown(t *testing.T) {
	server := miniredis.RunT(t)
	limiter := newConcurrencyInstance(t, server, "node-a")
	server.Close()
	started := time.Now()
	if _, _, err := limiter.Acquire(t.Context(), 5, 3); err == nil {
		t.Fatal("Acquire() with Redis down error = nil")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Acquire() with Redis down took %v, want about %v", elapsed, stateTimeout)
	}
}
