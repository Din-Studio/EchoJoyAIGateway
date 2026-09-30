package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"gpt-load/internal/affinity"
)

// affinityRecordScript is the Redis form of affinity.Cache's conditional
// success update: a different mapping that is still live under the current
// TTL wins, anything else (absent, observed, or older than the TTL) is
// replaced by the new mapping.
//
// KEYS: the affinity key. ARGV: expected token ("" when none was observed),
// new value, nowMS, ttlMS. Returns 1 when written and 0 when rejected.
var affinityRecordScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if current and current ~= ARGV[1] then
  local learned = string.match(current, '^%d+:%d+:%d+:(%d+):')
  if learned and tonumber(learned) + tonumber(ARGV[4]) > tonumber(ARGV[3]) then
    return 0
  end
end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[4])
return 1
`)

// Affinity shares soft credential preferences across instances. Unlike the
// in-process cache it ignores Policy.Capacity (memory is bounded by the TTL)
// and never clears on a configuration revision; a TTL change applies to
// existing entries at lookup time. It is nil in single-instance mode.
type Affinity struct {
	client *Client
	now    func() time.Time
}

// NewAffinity returns nil when cluster mode is disabled.
func NewAffinity(client *Client) *Affinity {
	if client == nil {
		return nil
	}
	return &Affinity{client: client, now: time.Now}
}

// Lookup returns the live mapping for key. A mapping learned longer ago than
// the current TTL counts as absent even if its Redis key has not expired.
func (store *Affinity) Lookup(ctx context.Context, policy affinity.Policy, key affinity.Key) (affinity.Observation, error) {
	if !key.Valid() {
		return affinity.Observation{}, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	token, err := store.client.Get(callCtx, store.key(key)).Result()
	if errors.Is(err, redis.Nil) {
		return affinity.SharedObservation(key, affinity.Target{}, ""), nil
	}
	if err != nil {
		return affinity.Observation{}, fmt.Errorf("look up soft affinity: %w", err)
	}
	target, learnedAt, ok := decodeAffinityValue(token)
	if !ok || !learnedAt.Add(policy.TTL).After(store.now()) {
		return affinity.SharedObservation(key, affinity.Target{}, ""), nil
	}
	return affinity.SharedObservation(key, target, token), nil
}

// RecordSuccess conditionally learns target, keeping the first live success.
func (store *Affinity) RecordSuccess(
	ctx context.Context,
	policy affinity.Policy,
	key affinity.Key,
	observed affinity.Observation,
	target affinity.Target,
) (bool, error) {
	if !key.Valid() || !target.Valid() || policy.TTL <= 0 {
		return false, nil
	}
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	now := store.now()
	value := strings.Join([]string{
		strconv.FormatUint(uint64(target.GroupID), 10),
		strconv.FormatUint(uint64(target.CredentialID), 10),
		strconv.FormatUint(target.IdentityGeneration, 10),
		strconv.FormatInt(now.UnixMilli(), 10),
		hex.EncodeToString(nonce),
	}, ":")
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	written, err := affinityRecordScript.Run(callCtx, store.client, []string{store.key(key)},
		observed.Token(), value, now.UnixMilli(), policy.TTL.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("record soft affinity: %w", err)
	}
	return written == 1, nil
}

func (store *Affinity) key(key affinity.Key) string {
	return store.client.Key("aff", string(key))
}

func decodeAffinityValue(value string) (affinity.Target, time.Time, bool) {
	fields := strings.Split(value, ":")
	if len(fields) != 5 {
		return affinity.Target{}, time.Time{}, false
	}
	groupID, groupErr := strconv.ParseUint(fields[0], 10, 64)
	credentialID, credentialErr := strconv.ParseUint(fields[1], 10, 64)
	generation, generationErr := strconv.ParseUint(fields[2], 10, 64)
	learnedMS, learnedErr := strconv.ParseInt(fields[3], 10, 64)
	if errors.Join(groupErr, credentialErr, generationErr, learnedErr) != nil {
		return affinity.Target{}, time.Time{}, false
	}
	target := affinity.Target{GroupID: uint(groupID), CredentialID: uint(credentialID), IdentityGeneration: generation}
	return target, time.UnixMilli(learnedMS), target.Valid()
}
