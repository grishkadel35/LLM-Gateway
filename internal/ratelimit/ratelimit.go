// Package ratelimit enforces each tenant's tokens-per-minute limit with a
// token bucket in Redis, shared by every gateway replica.
//
// A tenant's bucket holds up to one minute's limit and refills continuously
// at that rate. A request gets in when the bucket covers its cost, or the
// whole limit when it costs more than that, and is then charged its full
// cost. So the balance can go negative (debt), and debt holds back the
// tenant's next request until the refill has paid it off.
//
// Each operation is one Lua script (allow.lua, adjust.lua). Redis runs a
// script atomically, so concurrent requests can't spend the same tokens. The
// scripts read Redis's clock, so replicas whose clocks disagree still refill
// alike.
//
// A bucket's key expires when the bucket would be full again at its limit. A
// missing key reads as a full bucket, so an idle tenant costs Redis nothing,
// and expiry never forgives debt, with one exception: after a limit is
// lowered, an idle tenant's key can still expire when its bucket would have
// been full at the old limit.
package ratelimit

import (
	"context"
	// Go note: //go:embed (explained in internal/db) needs the embed package
	// imported even when it fills a string. Nothing here names the package,
	// so the import is blank.
	_ "embed"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// MaxCost is the most one request is charged: Allow clamps a larger cost to
// it. A client sets its own output cap, so a request's cost can be anything
// up to math.MaxInt64. MaxCost is also the deepest debt a bucket can reach.
//
// 2^40 tokens (about 1.1e12) is far above any real request, yet small enough
// that the scripts' float64 arithmetic stays exact and every expiry they set
// is one EXPIRE accepts, for any limit of 1 or more. Unclamped, a script
// could fail on EXPIRE after writing the balance, which Redis doesn't roll
// back: the bucket would be left in vast debt that never expires.
//
// A caller that later settles a charge with Adjust must start from what was
// charged, min(cost, MaxCost), or its refund would forgive real usage.
const MaxCost = 1 << 40

// maxWait is the longest wait Allow reports: the most whole seconds a
// time.Duration holds, about 292 years. Only a debt near MaxCost on a limit
// below about 7,000 tokens a minute takes longer to pay off.
const maxWait = math.MaxInt64 / time.Second * time.Second

//go:embed allow.lua
var allowLua string

//go:embed adjust.lua
var adjustLua string

// Go note: Redis caches every script it runs under the SHA-1 of its source.
// Script.Run sends only that hash (EVALSHA). Only when Redis replies that it
// doesn't have the script, after a restart say, does Run send the whole
// source (EVAL), which caches it again.
var (
	allowScript  = redis.NewScript(allowLua)
	adjustScript = redis.NewScript(adjustLua)
)

// Limiter checks requests against their tenant's bucket.
type Limiter struct {
	rdb redis.Scripter
}

// New returns a Limiter that keeps its buckets in rdb, usually a
// *redis.Client.
//
// rdb must not retry commands: in go-redis, set MaxRetries: -1 (0 means the
// default, 3). The scripts aren't idempotent: after a lost reply (io.EOF,
// ECONNRESET), a retry sends the EVALSHA again, which charges or refunds
// twice.
func New(rdb redis.Scripter) *Limiter {
	return &Limiter{rdb: rdb}
}

// key names a tenant's bucket. The braces mark the tenant ID as the key's
// Redis Cluster hash tag.
func key(tenantID string) string {
	return "ratelimit:{" + tenantID + "}"
}

// Allow charges a request of cost tokens, at most MaxCost, to the tenant's
// bucket, which holds up to limit tokens and refills at limit tokens per
// minute. It reports whether the request may go ahead and, if not, how long
// until the bucket could admit it: whole seconds, at least one. A refused
// request is not charged, and unless limit has changed, nothing is written.
// The bucket keeps the latest Allow's limit, which Adjust settles at.
//
// A limit below 1 or a negative cost is an error and never reaches Redis: the
// scripts divide by the limit.
func (l *Limiter) Allow(ctx context.Context, tenantID string, limit, cost int64) (allowed bool, retryAfter time.Duration, err error) {
	if limit <= 0 {
		return false, 0, fmt.Errorf("ratelimit: limit must be positive, got %d", limit)
	}
	if cost < 0 {
		return false, 0, fmt.Errorf("ratelimit: cost must not be negative, got %d", cost)
	}
	// Go note: min and max are built into the language (since Go 1.21), for
	// any ordered type.
	cost = min(cost, MaxCost)

	secs, err := allowScript.Run(ctx, l.rdb, []string{key(tenantID)}, limit, cost).Int64()
	if err != nil {
		return false, 0, err
	}
	// The script replies 0 for admitted, otherwise the seconds to wait, which
	// can outrun a time.Duration: compare before converting.
	if secs > int64(maxWait/time.Second) {
		return false, maxWait, nil
	}
	return secs == 0, time.Duration(secs) * time.Second, nil
}

// Adjust settles a charge once a request's real cost is known: a positive
// delta refunds tokens, a negative one charges more. It settles at the
// bucket's own limit, the latest Allow's, as the caller's may be out of date
// by the time its request ends. limit is used only when the bucket has none:
// its key is missing, or predates the stored limit. A refund never fills the
// bucket past its limit. A charge may take it into debt, but never deeper
// than MaxCost, however many charges arrive, so any delta is safe.
//
// As with Allow, a limit below 1 is an error and never reaches Redis.
func (l *Limiter) Adjust(ctx context.Context, tenantID string, limit, delta int64) error {
	if limit <= 0 {
		return fmt.Errorf("ratelimit: limit must be positive, got %d", limit)
	}
	// Go note: Run takes its arguments as ...any, where an untyped constant
	// becomes an int, too small for MaxCost on 32-bit platforms. int64(...)
	// gives it a type that holds it everywhere.
	return adjustScript.Run(ctx, l.rdb, []string{key(tenantID)}, limit, delta, int64(MaxCost)).Err()
}
