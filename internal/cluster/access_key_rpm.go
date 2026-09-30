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
	"github.com/sirupsen/logrus"

	"gpt-load/internal/ratelimit"
)

// rpmScript is the Redis form of ratelimit.AccessKeyRPM's 60 s sliding
// window: entries at or before now-60000 expire, a full window rejects with
// the admission time of the entry that must expire first, and an admitted
// request is recorded.
//
// KEYS: the AccessKey window ZSET. ARGV: nowMS, limit, unique member.
// Returns {"1"} when admitted or {"0", targetMS} when rejected.
var rpmScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - 60000)
local count = redis.call('ZCARD', KEYS[1])
if count >= limit then
  local target = redis.call('ZRANGE', KEYS[1], count - limit, count - limit, 'WITHSCORES')
  return {'0', target[2]}
end
redis.call('ZADD', KEYS[1], now, ARGV[3])
redis.call('PEXPIRE', KEYS[1], 60000)
return {'1'}
`)

// AccessKeyRPM enforces per-AccessKey RPM against one window shared by every
// instance. It is nil in single-instance mode.
type AccessKeyRPM struct {
	client *Client
	now    func() time.Time
	// memberPrefix and sequence make every window entry unique across all
	// limiters, even ones sharing an instance ID; a reused ZSET member would
	// replace an entry instead of counting a request.
	memberPrefix string
	sequence     atomic.Uint64
}

// NewAccessKeyRPM returns nil when cluster mode is disabled.
func NewAccessKeyRPM(client *Client) *AccessKeyRPM {
	if client == nil {
		return nil
	}
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	return &AccessKeyRPM{client: client, now: time.Now, memberPrefix: hex.EncodeToString(nonce) + ":"}
}

// Allow admits one request when fewer than limit requests were admitted in
// the last minute. A non-positive limit is unlimited and skips Redis; unlike
// the in-memory limiter it leaves the old window to expire on its own.
func (limiter *AccessKeyRPM) Allow(ctx context.Context, accessKeyID uint, limit int64) (ratelimit.LimitDecision, error) {
	if limit <= 0 {
		return ratelimit.LimitDecision{Allowed: true}, nil
	}
	now := limiter.now()
	member := limiter.memberPrefix + strconv.FormatUint(limiter.sequence.Add(1), 10)
	callCtx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	reply, err := runStrings(callCtx, limiter.client, rpmScript,
		[]string{limiter.client.Key(accessKeyHashTag(accessKeyID), "rpm")},
		[]any{strconv.FormatInt(now.UnixMilli(), 10), strconv.FormatInt(limit, 10), member})
	if err == nil && (len(reply) == 0 || (reply[0] == "0" && len(reply) != 2)) {
		err = fmt.Errorf("malformed reply %q", reply)
	}
	if err != nil {
		logrus.WithError(err).WithFields(logrus.Fields{
			"event": "access_key_rpm.redis_unavailable", "access_key_id": accessKeyID,
		}).Warn("shared access key rate limit state is unavailable")
		return ratelimit.LimitDecision{}, fmt.Errorf("access key %d rate limit state: %w", accessKeyID, err)
	}
	if reply[0] == "1" {
		return ratelimit.LimitDecision{Allowed: true}, nil
	}
	targetMS, err := strconv.ParseFloat(reply[1], 64)
	if err != nil {
		return ratelimit.LimitDecision{}, fmt.Errorf("access key %d rate limit state: %w", accessKeyID, err)
	}
	return ratelimit.LimitDecision{
		Allowed:    false,
		RetryAfter: ratelimit.RetryAfter(time.UnixMilli(int64(targetMS)), now),
	}, nil
}
