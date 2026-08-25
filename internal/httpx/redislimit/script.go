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
// KEYS[1] rolling-window ZSET (admission instants), KEYS[2] next-allowed-at,
// KEYS[3] in-flight ZSET (lease expiries, scored by deadline).
// ARGV quota, window_ms, min_delay_ms, slack_ms, nonce, cluster_concurrency,
// lease_ms, retry_ms.
//
// Returns 0 when the request is admitted AND booked, or the wait in
// milliseconds when it is not. A waiting caller books nothing: it may abandon
// the wait, and a reservation left behind by an abandoned caller would starve
// the host for a whole window.
//
// cluster_concurrency > 0 turns on the in-flight cap. It is the knob that lets
// this limiter do spacing and concurrency at once, which the release-side bump
// alone cannot: see the package doc. When it is 0 every line touching KEYS[3]
// is skipped and the script behaves exactly as it did before the cap existed.
const acquireScript = `
local quota  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local delay  = tonumber(ARGV[3])
local slack  = tonumber(ARGV[4])
local nonce  = ARGV[5]
local cc     = tonumber(ARGV[6])
local lease  = tonumber(ARGV[7])
local retry  = tonumber(ARGV[8])

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

-- The in-flight cap is checked LAST of the three, and only when nothing else
-- already owes a wait. Checking it first would purge leases and read a count
-- that a quota wait then throws away, and the purge is the one side effect in
-- this script that a caller who waits must not pay for.
if cc > 0 and admit <= now then
  -- A lease whose deadline has passed belongs to a pod that died mid-request:
  -- nothing will ever release it, so expiry IS the release. Purging on every
  -- attempt is what makes the cap self-healing without heartbeats.
  redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', now)
  if redis.call('ZCARD', KEYS[3]) >= cc then
    -- Report a short poll, not the time to the earliest lease deadline. A
    -- request almost always ends by release long before its lease expires, so
    -- the deadline is a wildly pessimistic estimate of the next opening. The
    -- caller re-attempts, which is the same jittered-retry protocol the quota
    -- path uses. Never report longer than the earliest deadline: past that the
    -- slot is free for certain.
    local head = redis.call('ZRANGE', KEYS[3], 0, 0, 'WITHSCORES')
    local wait = retry
    if head[2] then
      local until_expiry = tonumber(head[2]) - now
      if until_expiry < wait then
        wait = until_expiry
      end
    end
    if wait < 1 then
      wait = 1
    end
    return wait
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
if cc > 0 then
  -- Scored by deadline so the purge above is one ZREMRANGEBYSCORE. The member
  -- is the bare nonce because freeLeaseScript has to remove it by name, and it
  -- is the caller that holds the nonce, not the score.
  redis.call('ZADD', KEYS[3], now + lease, nonce)
  redis.call('PEXPIRE', KEYS[3], lease + slack)
end
return 0
`

// freeLeaseScript returns one in-flight slot.
//
// KEYS[1] in-flight ZSET. ARGV nonce.
//
// This is the release path when the in-flight cap is on, and it deliberately
// does NOT bump next-allowed-at the way releaseScript does. With a cap in play
// the minimum delay spaces SENDS, not gaps: bumping from completion would push
// every admission behind the slowest request in flight and collapse the cap
// back to one, which is the exact behaviour the cap exists to escape.
const freeLeaseScript = `
redis.call('ZREM', KEYS[1], ARGV[1])
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
	acquire   = redis.NewScript(acquireScript)
	release   = redis.NewScript(releaseScript)
	freeLease = redis.NewScript(freeLeaseScript)
)
