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
	// concurrencyTTL only proves the holding instance is alive; renewal keeps
	// a slot for as long as the work runs, and a crashed instance's slots
	// expire within one TTL.
	concurrencyTTL           = 30 * time.Second
	concurrencyRenewInterval = 10 * time.Second
	concurrencyReleaseWait   = 2 * time.Second
)

// Slots are ZSET members scored by their expiry on the Redis server clock, so
// no two instance clocks are compared.
//
// acquireSlotScript KEYS: the scope's in-flight ZSET. ARGV: limit, ttlMS,
// token. Returns 1 when the slot is taken, 0 when the scope is full.
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

// renewSlotsScript KEYS: the scope's in-flight ZSET. ARGV: ttlMS, then every
// token this instance holds there. Expired slots are dropped first so a lost
// slot is never revived. Returns how many tokens were renewed.
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

// inflightScript KEYS: the scope's in-flight ZSET. Counts the slots that have
// not expired on the Redis clock without writing.
var inflightScript = redis.NewScript(`
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
return redis.call('ZCOUNT', KEYS[1], '(' .. now, '+inf')
`)

// Concurrency caps the work in flight per credential, per access key, per
// group, and across the whole cluster, shared by every instance on one Redis.
// Every scope is one ZSET lease; a non-positive limit is unlimited and never
// touches Redis, so an unlimited scope is not counted.
type Concurrency struct {
	client        *Client
	renewInterval time.Duration

	mu sync.Mutex
	// held lists the slot tokens this instance owns, per Redis key; the
	// renewal loop runs only while it is non-empty.
	held     map[string]map[string]struct{}
	renewing bool
}

// NewConcurrency builds the shared concurrency limits.
func NewConcurrency(client *Client) *Concurrency {
	return &Concurrency{
		client:        client,
		renewInterval: concurrencyRenewInterval,
		held:          make(map[string]map[string]struct{}),
	}
}

// Acquire takes one slot of the credential without waiting. release is
// idempotent and must be called once the upstream exchange has finished; a
// full credential returns acquired=false, and an unavailable Redis returns an
// error.
func (limiter *Concurrency) Acquire(
	ctx context.Context,
	credentialID uint,
	limit int,
) (release func(), acquired bool, err error) {
	return limiter.acquire(ctx, limiter.credentialKey(credentialID), int64(limit), "credential", credentialID)
}

// AcquireRequest takes one cluster-wide slot and one slot of the access key
// for a logical request. Both are released together; when the access key is
// full the global slot is given back, so a rejected request holds nothing.
func (limiter *Concurrency) AcquireRequest(
	ctx context.Context,
	accessKeyID uint,
	globalLimit, keyLimit int64,
) (release func(), acquired bool, err error) {
	releaseGlobal, acquired, err := limiter.acquire(ctx, limiter.globalKey(), globalLimit, "global", 0)
	if err != nil || !acquired {
		return nil, false, err
	}
	releaseKey, acquired, err := limiter.acquire(ctx, limiter.accessKeyKey(accessKeyID), keyLimit, "access_key", accessKeyID)
	if err != nil || !acquired {
		releaseGlobal()
		return nil, false, err
	}
	return func() { releaseKey(); releaseGlobal() }, true, nil
}

// AcquireGroup takes one slot of the group for an upstream call.
func (limiter *Concurrency) AcquireGroup(
	ctx context.Context,
	groupID uint,
	limit int64,
) (release func(), acquired bool, err error) {
	return limiter.acquire(ctx, limiter.groupKey(groupID), limit, "group", groupID)
}

// GlobalInflight reports the cluster-wide slots currently held.
func (limiter *Concurrency) GlobalInflight(ctx context.Context) (int64, error) {
	counts, err := limiter.inflight(ctx, []string{limiter.globalKey()})
	if err != nil {
		return 0, err
	}
	return counts[0], nil
}

// AccessKeyInflight reports the slots held per access key.
func (limiter *Concurrency) AccessKeyInflight(ctx context.Context, ids []uint) (map[uint]int64, error) {
	return limiter.inflightByID(ctx, ids, limiter.accessKeyKey)
}

// GroupInflight reports the slots held per group.
func (limiter *Concurrency) GroupInflight(ctx context.Context, ids []uint) (map[uint]int64, error) {
	return limiter.inflightByID(ctx, ids, limiter.groupKey)
}

func (limiter *Concurrency) acquire(
	ctx context.Context,
	key string,
	limit int64,
	scope string,
	id uint,
) (release func(), acquired bool, err error) {
	if limit <= 0 {
		return func() {}, true, nil
	}
	token := limiter.client.InstanceID() + ":" + randomToken()
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	taken, err := acquireSlotScript.Run(callCtx, limiter.client, []string{key},
		strconv.FormatInt(limit, 10), strconv.FormatInt(concurrencyTTL.Milliseconds(), 10), token).Int()
	if err != nil {
		logrus.WithError(err).WithFields(logrus.Fields{
			"event": "concurrency.redis_unavailable", "scope": scope, "id": id,
		}).Warn("shared concurrency state is unavailable")
		return nil, false, fmt.Errorf("%s %d concurrency state: %w", scope, id, err)
	}
	if taken != 1 {
		return nil, false, nil
	}
	limiter.hold(key, token)
	var once sync.Once
	release = func() {
		once.Do(func() {
			limiter.drop(key, token)
			releaseCtx, cancel := context.WithTimeout(context.Background(), concurrencyReleaseWait)
			defer cancel()
			if err := limiter.client.ZRem(releaseCtx, key, token).Err(); err != nil {
				logrus.WithError(err).WithFields(logrus.Fields{
					"event": "concurrency.release_failed", "scope": scope, "id": id,
				}).Warn("concurrency slot release failed; it expires on its own")
			}
		})
	}
	return release, true, nil
}

func (limiter *Concurrency) inflightByID(
	ctx context.Context,
	ids []uint,
	key func(uint) string,
) (map[uint]int64, error) {
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, key(id))
	}
	counts, err := limiter.inflight(ctx, keys)
	if err != nil {
		return nil, err
	}
	result := make(map[uint]int64, len(ids))
	for index, id := range ids {
		result[id] = counts[index]
	}
	return result, nil
}

// inflight counts the live slots of every key in one pipeline.
func (limiter *Concurrency) inflight(ctx context.Context, keys []string) ([]int64, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	pipeline := limiter.client.Pipeline()
	commands := make([]*redis.Cmd, 0, len(keys))
	for _, key := range keys {
		commands = append(commands, inflightScript.Eval(callCtx, pipeline, []string{key}))
	}
	if _, err := pipeline.Exec(callCtx); err != nil {
		return nil, fmt.Errorf("read concurrency state: %w", err)
	}
	counts := make([]int64, 0, len(keys))
	for _, command := range commands {
		count, err := command.Int64()
		if err != nil {
			return nil, fmt.Errorf("read concurrency state: %w", err)
		}
		counts = append(counts, count)
	}
	return counts, nil
}

func (limiter *Concurrency) hold(key, token string) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	tokens := limiter.held[key]
	if tokens == nil {
		tokens = make(map[string]struct{})
		limiter.held[key] = tokens
	}
	tokens[token] = struct{}{}
	if !limiter.renewing {
		limiter.renewing = true
		go limiter.renewLoop()
	}
}

func (limiter *Concurrency) drop(key, token string) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	delete(limiter.held[key], token)
	if len(limiter.held[key]) == 0 {
		delete(limiter.held, key)
	}
}

// renewLoop is the instance's single renewal goroutine. It exits once no
// slot is held, so an instance that stops serving stops renewing.
func (limiter *Concurrency) renewLoop() {
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
func (limiter *Concurrency) renewHeld() bool {
	limiter.mu.Lock()
	if len(limiter.held) == 0 {
		limiter.renewing = false
		limiter.mu.Unlock()
		return false
	}
	keys := make([]string, 0, len(limiter.held))
	args := make([][]any, 0, len(limiter.held))
	ttl := strconv.FormatInt(concurrencyTTL.Milliseconds(), 10)
	for key, tokens := range limiter.held {
		values := make([]any, 0, len(tokens)+1)
		values = append(values, ttl)
		for token := range tokens {
			values = append(values, token)
		}
		keys = append(keys, key)
		args = append(args, values)
	}
	limiter.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	pipeline := limiter.client.Pipeline()
	commands := make([]*redis.Cmd, 0, len(keys))
	for index, key := range keys {
		commands = append(commands, renewSlotsScript.Eval(ctx, pipeline, []string{key}, args[index]...))
	}
	if _, err := pipeline.Exec(ctx); err != nil {
		logrus.WithError(err).WithField("event", "concurrency.lease_lost").
			Warn("concurrency slots could not be renewed; in-flight work continues")
		return true
	}
	for index, key := range keys {
		if renewed, err := commands[index].Int(); err != nil || renewed < limiter.stillHeld(key, args[index][1:]) {
			logrus.WithError(err).WithFields(logrus.Fields{
				"event": "concurrency.lease_lost", "key": key,
			}).Warn("concurrency slot expired before renewal; in-flight work continues")
		}
	}
	return true
}

// stillHeld counts the tokens not released since the renewal snapshot, so a
// release racing the pipeline is not reported as a lost slot.
func (limiter *Concurrency) stillHeld(key string, tokens []any) int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	count := 0
	for _, token := range tokens {
		if _, ok := limiter.held[key][token.(string)]; ok {
			count++
		}
	}
	return count
}

func (limiter *Concurrency) credentialKey(credentialID uint) string {
	return limiter.client.Key(credentialHashTag(credentialID), "inflight")
}

func (limiter *Concurrency) accessKeyKey(accessKeyID uint) string {
	return limiter.client.Key(accessKeyHashTag(accessKeyID), "inflight")
}

func (limiter *Concurrency) groupKey(groupID uint) string {
	return limiter.client.Key("{group:"+strconv.FormatUint(uint64(groupID), 10)+"}", "inflight")
}

func (limiter *Concurrency) globalKey() string {
	return limiter.client.Key("concurrency", "global")
}
