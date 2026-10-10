-- Admit a request to a tenant's token bucket, or say how long it must wait.
--
-- KEYS[1]  the bucket: a hash of tokens (the balance), ts (when the balance
--          was last written, in seconds) and limit (the limit it was last
--          written and its expiry set at)
-- ARGV[1]  limit: tokens per minute, which is also the bucket's capacity
-- ARGV[2]  cost: the request's tokens
--
-- Returns 0 when the request is admitted and charged, otherwise the whole
-- seconds until the bucket could admit it. A refusal charges nothing, and
-- writes nothing unless the limit changed: then it saves the refill and
-- expiry at the new limit.

local key = KEYS[1]
local limit = tonumber(ARGV[1])
local cost = tonumber(ARGV[2])

-- Redis's clock, never the caller's, so every gateway replica sees the same
-- time. TIME replies seconds and microseconds.
local clock = redis.call('TIME')
local now = tonumber(clock[1]) + tonumber(clock[2]) / 1000000

-- A missing key is a full bucket.
local state = redis.call('HMGET', key, 'tokens', 'ts', 'limit')
local tokens = tonumber(state[1]) or limit
local ts = tonumber(state[2]) or now

-- Refill for the time since the last write, up to the limit. If the clock
-- went back, no time has passed.
tokens = math.min(limit, tokens + math.max(0, now - ts) * limit / 60)

-- A request bigger than the limit needs only a full bucket, or it could
-- never get in.
local need = math.min(cost, limit)
local wait = 0
if tokens < need then
  -- Redis truncates a number a script returns to an integer, so round up
  -- here. need > tokens, so the wait is at least 1.
  wait = math.ceil((need - tokens) * 60 / limit)
  -- At the limit the bucket was written at, write nothing: nothing was
  -- charged and the refill is linear, so the stored balance and ts give the
  -- same refill from here on (or less, if the clock went back), and the
  -- expiry still falls when the bucket is full. That keeps a refusal working
  -- while Redis rejects writes. A changed limit must be written, or an expiry
  -- set at the old one could fire while debt remains; so must a key from
  -- before the scripts stored the limit, which has none.
  if tonumber(state[3]) == limit then
    return wait
  end
else
  -- Charge the full cost. The balance may go negative: debt, which holds back
  -- the next request until the refill has paid it off.
  tokens = tokens - cost
end

-- Redis formats the numbers passed to redis.call itself: whole ones as
-- integers, the rest with every digit needed to read back the same number.
-- (Lua's tostring would keep only 14 digits.)
redis.call('HSET', key, 'tokens', tokens, 'ts', now, 'limit', limit)

-- Expire when the bucket would be full again. A missing key reads as full,
-- so expiring sooner would forgive debt. EXPIRE takes whole seconds, and 0
-- would delete the key at once.
--
-- Redis doesn't roll back the write above if this fails, so it mustn't. It
-- can't: Go caps cost at MaxCost, and an admitted request's balance covered
-- min(cost, limit) before the charge. Existing debt is at most MaxCost, so
-- either path leaves an expiry EXPIRE accepts.
redis.call('EXPIRE', key, math.max(1, math.ceil((limit - tokens) * 60 / limit)))
return wait
