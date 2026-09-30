package affinity

import (
	"testing"
	"time"
)

func TestCacheLearnsAndRefreshesSuccessfulTarget(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	cache := newCache(2, time.Hour, func() time.Time { return now })
	key := Key("one")
	target := Target{GroupID: 1, CredentialID: 11, IdentityGeneration: 101}

	miss := cache.lookup(key)
	if miss.Found() {
		t.Fatalf("initial Lookup() = %#v, want miss", miss)
	}
	if !cache.recordSuccess(key, miss, target) {
		t.Fatal("RecordSuccess() = false, want insert")
	}
	first := cache.lookup(key)
	if !first.Found() || first.Target != target {
		t.Fatalf("Lookup() = %#v, want target %#v", first, target)
	}

	now = now.Add(50 * time.Minute)
	if !cache.recordSuccess(key, first, target) {
		t.Fatal("RecordSuccess() = false, want TTL refresh")
	}
	now = now.Add(20 * time.Minute)
	if refreshed := cache.lookup(key); !refreshed.Found() || refreshed.Target != target {
		t.Fatalf("Lookup() after refreshed TTL = %#v, want hit", refreshed)
	}
}

func TestCacheLookupDoesNotRefreshTTL(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	cache := newCache(2, time.Hour, func() time.Time { return now })
	key := Key("one")
	target := Target{GroupID: 1, CredentialID: 11, IdentityGeneration: 101}
	cache.recordSuccess(key, Observation{}, target)

	now = now.Add(50 * time.Minute)
	if !cache.lookup(key).Found() {
		t.Fatal("Lookup() before expiry = miss, want hit")
	}
	now = now.Add(11 * time.Minute)
	if got := cache.lookup(key); got.Found() {
		t.Fatalf("Lookup() after original expiry = %#v, want miss", got)
	}
}

func TestCacheFirstSuccessWinsAndFallbackUsesCompareAndSwap(t *testing.T) {
	cache := newCache(4, time.Hour, time.Now)
	key := Key("conversation")
	first := Target{GroupID: 1, CredentialID: 11, IdentityGeneration: 101}
	second := Target{GroupID: 1, CredentialID: 12, IdentityGeneration: 102}

	missOne := cache.lookup(key)
	missTwo := cache.lookup(key)
	if !cache.recordSuccess(key, missOne, first) {
		t.Fatal("first miss success did not create mapping")
	}
	if cache.recordSuccess(key, missTwo, second) {
		t.Fatal("second concurrent miss overwrote first-success mapping")
	}

	observedFirst := cache.lookup(key)
	if !cache.recordSuccess(key, observedFirst, second) {
		t.Fatal("fallback success did not switch observed mapping")
	}
	if cache.recordSuccess(key, observedFirst, first) {
		t.Fatal("stale success overwrote newer fallback mapping")
	}
	got := cache.lookup(key)
	if !got.Found() || got.Target != second {
		t.Fatalf("Lookup() = %#v, want fallback target %#v", got, second)
	}
}

func TestCacheEvictsLeastRecentlyUsedEntry(t *testing.T) {
	cache := newCache(2, time.Hour, time.Now)
	target := Target{GroupID: 1, CredentialID: 11, IdentityGeneration: 101}
	for _, key := range []Key{"one", "two"} {
		cache.recordSuccess(key, Observation{}, target)
	}
	cache.lookup("one")
	cache.recordSuccess("three", Observation{}, target)

	if cache.lookup("two").Found() {
		t.Fatal("least recently used entry remained cached")
	}
	if !cache.lookup("one").Found() || !cache.lookup("three").Found() || cache.entryCount() != 2 {
		t.Fatalf("cache state invalid after eviction; entries=%d", cache.entryCount())
	}
}

func TestCacheRejectsInvalidInputs(t *testing.T) {
	cache := newCache(2, time.Hour, time.Now)
	validTarget := Target{GroupID: 1, CredentialID: 11, IdentityGeneration: 101}
	for _, test := range []struct {
		key    Key
		target Target
	}{
		{target: validTarget},
		{key: "key"},
		{key: "key", target: Target{GroupID: 1, CredentialID: 11}},
	} {
		if cache.recordSuccess(test.key, Observation{}, test.target) {
			t.Fatalf("RecordSuccess(%q, %#v) = true, want false", test.key, test.target)
		}
	}
	if cache.lookup("").Found() || cache.entryCount() != 0 {
		t.Fatalf("invalid inputs changed cache; entries=%d", cache.entryCount())
	}
}

func TestCacheConfigureClearsEntriesAndRejectsOlderRevision(t *testing.T) {
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	cache := newCache(2, time.Hour, func() time.Time { return now })
	key := Key("one")
	target := Target{GroupID: 1, CredentialID: 11, IdentityGeneration: 101}
	if !cache.Configure(1, 2, time.Hour) {
		t.Fatal("Configure(1) = false")
	}
	observed := cache.lookup(key)
	if !cache.recordSuccess(key, observed, target) {
		t.Fatal("RecordSuccess() = false")
	}
	stale := cache.lookup(key)
	if !cache.Configure(2, 1, 30*time.Minute) {
		t.Fatal("Configure(2) = false")
	}
	if cache.lookup(key).Found() || cache.entryCount() != 0 {
		t.Fatal("new configuration did not clear old entries")
	}
	if cache.Configure(1, 2, time.Hour) {
		t.Fatal("older configuration revision was accepted")
	}
	if cache.recordSuccess(key, stale, target) {
		t.Fatal("request from an older configuration revision rewrote the cache")
	}
	for _, nextKey := range []Key{"two", "three"} {
		if !cache.recordSuccess(nextKey, cache.lookup(nextKey), target) {
			t.Fatalf("RecordSuccess(%q) = false", nextKey)
		}
	}
	if cache.lookup("two").Found() || !cache.lookup("three").Found() || cache.entryCount() != 1 {
		t.Fatal("configured capacity was not enforced")
	}
	now = now.Add(31 * time.Minute)
	if cache.lookup("three").Found() {
		t.Fatal("configured TTL was not enforced")
	}
}
