-- Admit a request to a tenant's token bucket, or say how long it must wait.
--
-- KEYS[1]  the bucket: a hash of tokens (the balance) and ts (when the
--          balance was last written, in seconds)
-- ARGV[1]  limit: tokens per minute, which is also the bucket's capacity
-- ARGV[2]  cost: the request's tokens
--
-- Returns 0 when the request is admitted and charged, otherwise the whole
-- seconds until the bucket could admit it. A refusal changes nothing.

local key = KEYS[1]
local limit = tonumber(ARGV[1])
local cost = tonumber(ARGV[2])

-- Redis's clock, never the caller's, so every gateway replica sees the same
-- time. TIME replies seconds and microseconds.
local clock = redis.call('TIME')
local now = tonumber(clock[1]) + tonumber(clock[2]) / 1000000

-- A missing key is a full bucket.
local state = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(state[1]) or limit
local ts = tonumber(state[2]) or now

-- Refill for the time since the last write, up to the limit. If the clock
-- went back, no time has passed.
tokens = math.min(limit, tokens + math.max(0, now - ts) * limit / 60)

-- A request bigger than the limit needs only a full bucket, or it could
-- never get in.
local need = math.min(cost, limit)
if tokens < need then
  -- Redis truncates a number a script returns to an integer, so round up
  -- here. need > tokens, so the wait is at least 1.
  return math.ceil((need - tokens) * 60 / limit)
end

-- Charge the full cost. The balance may go negative: debt, which holds back
-- the next request until the refill has paid it off.
tokens = tokens - cost

-- Redis formats the numbers passed to redis.call itself: whole ones as
-- integers, the rest with every digit needed to read back the same number.
-- (Lua's tostring would keep only 14 digits.)
redis.call('HSET', key, 'tokens', tokens, 'ts', now)

-- Expire when the bucket would be full again. A missing key reads as full,
-- so expiring sooner would forgive debt. EXPIRE takes whole seconds, and 0
-- would delete the key at once.
--
-- Redis doesn't roll back the write above if this fails, so it mustn't. It
-- can't: Go caps cost at MaxCost, and the balance covered min(cost, limit)
-- before the charge, so it is still above -MaxCost, and the expiry is one
-- EXPIRE accepts.
redis.call('EXPIRE', key, math.max(1, math.ceil((limit - tokens) * 60 / limit)))
return 0
