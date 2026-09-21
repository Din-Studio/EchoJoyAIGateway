package coordination

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"gpt-load/internal/ratelimit"
)

// rpmCommandTimeout bounds one limit decision. The limiter is on the data
// plane's critical path, so a Redis that has stopped answering must turn into
// a refusal quickly rather than hold the request open.
const rpmCommandTimeout = 500 * time.Millisecond

// AccessKeyRPM counts an access key's requests across every instance, so a
// limit configured as N requests per minute stays N in total rather than
// becoming N per instance.
//
// The window is a sorted set of request timestamps, which is the same shape
// the in-process limiter keeps in memory: admission compares the count in the
// last minute against the limit, and a rejection is dated by the request that
// has to age out before a slot frees up. Keeping the shape identical is what
// lets both implementations answer with ratelimit.RetryAfterFor and agree.
//
// Every operation is a single key, tagged so a future Redis Cluster keeps an
// access key's window on one slot.
type AccessKeyRPM struct {
	client   *Client
	timeout  time.Duration
	now      func() time.Time
	instance string
	sequence atomic.Uint64
}

// admitAccessKeyRPM is the whole decision, executed atomically so two
// instances cannot both read a count of limit-1 and both admit.
//
// The expiry is refreshed on every call, including a rejected one: a window
// that stops being written to is worthless one minute later, and letting it
// expire is what keeps idle access keys from accumulating.
//
// Scores are returned as strings because a Lua number is a double and a
// millisecond timestamp is not exactly representable in one past 2^53.
var admitAccessKeyRPM = redis.NewScript(`
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - window)
local count = redis.call('ZCARD', KEYS[1])
if count >= limit then
  local oldest = redis.call('ZRANGE', KEYS[1], count - limit, count - limit, 'WITHSCORES')
  redis.call('PEXPIRE', KEYS[1], window)
  return {0, oldest[2]}
end
redis.call('ZADD', KEYS[1], now, ARGV[4])
redis.call('PEXPIRE', KEYS[1], window)
return {1, '0'}
`)

// NewAccessKeyRPM binds the shared request window to a Redis client. The
// instance identity only makes this process's window members unique; it is
// never read back, so it carries no meaning beyond distinctness.
func NewAccessKeyRPM(client *Client, instance string) *AccessKeyRPM {
	return &AccessKeyRPM{
		client:   client,
		timeout:  rpmCommandTimeout,
		now:      time.Now,
		instance: instance,
	}
}

// Allow reports whether this request fits inside the access key's shared
// minute. A limit of zero or less is unlimited and never reaches Redis.
//
// A backend failure is reported as an undecided rejection rather than an
// admission: the count is authoritative only in Redis, so admitting on failure
// would silently restore the per-instance multiplication this type removes.
//
// The deadline is this limiter's own rather than the request's on purpose. A
// caller that disconnects mid-call would otherwise leave the window in an
// unknown state — the ZADD may or may not have run — and the next decision
// would be made against a count nobody can account for.
func (limiter *AccessKeyRPM) Allow(accessKeyID uint, limit int64) ratelimit.LimitDecision {
	if limit <= 0 {
		return ratelimit.LimitDecision{Allowed: true}
	}
	ctx, cancel := context.WithTimeout(context.Background(), limiter.timeout)
	defer cancel()

	now := limiter.now()
	reply, err := admitAccessKeyRPM.Run(
		ctx, limiter.client.Redis(),
		[]string{accessKeyRPMKey(accessKeyID)},
		now.UnixMilli(), ratelimit.Window.Milliseconds(), limit, limiter.member(),
	).Slice()
	if err != nil {
		return ratelimit.LimitDecision{Allowed: false, Unavailable: true}
	}
	admitted, target, err := parseAccessKeyRPMReply(reply)
	if err != nil {
		return ratelimit.LimitDecision{Allowed: false, Unavailable: true}
	}
	if admitted {
		return ratelimit.LimitDecision{Allowed: true}
	}
	return ratelimit.LimitDecision{
		Allowed:    false,
		RetryAfter: ratelimit.RetryAfterFor(time.UnixMilli(target), now),
	}
}

// member is unique per request within this process, and unique across
// processes through the instance identity. Two requests sharing a member would
// collapse into one sorted-set entry and one of them would go uncounted.
func (limiter *AccessKeyRPM) member() string {
	return limiter.instance + "-" + strconv.FormatUint(limiter.sequence.Add(1), 36)
}

func parseAccessKeyRPMReply(reply []any) (bool, int64, error) {
	if len(reply) != 2 {
		return false, 0, fmt.Errorf("admit access key rpm: unexpected reply shape")
	}
	admitted, ok := reply[0].(int64)
	if !ok {
		return false, 0, fmt.Errorf("admit access key rpm: unexpected verdict type")
	}
	if admitted == 1 {
		return true, 0, nil
	}
	score, ok := reply[1].(string)
	if !ok {
		return false, 0, fmt.Errorf("admit access key rpm: unexpected score type")
	}
	// Redis formats sorted-set scores as a double, so a whole millisecond
	// arrives as "1750000000000" on some versions and "1.75e+12" on others.
	target, err := strconv.ParseFloat(score, 64)
	if err != nil {
		return false, 0, fmt.Errorf("admit access key rpm: parse score: %w", err)
	}
	return false, int64(target), nil
}

func accessKeyRPMKey(accessKeyID uint) string {
	return Key("rpm", hashTag(strconv.FormatUint(uint64(accessKeyID), 10)))
}
