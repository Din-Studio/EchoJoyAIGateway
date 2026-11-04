package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestProxyTestConcurrencySharesEightIndependentSlots(t *testing.T) {
	server := miniredis.RunT(t)
	instances := []*Concurrency{
		newConcurrencyInstance(t, server, "proxy-a"),
		newConcurrencyInstance(t, server, "proxy-b"),
		newConcurrencyInstance(t, server, "proxy-c"),
	}
	var releases []func()
	for index := range 8 {
		release, acquired, err := instances[index%3].AcquireProxyTest(t.Context())
		if err != nil || !acquired {
			t.Fatalf("slot %d: acquired=%v err=%v", index, acquired, err)
		}
		releases = append(releases, release)
		t.Cleanup(release)
	}
	for _, instance := range instances {
		if _, acquired, err := instance.AcquireProxyTest(t.Context()); err != nil || acquired {
			t.Fatalf("ninth probe: acquired=%v err=%v", acquired, err)
		}
		if count, err := instance.GlobalInflight(t.Context()); err != nil || count != 0 {
			t.Fatalf("probes occupied business slots: count=%d err=%v", count, err)
		}
	}
	// Business capacity remains independent in both directions.
	business, acquired, err := instances[0].AcquireRequest(t.Context(), 7, 1, 1)
	if err != nil || !acquired {
		t.Fatalf("business slot: acquired=%v err=%v", acquired, err)
	}
	defer business()
	releases[0]()
	releases[0]()
	replacement, acquired, err := instances[2].AcquireProxyTest(t.Context())
	if err != nil || !acquired {
		t.Fatalf("released slot: acquired=%v err=%v", acquired, err)
	}
	defer replacement()
	if _, acquired, err := instances[1].AcquireProxyTest(t.Context()); err != nil || acquired {
		t.Fatalf("idempotent release freed two slots: acquired=%v err=%v", acquired, err)
	}
}

func TestProxyTestConcurrencyCancellationAndRedisFailure(t *testing.T) {
	server := miniredis.RunT(t)
	instance := newConcurrencyInstance(t, server, "proxy-a")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, acquired, err := instance.AcquireProxyTest(ctx); err == nil || acquired {
		t.Fatalf("canceled acquire: acquired=%v err=%v", acquired, err)
	}
	release, acquired, err := instance.AcquireProxyTest(t.Context())
	if err != nil || !acquired {
		t.Fatal("canceled acquire consumed capacity")
	}
	release()
	if instance.renewHeld() {
		t.Fatal("released probe remained in renewal bookkeeping")
	}
	server.Close()
	started := time.Now()
	if _, acquired, err := instance.AcquireProxyTest(t.Context()); err == nil || acquired {
		t.Fatalf("Redis failure: acquired=%v err=%v", acquired, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Redis failure took %s", elapsed)
	}
}

func TestProxyTestConcurrencyLeaseRenewal(t *testing.T) {
	server := miniredis.RunT(t)
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	server.SetTime(now)
	first := newConcurrencyInstance(t, server, "proxy-a")
	second := newConcurrencyInstance(t, server, "proxy-b")
	for range 8 {
		release, acquired, err := first.AcquireProxyTest(t.Context())
		if err != nil || !acquired {
			t.Fatal("acquire failed")
		}
		defer release()
	}
	for range 3 {
		now = now.Add(20 * time.Second)
		server.SetTime(now)
		if !first.renewHeld() {
			t.Fatal("renewal dropped live probes")
		}
	}
	if _, acquired, err := second.AcquireProxyTest(t.Context()); err != nil || acquired {
		t.Fatalf("renewed slots lost: acquired=%v err=%v", acquired, err)
	}
	now = now.Add(concurrencyTTL + time.Second)
	server.SetTime(now)
	release, acquired, err := second.AcquireProxyTest(t.Context())
	if err != nil || !acquired {
		t.Fatalf("expired slots not reclaimed: acquired=%v err=%v", acquired, err)
	}
	release()
}
