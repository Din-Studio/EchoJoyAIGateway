package cluster

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestRefreshLeaseGrantsOneHolder(t *testing.T) {
	server := miniredis.RunT(t)
	leases := []*RefreshLease{
		NewRefreshLease(newClientForServer(t, server, "node-a")),
		NewRefreshLease(newClientForServer(t, server, "node-b")),
	}
	var acquired atomic.Int32
	var releases sync.Map
	var wait sync.WaitGroup
	for index := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			release, ok, err := leases[index%2].Acquire(t.Context(), 7)
			if err != nil {
				t.Errorf("Acquire() error = %v", err)
				return
			}
			if ok {
				acquired.Add(1)
				releases.Store(index, release)
			}
		}()
	}
	wait.Wait()
	if acquired.Load() != 1 {
		t.Fatalf("acquired %d leases, want 1", acquired.Load())
	}
	releases.Range(func(_, value any) bool {
		value.(func())()
		return true
	})
	release, ok, err := leases[1].Acquire(t.Context(), 7)
	if err != nil || !ok {
		t.Fatalf("Acquire() after release = %t, %v", ok, err)
	}
	release()
}

func TestRefreshLeaseRenewsWhileHeldAndExpiresAfterStop(t *testing.T) {
	server := miniredis.RunT(t)
	lease := NewRefreshLease(newClientForServer(t, server, "node-a"))
	lease.renewInterval = 5 * time.Millisecond
	other := NewRefreshLease(newClientForServer(t, server, "node-b"))
	key := lease.key(9)

	release, ok, err := lease.Acquire(t.Context(), 9)
	if err != nil || !ok {
		t.Fatalf("Acquire() = %t, %v", ok, err)
	}
	owner, _ := server.Get(key)
	for range 9 {
		server.FastForward(10 * time.Second)
		time.Sleep(30 * time.Millisecond)
	}
	if !server.Exists(key) {
		t.Fatal("renewed lease expired after 90s")
	}

	// A non-holder's release leaves the lease alone.
	otherRelease, otherOK, _ := other.Acquire(t.Context(), 9)
	if otherOK {
		otherRelease()
		t.Fatal("second holder acquired a live lease")
	}
	if got, _ := server.Get(key); got != owner {
		t.Fatalf("lease owner = %q, want %q", got, owner)
	}

	release()
	if server.Exists(key) {
		t.Fatal("release left the lease behind")
	}
}

func TestRefreshLeaseExpiresWithoutRenewalAndIgnoresStaleRelease(t *testing.T) {
	server := miniredis.RunT(t)
	crashedHolder := NewRefreshLease(newClientForServer(t, server, "node-a"))
	crashedHolder.renewInterval = time.Hour // a crashed holder never renews
	other := NewRefreshLease(newClientForServer(t, server, "node-b"))

	crashed, ok, err := crashedHolder.Acquire(t.Context(), 10)
	if err != nil || !ok {
		t.Fatalf("Acquire(10) = %t, %v", ok, err)
	}
	held, err := other.Held(t.Context(), []uint{10, 11})
	if err != nil || !held[10] || held[11] {
		t.Fatalf("Held() = %#v, %v; want only 10 held", held, err)
	}
	server.FastForward(31 * time.Second)
	if held, err = other.Held(t.Context(), []uint{10}); err != nil || held[10] {
		t.Fatalf("Held() after expiry = %#v, %v", held, err)
	}
	takeover, ok, err := other.Acquire(t.Context(), 10)
	if err != nil || !ok {
		t.Fatalf("Acquire() after expiry = %t, %v", ok, err)
	}
	// The expired holder's release must not delete the new holder's lease.
	crashed()
	if !server.Exists(other.key(10)) {
		t.Fatal("stale release deleted another holder's lease")
	}
	takeover()
	if server.Exists(other.key(10)) {
		t.Fatal("release left the lease behind")
	}
}

func TestRefreshLeaseFailsWhenRedisIsDown(t *testing.T) {
	server := miniredis.RunT(t)
	lease := NewRefreshLease(newClientForServer(t, server, "node-a"))
	server.Close()
	if _, _, err := lease.Acquire(t.Context(), 1); err == nil {
		t.Fatal("Acquire() succeeded against a closed Redis")
	}
	if _, err := lease.Held(t.Context(), []uint{1}); err == nil {
		t.Fatal("Held() succeeded against a closed Redis")
	}
}
