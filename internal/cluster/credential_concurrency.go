package cluster

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

const (
	// credentialConcurrencyTTL only proves the holding instance is alive;
	// renewal keeps a slot for as long as the upstream request runs, and a
	// crashed instance's slots expire within one TTL.
	credentialConcurrencyTTL           = 30 * time.Second
	credentialConcurrencyRenewInterval = 10 * time.Second
	credentialConcurrencyReleaseWait   = 2 * time.Second
)

// Slots are ZSET members scored by their expiry on the Redis server clock, so
// no two instance clocks are compared.
//
// acquireSlotScript KEYS: the credential's in-flight ZSET. ARGV: limit, ttlMS,
// token. Returns 1 when the slot is taken, 0 when the credential is full.
var acquireSlotScript = redis.NewScript(`
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
local ttl = tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[1]) then
  return 0
end
redis.call('ZADD', KEYS[1], now + ttl, ARGV[3])
redis.call('PEXPIRE', KEYS[1], ttl)
return 1
`)

// renewSlotsScript KEYS: the credential's in-flight ZSET. ARGV: ttlMS, then
// every token this instance holds there. Expired slots are dropped first so a
// lost slot is never revived. Returns how many tokens were renewed.
var renewSlotsScript = redis.NewScript(`
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
local ttl = tonumber(ARGV[1])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
local renewed = 0
for index = 2, #ARGV do
  if redis.call('ZSCORE', KEYS[1], ARGV[index]) then
    redis.call('ZADD', KEYS[1], now + ttl, ARGV[index])
    renewed = renewed + 1
  end
end
if renewed > 0 then
  redis.call('PEXPIRE', KEYS[1], ttl)
end
return renewed
`)

// CredentialConcurrency caps the upstream requests in flight per credential
// across every instance sharing one Redis.
type CredentialConcurrency struct {
	client        *Client
	renewInterval time.Duration

	mu sync.Mutex
	// held lists the slot tokens this instance owns, per credential; the
	// renewal loop runs only while it is non-empty.
	held     map[uint]map[string]struct{}
	renewing bool
}

// NewCredentialConcurrency builds the shared per-credential concurrency limit.
func NewCredentialConcurrency(client *Client) *CredentialConcurrency {
	return &CredentialConcurrency{
		client:        client,
		renewInterval: credentialConcurrencyRenewInterval,
		held:          make(map[uint]map[string]struct{}),
	}
}

// Acquire takes one slot of the credential without waiting. A non-positive
// limit is unlimited and skips Redis. release is idempotent and must be called
// once the upstream exchange has finished; a full credential returns
// acquired=false, and an unavailable Redis returns an error.
func (limiter *CredentialConcurrency) Acquire(
	ctx context.Context,
	credentialID uint,
	limit int,
) (release func(), acquired bool, err error) {
	if limit <= 0 {
		return func() {}, true, nil
	}
	key := limiter.key(credentialID)
	token := limiter.client.InstanceID() + ":" + randomToken()
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	taken, err := acquireSlotScript.Run(callCtx, limiter.client, []string{key},
		strconv.Itoa(limit), strconv.FormatInt(credentialConcurrencyTTL.Milliseconds(), 10), token).Int()
	if err != nil {
		logrus.WithError(err).WithFields(logrus.Fields{
			"event": "credential_concurrency.redis_unavailable", "credential_id": credentialID,
		}).Warn("shared credential concurrency state is unavailable")
		return nil, false, fmt.Errorf("credential %d concurrency state: %w", credentialID, err)
	}
	if taken != 1 {
		return nil, false, nil
	}
	limiter.hold(credentialID, token)
	var once sync.Once
	release = func() {
		once.Do(func() {
			limiter.drop(credentialID, token)
			releaseCtx, cancel := context.WithTimeout(context.Background(), credentialConcurrencyReleaseWait)
			defer cancel()
			if err := limiter.client.ZRem(releaseCtx, key, token).Err(); err != nil {
				logrus.WithError(err).WithFields(logrus.Fields{
					"event": "credential_concurrency.release_failed", "credential_id": credentialID,
				}).Warn("credential concurrency slot release failed; it expires on its own")
			}
		})
	}
	return release, true, nil
}

func (limiter *CredentialConcurrency) hold(credentialID uint, token string) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	tokens := limiter.held[credentialID]
	if tokens == nil {
		tokens = make(map[string]struct{})
		limiter.held[credentialID] = tokens
	}
	tokens[token] = struct{}{}
	if !limiter.renewing {
		limiter.renewing = true
		go limiter.renewLoop()
	}
}

func (limiter *CredentialConcurrency) drop(credentialID uint, token string) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	delete(limiter.held[credentialID], token)
	if len(limiter.held[credentialID]) == 0 {
		delete(limiter.held, credentialID)
	}
}

// renewLoop is the instance's single renewal goroutine. It exits once no
// slot is held, so an instance that stops serving stops renewing.
func (limiter *CredentialConcurrency) renewLoop() {
	ticker := time.NewTicker(limiter.renewInterval)
	defer ticker.Stop()
	for range ticker.C {
		if !limiter.renewHeld() {
			return
		}
	}
}

// renewHeld extends every slot this instance holds in one pipeline. It
// reports false, and clears the running flag, when nothing is held.
func (limiter *CredentialConcurrency) renewHeld() bool {
	limiter.mu.Lock()
	if len(limiter.held) == 0 {
		limiter.renewing = false
		limiter.mu.Unlock()
		return false
	}
	ids := make([]uint, 0, len(limiter.held))
	args := make([][]any, 0, len(limiter.held))
	ttl := strconv.FormatInt(credentialConcurrencyTTL.Milliseconds(), 10)
	for id, tokens := range limiter.held {
		values := make([]any, 0, len(tokens)+1)
		values = append(values, ttl)
		for token := range tokens {
			values = append(values, token)
		}
		ids = append(ids, id)
		args = append(args, values)
	}
	limiter.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	pipeline := limiter.client.Pipeline()
	commands := make([]*redis.Cmd, 0, len(ids))
	for index, id := range ids {
		commands = append(commands, renewSlotsScript.Eval(ctx, pipeline, []string{limiter.key(id)}, args[index]...))
	}
	if _, err := pipeline.Exec(ctx); err != nil {
		logrus.WithError(err).WithField("event", "credential_concurrency.lease_lost").
			Warn("credential concurrency slots could not be renewed; in-flight requests continue")
		return true
	}
	for index, id := range ids {
		if renewed, err := commands[index].Int(); err != nil || renewed < limiter.stillHeld(id, args[index][1:]) {
			logrus.WithError(err).WithFields(logrus.Fields{
				"event": "credential_concurrency.lease_lost", "credential_id": id,
			}).Warn("credential concurrency slot expired before renewal; in-flight requests continue")
		}
	}
	return true
}

// stillHeld counts the tokens not released since the renewal snapshot, so a
// release racing the pipeline is not reported as a lost slot.
func (limiter *CredentialConcurrency) stillHeld(credentialID uint, tokens []any) int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	count := 0
	for _, token := range tokens {
		if _, ok := limiter.held[credentialID][token.(string)]; ok {
			count++
		}
	}
	return count
}

func (limiter *CredentialConcurrency) key(credentialID uint) string {
	return limiter.client.Key(credentialHashTag(credentialID), "inflight")
}
