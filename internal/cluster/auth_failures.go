package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// authFailureScript is the Redis form of the control plane's in-process
// admin authentication limiter: a live lock rejects without counting;
// otherwise failures outside the rolling window expire, this failure is
// recorded, and reaching the limit replaces the failures with a lock.
//
// KEYS: failure ZSET, lock key (one hash tag). ARGV: nowMS, windowMS, limit,
// lockMS, unique member. Returns {newlyLocked, retryAfterMS}.
var authFailureScript = redis.NewScript(`
local remaining = redis.call('PTTL', KEYS[2])
if remaining > 0 then
  return {0, remaining}
end
local now = tonumber(ARGV[1])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - tonumber(ARGV[2]))
redis.call('ZADD', KEYS[1], now, ARGV[5])
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[3]) then
  redis.call('DEL', KEYS[1])
  redis.call('SET', KEYS[2], '1', 'PX', ARGV[4])
  return {1, tonumber(ARGV[4])}
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return {0, 0}
`)

// AuthFailurePolicy is the lockout rule the control plane enforces.
type AuthFailurePolicy struct {
	Window       time.Duration
	Limit        int
	LockDuration time.Duration
}

// AuthFailureDecision is the shared lockout state after one evaluation.
type AuthFailureDecision struct {
	RetryAfter  time.Duration
	NewlyLocked bool
}

// AuthFailures counts admin authentication failures per peer across every
// instance, so a lockout reached through any instance applies to all. It is
// nil in single-instance mode.
type AuthFailures struct {
	client *Client
	// memberPrefix and sequence keep every recorded failure distinct.
	memberPrefix string
	sequence     atomic.Uint64
}

// NewAuthFailures returns nil when cluster mode is disabled.
func NewAuthFailures(client *Client) *AuthFailures {
	if client == nil {
		return nil
	}
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	return &AuthFailures{client: client, memberPrefix: hex.EncodeToString(nonce) + ":"}
}

// Evaluate records one authentication attempt from peer. A valid credential
// clears the peer's failures and lock.
func (failures *AuthFailures) Evaluate(
	ctx context.Context,
	peer string,
	valid bool,
	policy AuthFailurePolicy,
	now time.Time,
) (AuthFailureDecision, error) {
	keys := failures.keys(peer)
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	if valid {
		if err := failures.client.Del(callCtx, keys...).Err(); err != nil {
			return AuthFailureDecision{}, fmt.Errorf("clear admin auth failures: %w", err)
		}
		return AuthFailureDecision{}, nil
	}
	member := failures.memberPrefix + strconv.FormatUint(failures.sequence.Add(1), 10)
	result, err := authFailureScript.Run(callCtx, failures.client, keys,
		now.UnixMilli(), policy.Window.Milliseconds(), policy.Limit, policy.LockDuration.Milliseconds(), member,
	).Int64Slice()
	if err != nil {
		return AuthFailureDecision{}, fmt.Errorf("record admin auth failure: %w", err)
	}
	if len(result) != 2 {
		return AuthFailureDecision{}, fmt.Errorf("record admin auth failure: unexpected reply %v", result)
	}
	return AuthFailureDecision{
		RetryAfter:  time.Duration(result[1]) * time.Millisecond,
		NewlyLocked: result[0] == 1,
	}, nil
}

func (failures *AuthFailures) keys(peer string) []string {
	tag := "{auth:" + peer + "}"
	return []string{failures.client.Key(tag, "fail"), failures.client.Key(tag, "lock")}
}
