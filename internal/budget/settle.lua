-- Settle a reservation once its request is over: swap it for what the request
-- really cost.
--
-- KEYS[1]  the hash the reservation went into, as in reserve.lua
-- KEYS[2]  the hash's missed counter, as in reserve.lua
-- ARGV[1]  the hash's gen when the reservation was made
-- ARGV[2]  charge - amount: what to add to spent while the hash holds the
--          reservation
-- ARGV[3]  charge: what to add once the hash has been rebuilt without it
-- ARGV[4]  when the hash expires, in Unix seconds, which its counter shares
--
-- Go computes the adds and HINCRBY takes them as written, so no number written
-- here passes through a Lua double.
--
-- Returns spent as HINCRBY leaves it, or -1, which no spend can be, for a
-- missing hash. The spend comes back through a Lua double, which is exact up
-- to 2^53 µ$, well past every budget: Go caps them at 1e15.

local key = KEYS[1]
local gen = redis.call('HGET', key, 'gen')

-- A missing hash stays missing: built here, it would hold this one request
-- and none of the month before it. The next rebuild counts the request from
-- its usage row instead, which was queued before the request settled. Count
-- the settle, so that a rebuild under way, which may have read the queue
-- before the row was in it, reads again (reserve.lua).
--
-- The counter expires with the hash it stands for, so it can't expire, and
-- count from 1 again, during a rebuild. Neither command can fail: the counter
-- only ever holds INCR's integers.
if not gen then
  redis.call('INCR', KEYS[2])
  redis.call('EXPIREAT', KEYS[2], ARGV[4])
  return -1
end

-- Another gen means the hash was rebuilt since the reservation, so it doesn't
-- hold it: add the whole charge. If the rebuild also counted the request's
-- usage row, the request counts twice: over, never under.
local add = ARGV[3]
if gen == ARGV[1] then
  add = ARGV[2]
end
-- The only write here. HINCRBY fails only if spent would leave the 64-bit
-- range, and then writes nothing.
return redis.call('HINCRBY', key, 'spent', add)
