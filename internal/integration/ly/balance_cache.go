package ly

import (
	"context"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// balanceCacheTTL is how long a replica reuses a balance that allows
// requests. Lingying accepts the overdraft a user can run up in that window.
const balanceCacheTTL = 5 * time.Second

type cachedBalance struct {
	value     balance
	expiresAt time.Time
}

// balanceCache keeps the balances that allow requests and joins concurrent
// lookups of one credential into a single call. Every other answer is fetched
// again on each lookup, so a rejection always reflects Lingying's current
// state.
type balanceCache struct {
	mu      sync.Mutex
	entries map[string]cachedBalance
	sweptAt time.Time
	group   singleflight.Group
}

type balanceFetch func(context.Context, Credential) (int, balance, error)

type fetchedBalance struct {
	status int
	value  balance
}

func newBalanceCache() *balanceCache {
	return &balanceCache{entries: map[string]cachedBalance{}}
}

// lookup returns what fetch returns, from the cache while a balance that
// allows requests is fresh. The shared call outlives a caller that gives up,
// so one cancelled request does not fail the others waiting on it; its own
// timeout bounds it.
func (c *balanceCache) lookup(ctx context.Context, now time.Time, credential Credential, fetch balanceFetch) (int, balance, error) {
	key := cacheKey(credential)
	if value, ok := c.get(now, key); ok {
		return http.StatusOK, value, nil
	}
	shared := c.group.DoChan(key, func() (any, error) {
		status, value, err := fetch(context.WithoutCancel(ctx), credential)
		if err == nil && allows(status, value) {
			c.put(now, key, value)
		}
		return fetchedBalance{status: status, value: value}, err
	})
	select {
	case <-ctx.Done():
		return 0, balance{}, ctx.Err()
	case result := <-shared:
		fetched := result.Val.(fetchedBalance)
		return fetched.status, fetched.value, result.Err
	}
}

// cacheKey identifies a credential; exactly one of its fields is set.
func cacheKey(credential Credential) string {
	if credential.APIKey != "" {
		return "k:" + credential.APIKey
	}
	return "u:" + credential.UserID
}

func (c *balanceCache) get(now time.Time, key string) (balance, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || !now.Before(entry.expiresAt) {
		return balance{}, false
	}
	return entry.value, true
}

// put stores an allowing balance and, at most once per TTL, drops the
// expired entries so credentials seen once do not accumulate.
func (c *balanceCache) put(now time.Time, key string, value balance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.sweptAt) >= balanceCacheTTL {
		for stale, entry := range c.entries {
			if !now.Before(entry.expiresAt) {
				delete(c.entries, stale)
			}
		}
		c.sweptAt = now
	}
	c.entries[key] = cachedBalance{value: value, expiresAt: now.Add(balanceCacheTTL)}
}
