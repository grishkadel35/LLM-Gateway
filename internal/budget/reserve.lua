-- Reserve the most a request could cost against its tenant's budget for the
-- month, or refuse it.
--
-- KEYS[1]  the tenant's hash for the month: spent (µ$ settled, plus the
--          reservations in flight), gen (a token, new each time the hash is
--          built) and warned (set once a reservation has taken spent to 80%
--          of the budget)
-- KEYS[2]  the hash's missed counter: how many settles have found it missing
--          (settle.lua)
-- ARGV[1]  budget, µ$. Spent in budget.go passes -1, which refuses every
--          reservation, to read spent.
-- ARGV[2]  amount: the reservation, µ$
-- ARGV[3]  spent for a missing hash, from a rebuild
-- ARGV[4]  gen for a missing hash
-- ARGV[5]  when a missing hash expires, in Unix seconds
-- ARGV[6]  the missed counter as the rebuild read it, before its other reads
--
-- ARGV[3] to ARGV[6] come only after a rebuild. Replies:
--   {-2}             the hash is missing, and a settle has found it missing
--                    since the rebuild read the counter: rebuild again.
--                    Nothing is written.
--   {-1}             the hash is missing and no rebuild came with the call
--   {0, spent}       refused, as spent + amount is over the budget. Nothing
--                    is written, beyond a hash this call built.
--   {1, spent, gen}  admitted: amount is added to spent
--   {2, spent, gen}  admitted, and the first reservation in this hash to take
--                    spent to 80% of the budget or more
--
-- Lua's numbers are doubles, exact for integers below 2^53. Budget and amount
-- are at most 1e15 (Go checks), and an admitted spent is at most the budget,
-- so every number compared below is exact. Settlement can take spent higher,
-- but only past every budget, where rounding can't turn a refusal into an
-- admission.

local key = KEYS[1]
local budget = tonumber(ARGV[1])
local amount = tonumber(ARGV[2])

local state = redis.call('HMGET', key, 'spent', 'gen')
local spent, gen = state[1], state[2]
if not spent then
  if not ARGV[3] then
    return {-1}
  end
  -- A settle that found the hash missing after the rebuild read the counter
  -- may have queued its usage row after the rebuild read the queue, and had
  -- it committed after the SUM: counted in neither. Build nothing from those
  -- reads.
  if tonumber(redis.call('GET', KEYS[2]) or 0) ~= tonumber(ARGV[6]) then
    return {-2}
  end
  -- Built only while still missing: another request or replica may have
  -- built the hash since this caller read Postgres, and then both
  -- reservations count in that one.
  spent, gen = ARGV[3], ARGV[4]
  redis.call('HSET', key, 'spent', spent, 'gen', gen)
  redis.call('EXPIREAT', key, ARGV[5])
end

if tonumber(spent) + amount > budget then
  -- spent as Redis stores it, a string, which Go reads exactly however large
  -- it has grown.
  return {0, spent}
end

-- Redis doesn't roll back a script's writes when a later command fails, so
-- nothing from here on can fail. HINCRBY adds as a 64-bit integer, and the
-- check above keeps the sum within the budget.
spent = redis.call('HINCRBY', key, 'spent', ARGV[2])
local reply = 1
-- spent >= 80% of the budget, in integers. HSETNX sets warned only if it
-- isn't set yet, so only the first reservation there reports it.
if spent * 5 >= budget * 4 and redis.call('HSETNX', key, 'warned', 1) == 1 then
  reply = 2
end
return {reply, spent, gen}
