-- Add delta tokens to a tenant's bucket after the fact: a refund when a
-- request used fewer tokens than it was charged, a charge when it used more.
-- The clock, refill and writes work as in allow.lua.
--
-- KEYS[1]  the bucket, as in allow.lua
-- ARGV[1]  limit: tokens per minute, which is also the bucket's capacity
-- ARGV[2]  delta: tokens to add; negative to charge
-- ARGV[3]  maxCost: the deepest debt a charge may leave (MaxCost in
--          ratelimit.go)
--
-- Returns 0. (A script that returns nothing replies nil, which go-redis
-- reports as the error redis.Nil.)

local key = KEYS[1]
local limit = tonumber(ARGV[1])
local delta = tonumber(ARGV[2])
local maxCost = tonumber(ARGV[3])

local clock = redis.call('TIME')
local now = tonumber(clock[1]) + tonumber(clock[2]) / 1000000

local state = redis.call('HMGET', key, 'tokens', 'ts')
-- A missing key is a full bucket, which a refund can't raise: leave it
-- missing rather than write a full bucket.
if not state[1] and delta >= 0 then
  return 0
end
local tokens = tonumber(state[1]) or limit
local ts = tonumber(state[2]) or now

tokens = math.min(limit, tokens + math.max(0, now - ts) * limit / 60)

-- A refund never fills the bucket past its limit. A charge may take it into
-- debt, but no deeper than maxCost, however many charges arrive. That keeps
-- the expiry below one EXPIRE accepts: Redis wouldn't roll back the write if
-- EXPIRE failed.
tokens = math.max(-maxCost, math.min(limit, tokens + delta))

redis.call('HSET', key, 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', key, math.max(1, math.ceil((limit - tokens) * 60 / limit)))
return 0
