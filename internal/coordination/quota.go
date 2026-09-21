package coordination

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"gpt-load/internal/accessquota"
)

const (
	// quotaCommandTimeout bounds one ledger call. Admission is on the data
	// plane's critical path, so a Redis that has stopped answering must turn
	// into a refusal quickly rather than hold the request open.
	quotaCommandTimeout = 500 * time.Millisecond
	// quotaTotalTTL keeps a lifetime rule's counter alive across any plausible
	// idle period. Expiry is safe rather than merely tolerable: a rule the
	// ledger has no state for is seeded from the caller's database-backed
	// baseline, which is the same value the key held.
	quotaTotalTTL = 30 * 24 * time.Hour
	// quotaSaturatedUsed is the largest counter an int64 can carry. A charge
	// that would pass it is clamped here and reported as an overflow fault,
	// matching what the in-memory runtime does with the same overflow.
	quotaSaturatedUsed = "9223372036854775807"
)

// QuotaLedger is the cross-instance authority for access key cost limits: the
// counter and the periodic window live in Redis, so N instances spend against
// one budget instead of N copies of it.
//
// Counters are read and written as strings throughout. A Lua number is a
// double, and nano-USD counters run past the range a double represents
// exactly; passing them through as text keeps the value the client is billed
// for identical to the value the database checkpoint records.
//
// Each access key's rules share a hash tag so one script never spans slots if
// this is ever pointed at a Redis Cluster.
type QuotaLedger struct {
	client  *Client
	timeout time.Duration
}

// syncAccessQuota reports every rule's authoritative counter and rolls any
// periodic window that has ended, atomically so two instances cannot both
// observe an ended window and both start a new one.
//
// A rule whose stored revision differs from the caller's is reseeded from the
// caller's baseline: a new revision is a new limit, and its counter starts
// where the durable checkpoint says it does.
var syncAccessQuota = redis.NewScript(`
local now = tonumber(ARGV[1])
local roll = ARGV[2] == '1'
local cursor = 3
local out = {}
for index = 1, #KEYS do
  local key = KEYS[index]
  local revision = ARGV[cursor]
  local periodic = ARGV[cursor + 1] == '1'
  local periodMS = tonumber(ARGV[cursor + 2])
  local used = ARGV[cursor + 3]
  local windowStart = ARGV[cursor + 4]
  local windowEnd = ARGV[cursor + 5]
  local generation = ARGV[cursor + 6]
  local ttl = tonumber(ARGV[cursor + 7])
  cursor = cursor + 8

  local stored = redis.call('HMGET', key, 'rev', 'used', 'win_start', 'win_end', 'win_gen')
  if stored[1] == revision then
    used = stored[2]
    windowStart = stored[3]
    windowEnd = stored[4]
    generation = stored[5]
  else
    redis.call('HSET', key, 'rev', revision, 'used', used,
      'win_start', windowStart, 'win_end', windowEnd, 'win_gen', generation)
  end

  if periodic and roll and (windowEnd == '' or now >= tonumber(windowEnd)) then
    used = '0'
    windowStart = tostring(now)
    windowEnd = tostring(now + periodMS)
    generation = tostring(tonumber(generation) + 1)
    redis.call('HSET', key, 'used', used,
      'win_start', windowStart, 'win_end', windowEnd, 'win_gen', generation)
  end

  redis.call('PEXPIRE', key, ttl)
  out[#out + 1] = used
  out[#out + 1] = windowStart
  out[#out + 1] = windowEnd
  out[#out + 1] = generation
end
return out
`)

// chargeAccessQuota adds a completed request's cost to every rule the ticket
// still matches. A rule whose revision or window generation moved on is left
// alone: the request did not run in the window that is open now, and charging
// it there would spend a budget it never belonged to.
//
// HINCRBY is the only arithmetic here, and it happens in Redis where the
// counter is a true int64. An increment past that range is an error rather
// than a wrap, which is the signal the counter has saturated.
var chargeAccessQuota = redis.NewScript(`
local cost = ARGV[1]
local cursor = 2
local out = {}
for index = 1, #KEYS do
  local key = KEYS[index]
  local revision = ARGV[cursor]
  local generation = ARGV[cursor + 1]
  local ttl = tonumber(ARGV[cursor + 2])
  cursor = cursor + 3

  local stored = redis.call('HMGET', key, 'rev', 'win_gen')
  if stored[1] == revision and stored[2] == generation then
    local saturated = '0'
    local charged = redis.pcall('HINCRBY', key, 'used', cost)
    if type(charged) == 'table' and charged.err then
      redis.call('HSET', key, 'used', '` + quotaSaturatedUsed + `')
      saturated = '1'
    end
    redis.call('PEXPIRE', key, ttl)
    out[#out + 1] = redis.call('HGET', key, 'used')
    out[#out + 1] = redis.call('HGET', key, 'win_start')
    out[#out + 1] = redis.call('HGET', key, 'win_end')
    out[#out + 1] = stored[2]
    out[#out + 1] = saturated
  else
    out[#out + 1] = ''
    out[#out + 1] = ''
    out[#out + 1] = ''
    out[#out + 1] = ''
    out[#out + 1] = '0'
  end
end
return out
`)

// NewQuotaLedger binds access key cost limits to a Redis client.
func NewQuotaLedger(client *Client) *QuotaLedger {
	return &QuotaLedger{client: client, timeout: quotaCommandTimeout}
}

// Sync implements accessquota.SharedLedger.
func (ledger *QuotaLedger) Sync(
	accessKeyID uint,
	rules []accessquota.SharedRule,
	nowMS int64,
	roll bool,
) ([]accessquota.SharedState, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(rules))
	arguments := make([]any, 0, 2+len(rules)*8)
	arguments = append(arguments, nowMS, boolArgument(roll))
	for _, rule := range rules {
		keys = append(keys, accessQuotaKey(accessKeyID, rule.RuleID))
		arguments = append(arguments,
			strconv.FormatUint(rule.Revision, 10),
			boolArgument(rule.Periodic),
			rule.PeriodSeconds*int64(time.Second/time.Millisecond),
			strconv.FormatInt(rule.UsedNanoUSD, 10),
			optionalMSArgument(rule.WindowStartedAtMS),
			optionalMSArgument(rule.WindowEndsAtMS),
			strconv.FormatUint(rule.WindowGeneration, 10),
			quotaTTL(rule).Milliseconds(),
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), ledger.timeout)
	defer cancel()
	reply, err := syncAccessQuota.Run(ctx, ledger.client.Redis(), keys, arguments...).Slice()
	if err != nil {
		return nil, fmt.Errorf("sync access quota: %w", err)
	}
	return parseQuotaReply(rules, reply, 4)
}

// Add implements accessquota.SharedLedger.
func (ledger *QuotaLedger) Add(
	accessKeyID uint,
	rules []accessquota.SharedTicketRule,
	costNanoUSD int64,
) ([]accessquota.SharedState, error) {
	if len(rules) == 0 || costNanoUSD == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(rules))
	arguments := make([]any, 0, 1+len(rules)*3)
	arguments = append(arguments, strconv.FormatInt(costNanoUSD, 10))
	for _, rule := range rules {
		keys = append(keys, accessQuotaKey(accessKeyID, rule.RuleID))
		arguments = append(arguments,
			strconv.FormatUint(rule.Revision, 10),
			strconv.FormatUint(rule.WindowGeneration, 10),
			// A charge only ever refreshes an existing key's lifetime; the
			// longest TTL is correct here because the rule's shape is not part
			// of the ticket.
			quotaTotalTTL.Milliseconds(),
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), ledger.timeout)
	defer cancel()
	reply, err := chargeAccessQuota.Run(ctx, ledger.client.Redis(), keys, arguments...).Slice()
	if err != nil {
		return nil, fmt.Errorf("charge access quota: %w", err)
	}
	ruleIDs := make([]accessquota.SharedRule, 0, len(rules))
	for _, rule := range rules {
		ruleIDs = append(ruleIDs, accessquota.SharedRule{RuleID: rule.RuleID})
	}
	return parseQuotaReply(ruleIDs, reply, 5)
}

// parseQuotaReply turns the script's flat string array back into per-rule
// state. A rule the script declined to answer for arrives as empty strings and
// is returned with a zero RuleID, which the runtime skips.
func parseQuotaReply(
	rules []accessquota.SharedRule,
	reply []any,
	stride int,
) ([]accessquota.SharedState, error) {
	if len(reply) != len(rules)*stride {
		return nil, fmt.Errorf("read access quota: unexpected reply shape")
	}
	states := make([]accessquota.SharedState, 0, len(rules))
	for index, rule := range rules {
		fields := make([]string, stride)
		for offset := range stride {
			value, ok := reply[index*stride+offset].(string)
			if !ok {
				return nil, fmt.Errorf("read access quota rule %d: unexpected field type", rule.RuleID)
			}
			fields[offset] = value
		}
		if fields[0] == "" {
			states = append(states, accessquota.SharedState{})
			continue
		}
		used, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("read access quota rule %d: parse counter: %w", rule.RuleID, err)
		}
		windowStart, err := parseOptionalMS(fields[1])
		if err != nil {
			return nil, fmt.Errorf("read access quota rule %d: parse window start: %w", rule.RuleID, err)
		}
		windowEnd, err := parseOptionalMS(fields[2])
		if err != nil {
			return nil, fmt.Errorf("read access quota rule %d: parse window end: %w", rule.RuleID, err)
		}
		generation, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("read access quota rule %d: parse generation: %w", rule.RuleID, err)
		}
		states = append(states, accessquota.SharedState{
			RuleID: rule.RuleID, UsedNanoUSD: used,
			WindowStartedAtMS: windowStart, WindowEndsAtMS: windowEnd,
			WindowGeneration: generation,
			Saturated:        stride == 5 && fields[4] == "1",
		})
	}
	return states, nil
}

// quotaTTL keeps a periodic rule's key alive for two windows. One window would
// race the roll itself; two is long enough that a key only expires after the
// rule has genuinely gone quiet, at which point its counter is about to be
// reset anyway.
func quotaTTL(rule accessquota.SharedRule) time.Duration {
	if !rule.Periodic {
		return quotaTotalTTL
	}
	return 2 * time.Duration(rule.PeriodSeconds) * time.Second
}

func boolArgument(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func optionalMSArgument(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}

func parseOptionalMS(value string) (*int64, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func accessQuotaKey(accessKeyID, ruleID uint) string {
	return Key("quota",
		hashTag(strconv.FormatUint(uint64(accessKeyID), 10)),
		strconv.FormatUint(uint64(ruleID), 10),
	)
}
