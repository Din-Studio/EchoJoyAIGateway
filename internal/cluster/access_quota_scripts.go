package cluster

import "github.com/redis/go-redis/v9"

// Every AccessQuota script mirrors one accessquota.Runtime state transition
// and must stay behaviorally identical to it. Each rule is one Hash with
// fields rev/used/ws/we/gen/ver; an inactive window stores ws and we as "".
// Amounts are nano-USD int64 values that exceed Lua's exact double range, so
// they are compared as canonical decimal strings and only added through
// HINCRBY after an overflow check. Timestamps and revisions stay far below
// 2^53.

// quotaScript evaluates (and in admit mode, opens periodic windows for) all
// rules of one AccessKey.
//
// KEYS: one Hash per rule. ARGV: mode ("check"|"admit"), nowMS, then per rule
// rev, kind, limit, windowEndMS (nowMS + period for periodic rules).
// Returns {"STALE"} when any stored revision is newer than the caller's,
// {"NEED_INIT", i...} with the 1-based indexes of missing or older rules, or
// {"OK", allowed, then per rule used, ws, we, gen, ver, opened}.
var quotaScript = redis.NewScript(`
local function cmp(a, b)
  if #a ~= #b then
    if #a < #b then return -1 end
    return 1
  end
  if a == b then return 0 end
  if a < b then return -1 end
  return 1
end

local mode = ARGV[1]
local now = tonumber(ARGV[2])
local stale = false
local missing = {}
local rows = {}
for i = 1, #KEYS do
  local rev = tonumber(ARGV[2 + (i - 1) * 4 + 1])
  local row = redis.call('HMGET', KEYS[i], 'rev', 'used', 'ws', 'we', 'gen', 'ver')
  if not row[1] then
    missing[#missing + 1] = tostring(i)
  else
    local stored = tonumber(row[1])
    if stored > rev then
      stale = true
    elseif stored < rev then
      missing[#missing + 1] = tostring(i)
    end
  end
  rows[i] = row
end
if stale then return {'STALE'} end
if #missing > 0 then
  table.insert(missing, 1, 'NEED_INIT')
  return missing
end

local allowed = true
local inactive = {}
for i = 1, #KEYS do
  local base = 2 + (i - 1) * 4
  local row = rows[i]
  local periodic = ARGV[base + 2] == 'periodic'
  inactive[i] = periodic and (row[3] == '' or row[4] == '' or now >= tonumber(row[4]))
  if not inactive[i] and cmp(row[2], ARGV[base + 3]) >= 0 then
    allowed = false
  end
end

local result = {'OK', allowed and '1' or '0'}
for i = 1, #KEYS do
  local base = 2 + (i - 1) * 4
  local row = rows[i]
  local opened = '0'
  if mode == 'admit' and allowed and inactive[i] then
    redis.call('HSET', KEYS[i], 'used', '0', 'ws', ARGV[2], 'we', ARGV[base + 4])
    row[2], row[3], row[4] = '0', ARGV[2], ARGV[base + 4]
    row[5] = tostring(redis.call('HINCRBY', KEYS[i], 'gen', 1))
    row[6] = tostring(redis.call('HINCRBY', KEYS[i], 'ver', 1))
    opened = '1'
  end
  for field = 2, 6 do result[#result + 1] = row[field] end
  result[#result + 1] = opened
end
return result
`)

// initScript writes checkpoint states for rules Redis does not hold at the
// current revision. A key already at the same or a newer revision is kept,
// so concurrent hydrations and live updates are never overwritten.
//
// KEYS: one Hash per rule. ARGV per rule: rev, used, ws, we, gen, ver.
var initScript = redis.NewScript(`
for i = 1, #KEYS do
  local base = (i - 1) * 6
  local stored = redis.call('HGET', KEYS[i], 'rev')
  if not stored or tonumber(stored) < tonumber(ARGV[base + 1]) then
    redis.call('HSET', KEYS[i], 'rev', ARGV[base + 1], 'used', ARGV[base + 2],
      'ws', ARGV[base + 3], 'we', ARGV[base + 4], 'gen', ARGV[base + 5], 'ver', ARGV[base + 6])
  end
end
return {'OK'}
`)

// completeScript adds a settled cost to every ticket rule whose revision and
// window generation still match; total rules always keep generation 0. Usage
// saturates at MaxInt64, detected as used > headroom (MaxInt64 - cost); a
// negative cost saturates immediately.
//
// KEYS: one Hash per ticket rule. ARGV: cost, headroom, then per rule rev, gen.
// Returns {fault, then per changed rule index, rev, ver}.
var completeScript = redis.NewScript(`
local function cmp(a, b)
  if #a ~= #b then
    if #a < #b then return -1 end
    return 1
  end
  if a == b then return 0 end
  if a < b then return -1 end
  return 1
end

local maxUsed = '9223372036854775807'
local cost = ARGV[1]
local headroom = ARGV[2]
local negative = string.sub(cost, 1, 1) == '-'
local fault = ''
local changed = {}
for i = 1, #KEYS do
  local base = 2 + (i - 1) * 2
  local row = redis.call('HMGET', KEYS[i], 'rev', 'gen', 'used')
  if row[1] and tonumber(row[1]) == tonumber(ARGV[base + 1]) and row[2] == ARGV[base + 2] then
    local updated = true
    if negative or cmp(row[3], headroom) > 0 then
      if negative then
        fault = 'negative_estimate'
      elseif fault == '' then
        fault = 'overflow'
      end
      updated = row[3] ~= maxUsed
      redis.call('HSET', KEYS[i], 'used', maxUsed)
    else
      redis.call('HINCRBY', KEYS[i], 'used', cost)
    end
    if updated then
      changed[#changed + 1] = tostring(i)
      changed[#changed + 1] = row[1]
      changed[#changed + 1] = tostring(redis.call('HINCRBY', KEYS[i], 'ver', 1))
    end
  end
end
table.insert(changed, 1, fault)
return changed
`)
