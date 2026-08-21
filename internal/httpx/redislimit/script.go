package redislimit

import "github.com/redis/go-redis/v9"

// Both scripts read the clock with Redis' own TIME, so pod clock skew cannot
// affect pacing: every replica books against one clock. Redis has allowed
// writes after a non-deterministic command since 5.0 (effects replication),
// which is what makes a TIME-then-ZADD script legal.
//
// Every key a script writes gets a TTL in the same call. The shared Redis this
// targets runs `noeviction` with a tight memory limit, so one un-TTL'd key per
// host is a leak that eventually takes the whole instance down. The slack added
// to each TTL absorbs the gap between the score and the moment the last reader
// needs it.

// acquireScript admits one request or reports how long the caller must wait.
//
// KEYS[1] rolling-window ZSET (admission instants), KEYS[2] next-allowed-at.
// ARGV quota, window_ms, min_delay_ms, slack_ms, nonce.
//
// Returns 0 when the request is admitted AND booked, or the wait in
// milliseconds when it is not. A waiting caller books nothing: it may abandon
// the wait, and a reservation left behind by an abandoned caller would starve
// the host for a whole window.
const acquireScript = `
local quota  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local delay  = tonumber(ARGV[3])
local slack  = tonumber(ARGV[4])
local nonce  = ARGV[5]

local t = redis.call('TIME')
local now = t[1] * 1000 + math.floor(t[2] / 1000)

local admit = now
local nxt = redis.call('GET', KEYS[2])
if nxt and tonumber(nxt) > admit then
  admit = tonumber(nxt)
end

if quota > 0 then
  redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - window)
  local n = redis.call('ZCARD', KEYS[1])
  if n >= quota then
    -- The opening is not the oldest admission but the one 'quota' places back:
    -- everything newer than it holds a slot of its own.
    local idx = n - quota
    local slot = redis.call('ZRANGE', KEYS[1], idx, idx, 'WITHSCORES')
    local opening = tonumber(slot[2]) + window
    if opening > admit then
      admit = opening
    end
  end
end

if admit > now then
  return admit - now
end

if quota > 0 then
  -- The nonce keeps two admissions in the same millisecond as two members.
  redis.call('ZADD', KEYS[1], now, now .. '-' .. nonce)
  redis.call('PEXPIRE', KEYS[1], window + slack)
end
if delay > 0 then
  redis.call('SET', KEYS[2], now + delay, 'PX', delay + slack)
end
return 0
`

// releaseScript pushes the next admission to min_delay after THIS instant.
//
// KEYS[1] next-allowed-at. ARGV min_delay_ms, slack_ms.
//
// The in-process limiter measures its minimum delay from the previous request's
// completion, not from its send. Without this bump the distributed limiter
// would space sends instead of gaps and so send harder than the local one on
// the same settings. Bumping to the max never shortens a delay another replica
// already reserved.
const releaseScript = `
local delay = tonumber(ARGV[1])
local slack = tonumber(ARGV[2])
if delay <= 0 then
  return 0
end

local t = redis.call('TIME')
local now = t[1] * 1000 + math.floor(t[2] / 1000)

local target = now + delay
local cur = redis.call('GET', KEYS[1])
if cur and tonumber(cur) > target then
  target = tonumber(cur)
end
redis.call('SET', KEYS[1], target, 'PX', target - now + slack)
return target
`

var (
	acquire = redis.NewScript(acquireScript)
	release = redis.NewScript(releaseScript)
)
