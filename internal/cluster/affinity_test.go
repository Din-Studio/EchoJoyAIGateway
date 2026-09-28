package cluster

import (
	"context"
	"testing"
	"time"

	"gpt-load/internal/affinity"
)

func newTestAffinity(t *testing.T) (*Affinity, *rpmClock, func(time.Duration)) {
	t.Helper()
	server, client := newTestClient(t)
	clock := &rpmClock{now: time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)}
	store := NewAffinity(client)
	store.now = clock.current
	advance := func(duration time.Duration) {
		clock.advance(duration)
		server.FastForward(duration)
	}
	return store, clock, advance
}

func mustLookupAffinity(t *testing.T, store *Affinity, policy affinity.Policy, key affinity.Key) affinity.Observation {
	t.Helper()
	observation, err := store.Lookup(context.Background(), policy, key)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	return observation
}

func mustRecordAffinity(
	t *testing.T,
	store *Affinity,
	policy affinity.Policy,
	key affinity.Key,
	observed affinity.Observation,
	target affinity.Target,
) bool {
	t.Helper()
	recorded, err := store.RecordSuccess(context.Background(), policy, key, observed, target)
	if err != nil {
		t.Fatalf("RecordSuccess() error = %v", err)
	}
	return recorded
}

// Mirrors affinity.TestCacheFirstSuccessWinsAndFallbackUsesCompareAndSwap.
func TestAffinityFirstSuccessWinsAndFallbackUsesCompareAndSwap(t *testing.T) {
	store, _, _ := newTestAffinity(t)
	policy := affinity.Policy{Revision: 1, Capacity: 4, TTL: time.Hour}
	key := affinity.Key("conversation")
	first := affinity.Target{GroupID: 1, CredentialID: 11, IdentityGeneration: 101}
	second := affinity.Target{GroupID: 1, CredentialID: 12, IdentityGeneration: 102}

	missOne := mustLookupAffinity(t, store, policy, key)
	missTwo := mustLookupAffinity(t, store, policy, key)
	if missOne.Found() {
		t.Fatalf("initial Lookup() = %#v, want miss", missOne)
	}
	if !mustRecordAffinity(t, store, policy, key, missOne, first) {
		t.Fatal("first miss success did not create mapping")
	}
	if mustRecordAffinity(t, store, policy, key, missTwo, second) {
		t.Fatal("second concurrent miss overwrote first-success mapping")
	}

	observedFirst := mustLookupAffinity(t, store, policy, key)
	if !observedFirst.Found() || observedFirst.Target != first {
		t.Fatalf("Lookup() = %#v, want first target", observedFirst)
	}
	if !mustRecordAffinity(t, store, policy, key, observedFirst, second) {
		t.Fatal("fallback success did not switch observed mapping")
	}
	if mustRecordAffinity(t, store, policy, key, observedFirst, first) {
		t.Fatal("stale success overwrote newer fallback mapping")
	}
	if got := mustLookupAffinity(t, store, policy, key); !got.Found() || got.Target != second {
		t.Fatalf("Lookup() = %#v, want fallback target %#v", got, second)
	}
}

// Mirrors affinity.TestCacheLearnsAndRefreshesSuccessfulTarget and
// TestCacheLookupDoesNotRefreshTTL.
func TestAffinityRefreshesOnSuccessAndExpiresAfterTTL(t *testing.T) {
	store, _, advance := newTestAffinity(t)
	policy := affinity.Policy{Revision: 1, Capacity: 2, TTL: time.Hour}
	key := affinity.Key("one")
	target := affinity.Target{GroupID: 1, CredentialID: 11, IdentityGeneration: 101}

	mustRecordAffinity(t, store, policy, key, mustLookupAffinity(t, store, policy, key), target)
	advance(50 * time.Minute)
	first := mustLookupAffinity(t, store, policy, key)
	if !first.Found() {
		t.Fatal("Lookup() before expiry = miss, want hit")
	}
	if !mustRecordAffinity(t, store, policy, key, first, target) {
		t.Fatal("RecordSuccess() = false, want TTL refresh")
	}
	advance(59 * time.Minute)
	if !mustLookupAffinity(t, store, policy, key).Found() {
		t.Fatal("Lookup() after refreshed TTL = miss, want hit")
	}
	advance(2 * time.Minute)
	if got := mustLookupAffinity(t, store, policy, key); got.Found() {
		t.Fatalf("Lookup() after expiry = %#v, want miss", got)
	}
}

func TestAffinityObservedMappingExpiredIsReplaced(t *testing.T) {
	store, _, advance := newTestAffinity(t)
	policy := affinity.Policy{Revision: 1, Capacity: 2, TTL: time.Hour}
	key := affinity.Key("one")
	first := affinity.Target{GroupID: 1, CredentialID: 11, IdentityGeneration: 101}
	second := affinity.Target{GroupID: 1, CredentialID: 12, IdentityGeneration: 102}

	mustRecordAffinity(t, store, policy, key, affinity.Observation{}, first)
	observed := mustLookupAffinity(t, store, policy, key)
	advance(2 * time.Hour)
	if !mustRecordAffinity(t, store, policy, key, observed, second) {
		t.Fatal("success after the observed mapping expired was rejected")
	}
	if got := mustLookupAffinity(t, store, policy, key); got.Target != second {
		t.Fatalf("Lookup() = %#v, want %#v", got, second)
	}
}

func TestAffinityShorterTTLAppliesToExistingMappings(t *testing.T) {
	store, _, advance := newTestAffinity(t)
	long := affinity.Policy{Revision: 1, Capacity: 2, TTL: time.Hour}
	short := affinity.Policy{Revision: 2, Capacity: 2, TTL: 10 * time.Minute}
	key := affinity.Key("one")
	first := affinity.Target{GroupID: 1, CredentialID: 11, IdentityGeneration: 101}
	second := affinity.Target{GroupID: 1, CredentialID: 12, IdentityGeneration: 102}

	mustRecordAffinity(t, store, long, key, affinity.Observation{}, first)
	advance(20 * time.Minute)
	if !mustLookupAffinity(t, store, long, key).Found() {
		t.Fatal("mapping missing under the original TTL")
	}
	miss := mustLookupAffinity(t, store, short, key)
	if miss.Found() {
		t.Fatalf("Lookup() under shorter TTL = %#v, want miss", miss)
	}
	if !mustRecordAffinity(t, store, short, key, miss, second) {
		t.Fatal("mapping older than the shorter TTL blocked a new success")
	}
	if got := mustLookupAffinity(t, store, short, key); got.Target != second {
		t.Fatalf("Lookup() = %#v, want %#v", got, second)
	}
}

func TestAffinityRejectsInvalidInputsAndReportsRedisErrors(t *testing.T) {
	server, client := newTestClient(t)
	store := NewAffinity(client)
	policy := affinity.Policy{Revision: 1, Capacity: 2, TTL: time.Hour}
	valid := affinity.Target{GroupID: 1, CredentialID: 11, IdentityGeneration: 101}
	for _, test := range []struct {
		key    affinity.Key
		target affinity.Target
	}{
		{target: valid},
		{key: "key"},
		{key: "key", target: affinity.Target{GroupID: 1, CredentialID: 11}},
	} {
		if mustRecordAffinity(t, store, policy, test.key, affinity.Observation{}, test.target) {
			t.Fatalf("RecordSuccess(%q, %#v) = true, want false", test.key, test.target)
		}
	}
	if err := server.Set("gl:aff:bad", "not-a-mapping"); err != nil {
		t.Fatal(err)
	}
	if got := mustLookupAffinity(t, store, policy, "bad"); got.Found() {
		t.Fatalf("malformed value Lookup() = %#v, want miss", got)
	}
	if !mustRecordAffinity(t, store, policy, "bad", affinity.Observation{}, valid) {
		t.Fatal("malformed value blocked a new success")
	}

	server.Close()
	if _, err := store.Lookup(context.Background(), policy, "key"); err == nil {
		t.Fatal("Lookup() error = nil with Redis down")
	}
	if _, err := store.RecordSuccess(context.Background(), policy, "key", affinity.Observation{}, valid); err == nil {
		t.Fatal("RecordSuccess() error = nil with Redis down")
	}
	if NewAffinity(nil) != nil {
		t.Fatal("NewAffinity(nil) != nil")
	}
}
