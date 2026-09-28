package affinity

import (
	"container/list"
	"context"
	"sync"
	"time"
)

const (
	DefaultTTL      = time.Hour
	DefaultCapacity = 10_000
)

// Target is the exact Credential identity remembered as a soft preference.
type Target struct {
	GroupID            uint
	CredentialID       uint
	IdentityGeneration uint64
}

func (target Target) Valid() bool {
	return target.GroupID != 0 && target.CredentialID != 0 && target.IdentityGeneration != 0
}

// Policy is the frozen affinity configuration of one published snapshot.
type Policy struct {
	Revision uint64
	Capacity int
	TTL      time.Duration
}

// Valid reports whether the policy enables affinity at all.
func (policy Policy) Valid() bool {
	return policy.Revision != 0 && policy.Capacity > 0 && policy.TTL > 0
}

// Observation is a versioned cache lookup used for conditional success updates.
type Observation struct {
	Target   Target
	key      Key
	revision uint64
	version  uint64
	found    bool
	// token is the stored value a shared store compares before overwriting.
	token string
}

func (observation Observation) Found() bool {
	return observation.found
}

// SharedObservation records what a shared store saw for key. An empty token
// means no live mapping was observed.
func SharedObservation(key Key, target Target, token string) Observation {
	return Observation{Target: target, key: key, found: token != "", token: token}
}

// Token returns the stored value observed by a shared store.
func (observation Observation) Token() string {
	return observation.token
}

type cacheEntry struct {
	key       Key
	target    Target
	version   uint64
	expiresAt time.Time
}

// Cache is a bounded, process-local soft-affinity cache.
type Cache struct {
	mu          sync.Mutex
	entries     map[Key]*list.Element
	recent      list.List
	capacity    int
	ttl         time.Duration
	now         func() time.Time
	revision    uint64
	nextVersion uint64
}

func NewCache() *Cache {
	return newCache(DefaultCapacity, DefaultTTL, time.Now)
}

func newCache(capacity int, ttl time.Duration, now func() time.Time) *Cache {
	return &Cache{
		entries:  make(map[Key]*list.Element),
		capacity: capacity,
		ttl:      ttl,
		now:      now,
	}
}

// Configure applies one frozen runtime configuration revision. Moving to a
// newer revision clears entries so changed TTL, capacity, and group policy
// take effect atomically. Older requests cannot restore stale configuration.
func (cache *Cache) Configure(revision uint64, capacity int, ttl time.Duration) bool {
	if cache == nil || !(Policy{Revision: revision, Capacity: capacity, TTL: ttl}).Valid() || cache.now == nil {
		return false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if revision < cache.revision {
		return false
	}
	if revision == cache.revision {
		return cache.capacity == capacity && cache.ttl == ttl
	}
	cache.revision = revision
	cache.capacity = capacity
	cache.ttl = ttl
	cache.entries = make(map[Key]*list.Element)
	cache.recent.Init()
	return true
}

// Lookup applies policy and returns the current mapping for key. A policy the
// cache cannot apply, such as an older revision, disables affinity for the
// request. The in-process cache never fails.
func (cache *Cache) Lookup(_ context.Context, policy Policy, key Key) (Observation, error) {
	if !cache.Configure(policy.Revision, policy.Capacity, policy.TTL) {
		return Observation{}, nil
	}
	return cache.lookup(key), nil
}

// RecordSuccess conditionally learns target under policy; see recordSuccess.
func (cache *Cache) RecordSuccess(_ context.Context, policy Policy, key Key, observed Observation, target Target) (bool, error) {
	if !cache.Configure(policy.Revision, policy.Capacity, policy.TTL) {
		return false, nil
	}
	return cache.recordSuccess(key, observed, target), nil
}

func (cache *Cache) lookup(key Key) Observation {
	if cache == nil || !key.Valid() || cache.now == nil {
		return Observation{}
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	element := cache.currentElementLocked(key, cache.now())
	if element == nil {
		return Observation{key: key, revision: cache.revision}
	}
	cache.recent.MoveToFront(element)
	entry := element.Value.(*cacheEntry)
	return Observation{
		Target: entry.target, key: key, revision: cache.revision,
		version: entry.version, found: true,
	}
}

// recordSuccess conditionally learns the successful target. It returns true
// when this call inserted, refreshed, or changed the current mapping.
func (cache *Cache) recordSuccess(key Key, observed Observation, target Target) bool {
	if cache == nil || !key.Valid() || !target.Valid() || cache.now == nil {
		return false
	}
	if observed.key.Valid() && observed.key != key {
		return false
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.capacity <= 0 || cache.ttl <= 0 || observed.revision != cache.revision {
		return false
	}
	now := cache.now()
	element := cache.currentElementLocked(key, now)
	if observed.found {
		if element != nil {
			current := element.Value.(*cacheEntry)
			if current.version != observed.version || current.target != observed.Target {
				return false
			}
		} else {
			return cache.insertLocked(key, target, now)
		}
	} else if element != nil {
		return false
	}

	if element == nil {
		return cache.insertLocked(key, target, now)
	}
	cache.nextVersion++
	entry := element.Value.(*cacheEntry)
	entry.target = target
	entry.version = cache.nextVersion
	entry.expiresAt = now.Add(cache.ttl)
	cache.recent.MoveToFront(element)
	return true
}

func (cache *Cache) entryCount() int {
	if cache == nil {
		return 0
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.entries)
}

func (cache *Cache) currentElementLocked(key Key, now time.Time) *list.Element {
	element := cache.entries[key]
	if element == nil {
		return nil
	}
	entry := element.Value.(*cacheEntry)
	if !now.Before(entry.expiresAt) {
		cache.removeLocked(element)
		return nil
	}
	return element
}

func (cache *Cache) insertLocked(key Key, target Target, now time.Time) bool {
	for len(cache.entries) >= cache.capacity {
		oldest := cache.recent.Back()
		if oldest == nil {
			break
		}
		cache.removeLocked(oldest)
	}
	cache.nextVersion++
	entry := &cacheEntry{
		key: key, target: target, version: cache.nextVersion,
		expiresAt: now.Add(cache.ttl),
	}
	cache.entries[key] = cache.recent.PushFront(entry)
	return true
}

func (cache *Cache) removeLocked(element *list.Element) {
	entry := element.Value.(*cacheEntry)
	delete(cache.entries, entry.key)
	cache.recent.Remove(element)
}
