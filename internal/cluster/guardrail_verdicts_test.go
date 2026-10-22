package cluster

import (
	"context"
	"testing"
	"time"
)

func TestGuardrailVerdictsKeepFixedTTLAndAccessKeyIsolation(t *testing.T) {
	server, client := newTestClient(t)
	store := NewGuardrailVerdicts(client, time.Hour)
	ctx := context.Background()
	pass, verdict, missing := [32]byte{1}, [32]byte{2}, [32]byte{3}
	if err := store.Store(ctx, 7, map[[32]byte]float64{pass: 0, verdict: 0.93}); err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	found, err := store.Lookup(ctx, 7, [][32]byte{pass, verdict, missing})
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if len(found) != 2 || found[pass] != 0 || found[verdict] != 0.93 {
		t.Fatalf("Lookup() = %v, want pass=0 verdict=0.93", found)
	}
	if other, err := store.Lookup(ctx, 8, [][32]byte{pass, verdict}); err != nil || len(other) != 0 {
		t.Fatalf("another AccessKey saw the evidence: %v, %v", other, err)
	}

	// 读取不延长有效期：59 分钟时仍在，再读一次后到 1 小时即过期。
	server.FastForward(59 * time.Minute)
	if found, _ := store.Lookup(ctx, 7, [][32]byte{verdict}); len(found) != 1 {
		t.Fatal("evidence expired before its TTL")
	}
	server.FastForward(time.Minute)
	if found, _ := store.Lookup(ctx, 7, [][32]byte{verdict}); len(found) != 0 {
		t.Fatal("reading extended the evidence TTL")
	}
}

func TestGuardrailVerdictsReportRedisFailure(t *testing.T) {
	server, client := newTestClient(t)
	store := NewGuardrailVerdicts(client, time.Hour)
	server.Close()
	if _, err := store.Lookup(context.Background(), 7, [][32]byte{{1}}); err == nil {
		t.Fatal("Lookup() hid a Redis failure")
	}
	if err := store.Store(context.Background(), 7, map[[32]byte]float64{{1}: 0}); err == nil {
		t.Fatal("Store() hid a Redis failure")
	}
}
