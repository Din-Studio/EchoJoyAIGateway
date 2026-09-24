package cluster

import "github.com/redis/go-redis/v9"

// credentialHealthScript applies one state.CredentialRegistry health
// mutation to a credential's Hash and must stay behaviorally identical to
// it. Fields: ep (record epoch), ver (bumped on every mutation), idg
// (identity generation), cd (cooldown deadline ms), bl ("1" when
// blacklisted), fc (failure count), fg/mg (failure and model cooldown
// generations), m:<model> (model cooldown deadline ms), auth and asv (auth
// state and the secret version it belongs to). Absent cd/bl/fc mean zero.
// Identity generations and the fg/mg generations are compared as decimal
// strings; deadlines and secret versions stay far below 2^53.
//
// A caller with another identity generation resets the record first, like a
// local registry entry replaced for a new identity. Identity generations are
// hashes and cannot be ordered, so a peer that has not reloaded a target
// change yet can briefly bounce the record back to the old identity; this is
// accepted because it only loses health recorded during reload skew and the
// next write under the current identity resets it again. Every mutation bumps ver
// and publishes the full record, so a peer applies it without a read.
//
// KEYS[1]: the credential Hash. ARGV: op, origin, credentialID, idg,
// candidate epoch, ttlMS, events channel, then op arguments.
// Returns {accepted, changed, becameBlacklisted, HGETALL pairs...}.
var credentialHealthScript = redis.NewScript(`
local key = KEYS[1]
local op, idg = ARGV[1], ARGV[4]
local mutated = false

local function num(field)
  return tonumber(redis.call('HGET', key, field) or '0')
end

local function clear_model_cooldowns()
  local fields = redis.call('HKEYS', key)
  for _, field in ipairs(fields) do
    if string.sub(field, 1, 2) == 'm:' then redis.call('HDEL', key, field) end
  end
end

if redis.call('HSETNX', key, 'ep', ARGV[5]) == 1 then
  redis.call('HSET', key, 'ver', '0', 'idg', idg, 'fg', '0', 'mg', '0')
  mutated = true
elseif redis.call('HGET', key, 'idg') ~= idg then
  clear_model_cooldowns()
  redis.call('HDEL', key, 'cd', 'bl', 'fc', 'auth', 'asv')
  redis.call('HSET', key, 'idg', idg, 'fg', '0', 'mg', '0')
  mutated = true
end

local accepted, changed, became = true, false, false
if op == 'cooldown' then
  local expected = ARGV[9]
  local asv = redis.call('HGET', key, 'asv')
  if expected ~= '' and asv and asv ~= expected then
    accepted = false
  elseif tonumber(ARGV[8]) > num('cd') then
    redis.call('HSET', key, 'cd', ARGV[8])
    changed = true
  end
elseif op == 'fail' then
  local count = redis.call('HINCRBY', key, 'fc', 1)
  redis.call('HINCRBY', key, 'fg', 1)
  changed = true
  local threshold = tonumber(ARGV[8])
  if threshold > 0 and count >= threshold and redis.call('HGET', key, 'bl') ~= '1' then
    redis.call('HSET', key, 'bl', '1')
    redis.call('HINCRBY', key, 'fg', 1)
    became = true
  end
elseif op == 'clear_failure' then
  if num('fc') ~= 0 then
    redis.call('HDEL', key, 'fc')
    redis.call('HINCRBY', key, 'fg', 1)
    changed = true
  end
elseif op == 'model_cooldown' then
  local field, untilMS, now = 'm:' .. ARGV[8], tonumber(ARGV[9]), tonumber(ARGV[10])
  if redis.call('HGET', key, 'mg') ~= ARGV[11] then
    accepted = false
  else
    local fields = redis.call('HGETALL', key)
    for i = 1, #fields, 2 do
      if string.sub(fields[i], 1, 2) == 'm:' and tonumber(fields[i + 1]) <= now then
        redis.call('HDEL', key, fields[i])
        mutated = true
      end
    end
    if untilMS > num(field) then
      redis.call('HSET', key, field, ARGV[9])
      changed = true
    end
  end
elseif op == 'restore' then
  if ARGV[8] == '1' then
    redis.call('HDEL', key, 'cd', 'bl', 'fc')
    redis.call('HINCRBY', key, 'fg', 1)
  end
  if ARGV[9] == '1' then
    clear_model_cooldowns()
    redis.call('HINCRBY', key, 'mg', 1)
  end
  changed = true
elseif op == 'recover_if_match' then
  local cooldown = ARGV[9]
  if redis.call('HGET', key, 'bl') ~= '1' or redis.call('HGET', key, 'fg') ~= ARGV[8] or
      cooldown ~= '' and num('cd') ~= tonumber(cooldown) then
    accepted = false
  else
    if cooldown ~= '' then redis.call('HDEL', key, 'cd') end
    redis.call('HDEL', key, 'bl', 'fc')
    redis.call('HINCRBY', key, 'fg', 1)
    changed = true
  end
elseif op == 'clear_cooldown_if_match' then
  if num('cd') ~= tonumber(ARGV[8]) then
    accepted = false
  else
    redis.call('HDEL', key, 'cd')
    changed = true
  end
elseif op == 'auth' then
  -- ARGV[10], ARGV[11]: when set, the record epoch and version the caller
  -- read; the write is refused once any other write moved the record.
  local stored = redis.call('HGET', key, 'asv')
  if ARGV[10] ~= '' and (redis.call('HGET', key, 'ep') ~= ARGV[10] or redis.call('HGET', key, 'ver') ~= ARGV[11]) then
    accepted = false
  elseif stored and tonumber(stored) > tonumber(ARGV[9]) then
    accepted = false
  else
    if redis.call('HGET', key, 'auth') ~= ARGV[8] or stored ~= ARGV[9] then
      redis.call('HSET', key, 'auth', ARGV[8], 'asv', ARGV[9])
      changed = true
    end
    -- Every accepted auth write moves the version, even an unchanged one, so
    -- a conditional republish that read the record earlier is refused.
    mutated = true
  end
else
  return redis.error_reply('unknown credential health op ' .. op)
end

if changed or mutated then
  redis.call('HINCRBY', key, 'ver', 1)
end
redis.call('PEXPIRE', key, ARGV[6])
local all = redis.call('HGETALL', key)
if changed or mutated then
  local record = {}
  for i = 1, #all, 2 do record[all[i]] = all[i + 1] end
  redis.call('PUBLISH', ARGV[7], cjson.encode({origin = ARGV[2], id = tonumber(ARGV[3]), state = record}))
end
local result = {accepted and '1' or '0', changed and '1' or '0', became and '1' or '0'}
for i = 1, #all do result[#result + 1] = all[i] end
return result
`)

// renewLeaseScript extends a lease only while the caller still owns it.
// KEYS[1]: lease key. ARGV: owner token, ttlMS. Returns 1 when renewed.
var renewLeaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

// releaseLeaseScript deletes a lease only while the caller still owns it.
// KEYS[1]: lease key. ARGV: owner token. Returns 1 when released.
var releaseLeaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)
