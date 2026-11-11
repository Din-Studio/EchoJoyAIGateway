package cluster

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gpt-load/internal/codexrecovery"
)

func TestInvalidLineageSharedIdentityAndIdleTTL(t *testing.T) {
	server, client := newTestClient(t)
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	server.SetTime(now)
	a, b := NewInvalidLineage(client), NewInvalidLineage(client)
	key := codexrecovery.InvalidLineageKey(1, 2, "tenant-session")
	digest := codexrecovery.EncryptedContentDigest("opaque")
	if n, err := a.Mark(t.Context(), key, []string{digest, digest}, []string{"msg_old"}); err != nil || n != 2 {
		t.Fatalf("mark=%d %v", n, err)
	}
	for _, other := range []string{codexrecovery.InvalidLineageKey(2, 2, "tenant-session"), codexrecovery.InvalidLineageKey(1, 3, "tenant-session"), codexrecovery.InvalidLineageKey(1, 2, "other-tenant-session")} {
		d, ids, err := b.Lookup(t.Context(), other)
		if err != nil || len(d)+len(ids) != 0 {
			t.Fatalf("scope leak %v %v %v", d, ids, err)
		}
	}
	for range 2 {
		server.FastForward(59 * time.Minute)
		now = now.Add(59 * time.Minute)
		server.SetTime(now)
		d, ids, err := b.Lookup(t.Context(), key)
		if err != nil || len(d) != 1 || len(ids) != 1 {
			t.Fatalf("shared renewal %v %v %v", d, ids, err)
		}
		if _, ok := d[digest]; !ok {
			t.Fatal("missing digest")
		}
	}
	server.FastForward(61 * time.Minute)
	server.SetTime(now.Add(61 * time.Minute))
	d, ids, err := a.Lookup(t.Context(), key)
	if err != nil || len(d)+len(ids) != 0 {
		t.Fatalf("expired %v %v %v", d, ids, err)
	}
	server.Close()
	if _, _, err := a.Lookup(t.Context(), key); err == nil {
		t.Fatal("Redis failure hidden")
	}
	if _, err := a.Mark(t.Context(), key, []string{digest}, nil); err == nil {
		t.Fatal("Redis write failure hidden")
	}
}

func TestInvalidLineageConcurrentUnionAndLongSession(t *testing.T) {
	_, client := newTestClient(t)
	a, b := NewInvalidLineage(client), NewInvalidLineage(client)
	var wg sync.WaitGroup
	for i, store := range []*InvalidLineage{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			values := make([]string, 200)
			for j := range values {
				values[j] = fmt.Sprintf("%d-%d", i, j)
			}
			if n, err := store.Mark(t.Context(), "session", values, nil); err != nil || n != 200 {
				t.Errorf("mark %d %v", n, err)
			}
		}()
	}
	wg.Wait()
	d, _, err := b.Lookup(t.Context(), "session")
	if err != nil || len(d) != 400 {
		t.Fatalf("union %d %v", len(d), err)
	}
	if n, err := b.Mark(t.Context(), "session", []string{"0-0"}, nil); err != nil || n != 0 {
		t.Fatalf("duplicate %d %v", n, err)
	}
}

func TestInvalidLineageClusterBoundsAndExpiryAccounting(t *testing.T) {
	server, client := newTestClient(t)
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	server.SetTime(now)
	store := NewInvalidLineage(client)
	// Small limits exercise exactly the production script without allocating
	// 100k entries in the in-process Redis interpreter.
	run := func(key string, values ...string) int {
		t.Helper()
		args := []any{"mark", invalidLineageTTL.Milliseconds(), 2, 4}
		for _, v := range values {
			args = append(args, "d:"+v)
		}
		n, err := invalidLineageScript.Run(t.Context(), client.UniversalClient, store.keys(key), args...).Int()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if run("a", "1", "2", "3") != 3 || run("b", "4", "5") != 1 {
		t.Fatal("global record cap not enforced")
	}
	// Renew b so a is unambiguously oldest, then admit c and evict a.
	now = now.Add(time.Second)
	server.SetTime(now)
	if _, _, err := store.Lookup(t.Context(), "b"); err != nil {
		t.Fatal(err)
	}
	if run("c", "6", "7") != 2 {
		t.Fatal("eviction did not release record capacity")
	}
	if n := client.Exists(t.Context(), store.keys("a")[2]).Val(); n != 0 {
		t.Fatal("evicted payload remains")
	}
	if total := client.HGet(t.Context(), store.keys("c")[1], "!total").Val(); total != "3" {
		t.Fatalf("total=%s", total)
	}
	// Keep global metadata alive through c while b expires.
	server.FastForward(59 * time.Minute)
	now = now.Add(59 * time.Minute)
	server.SetTime(now)
	if _, _, err := store.Lookup(t.Context(), "c"); err != nil {
		t.Fatal(err)
	}
	server.FastForward(2 * time.Minute)
	now = now.Add(2 * time.Minute)
	server.SetTime(now)
	if run("d", "8", "9", "10") != 2 {
		t.Fatal("expiration did not release capacity")
	}
	if total := client.HGet(t.Context(), store.keys("d")[1], "!total").Val(); total != "4" {
		t.Fatalf("expired total=%s", total)
	}
	if _, err := client.ZScore(t.Context(), store.keys("b")[0], store.keys("b")[2]).Result(); err != redis.Nil {
		t.Fatalf("expired index: %v", err)
	}
}
