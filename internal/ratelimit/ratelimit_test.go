package ratelimit

import (
	"context"
	"crypto/rand"
	"maps"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/grishkadel/llm-gateway/internal/redistest"
)

// The limits below keep each test's own refill from flipping an outcome: at
// 100,000/min the bucket refills 1,667 tokens a second, so moving a wait by a
// whole second would take a second of test time.

// TestBadArgumentsNeverReachRedis uses a Limiter with no client: if a call
// reached Redis, the nil client would panic.
func TestBadArgumentsNeverReachRedis(t *testing.T) {
	ctx := context.Background()
	l := New(nil)
	for _, tc := range []struct{ limit, cost int64 }{{0, 1}, {-1, 1}, {100_000, -1}} {
		if _, _, err := l.Allow(ctx, "tn_test", tc.limit, tc.cost); err == nil {
			t.Errorf("Allow(limit %d, cost %d): want an error", tc.limit, tc.cost)
		}
	}
	for _, limit := range []int64{0, -1} {
		if err := l.Adjust(ctx, "tn_test", limit, 1); err == nil {
			t.Errorf("Adjust(limit %d): want an error", limit)
		}
	}
}

// newBucket returns a Limiter over the test Redis, that client, and a fresh
// tenant.
func newBucket(t *testing.T) (*Limiter, *redis.Client, string) {
	t.Helper()
	rdb := redistest.New(t)
	return New(rdb), rdb, newTenant(t, rdb)
}

// newTenant returns a tenant ID no other test uses. Its key is deleted when
// the test ends.
func newTenant(t *testing.T, rdb *redis.Client) string {
	t.Helper()
	tenantID := "tn_test_" + rand.Text()
	t.Cleanup(func() { rdb.Del(context.Background(), key(tenantID)) })
	return tenantID
}

func mustAllow(t *testing.T, l *Limiter, tenantID string, limit, cost int64) {
	t.Helper()
	ok, wait, err := l.Allow(context.Background(), tenantID, limit, cost)
	if err != nil || !ok {
		t.Fatalf("Allow(limit %d, cost %d) = %v, %v, %v; want admitted", limit, cost, ok, wait, err)
	}
}

func mustRefuse(t *testing.T, l *Limiter, tenantID string, limit, cost int64, want time.Duration) {
	t.Helper()
	ok, wait, err := l.Allow(context.Background(), tenantID, limit, cost)
	if err != nil || ok || wait != want {
		t.Fatalf("Allow(limit %d, cost %d) = %v, %v, %v; want refused with %v to wait", limit, cost, ok, wait, err, want)
	}
}

// balance reads the bucket as the last script wrote it, without the refill
// since.
func balance(t *testing.T, rdb *redis.Client, tenantID string) float64 {
	t.Helper()
	b, err := rdb.HGet(context.Background(), key(tenantID), "tokens").Float64()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestThreshold: three 30,000-token requests fit a fresh 100,000/min bucket.
// The fourth waits for the 20,000 tokens it lacks: 12 s.
func TestThreshold(t *testing.T) {
	l, _, tenant := newBucket(t)
	for range 3 {
		mustAllow(t, l, tenant, 100_000, 30_000)
	}
	mustRefuse(t, l, tenant, 100_000, 30_000, 12*time.Second)
}

func TestConcurrentRequestsCannotOverAdmit(t *testing.T) {
	l, _, tenant := newBucket(t)

	type result struct {
		ok   bool
		wait time.Duration
		err  error
	}
	results := make([]result, 50)

	// Go note: a sync.WaitGroup waits for a group of goroutines. wg.Go runs
	// a function in a new goroutine and counts it in; wg.Wait blocks until
	// every one has returned. Each goroutine writes only its own element of
	// results, so they need no lock.
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			ok, wait, err := l.Allow(context.Background(), tenant, 100_000, 10_000)
			results[i] = result{ok, wait, err}
		})
	}
	wg.Wait()

	admitted := 0
	for _, r := range results {
		switch {
		case r.err != nil:
			t.Fatal(r.err)
		case r.ok:
			admitted++
		case r.wait != 6*time.Second: // a whole 10,000 to refill
			t.Errorf("refused with %v to wait, want 6s", r.wait)
		}
	}
	if admitted != 10 {
		t.Errorf("admitted %d of 50 requests for 10,000 of a 100,000 bucket, want exactly 10", admitted)
	}
}

// TestDrainedBucketRefills: at 6,000,000/min the bucket refills 100,000 tokens
// a second, so a 15,000-token request refused on an empty bucket fits 150 ms
// later.
func TestDrainedBucketRefills(t *testing.T) {
	l, _, tenant := newBucket(t)
	mustAllow(t, l, tenant, 6_000_000, 6_000_000)
	// 150 ms, rounded up to whole seconds.
	mustRefuse(t, l, tenant, 6_000_000, 15_000, time.Second)
	time.Sleep(200 * time.Millisecond)
	mustAllow(t, l, tenant, 6_000_000, 15_000)
}

func TestRefillStopsAtTheLimit(t *testing.T) {
	l, rdb, tenant := newBucket(t)
	mustAllow(t, l, tenant, 6_000_000, 1)
	time.Sleep(20 * time.Millisecond) // 2,000 tokens of refill, if nothing capped it
	mustAllow(t, l, tenant, 6_000_000, 1)
	if got := balance(t, rdb, tenant); got != 6_000_000-1 {
		t.Errorf("balance = %v, want %v: refill stops at the limit", got, 6_000_000-1)
	}
}

// TestOversizeRequestRunsIntoDebt: a request bigger than the whole limit gets
// in on a full bucket and is charged in full. The debt then holds back the
// next request.
func TestOversizeRequestRunsIntoDebt(t *testing.T) {
	l, rdb, tenant := newBucket(t)
	mustAllow(t, l, tenant, 100_000, 150_000)
	if got := balance(t, rdb, tenant); got != -50_000 {
		t.Errorf("balance = %v, want -50000", got)
	}
	// 51,000 tokens to refill: 30.6 s, rounded up.
	mustRefuse(t, l, tenant, 100_000, 1_000, 31*time.Second)
}

func TestOversizeRequestWaitsForAFullBucket(t *testing.T) {
	l, rdb, tenant := newBucket(t)
	mustAllow(t, l, tenant, 100_000, 30_000)
	// 30,000 tokens to refill: 18 s.
	mustRefuse(t, l, tenant, 100_000, 150_000, 18*time.Second)
	if got := balance(t, rdb, tenant); got < 70_000 || got >= 71_000 {
		t.Errorf("balance = %v, want 70000 plus refill: a refusal charges nothing", got)
	}
}

// TestRefusalAtAnUnchangedLimitWritesNothing: at the limit the bucket was
// written at, its stored balance, ts and expiry already say what a refusal's
// write would, so the refusal leaves them alone and keeps working while Redis
// rejects writes.
func TestRefusalAtAnUnchangedLimitWritesNothing(t *testing.T) {
	ctx := context.Background()
	l, rdb, tenant := newBucket(t)
	// The bucket's fields, and when its key expires.
	read := func() (map[string]string, time.Duration) {
		t.Helper()
		fields, err := rdb.HGetAll(ctx, key(tenant)).Result()
		if err != nil {
			t.Fatal(err)
		}
		expiry, err := rdb.PExpireTime(ctx, key(tenant)).Result()
		if err != nil {
			t.Fatal(err)
		}
		return fields, expiry
	}

	mustAllow(t, l, tenant, 100_000, 90_000)
	fields, expiry := read()
	time.Sleep(10 * time.Millisecond) // so that an expiry set now would fall later
	// 20,000 tokens to refill: 12 s.
	mustRefuse(t, l, tenant, 100_000, 30_000, 12*time.Second)

	gotFields, gotExpiry := read()
	// Go note: the maps package (since Go 1.21) has generic helpers that work
	// on any map type. maps.Equal reports whether two maps hold the same keys
	// with the same values.
	if !maps.Equal(gotFields, fields) {
		t.Errorf("bucket after the refusal = %v, want it unchanged, %v", gotFields, fields)
	}
	if gotExpiry != expiry {
		t.Errorf("key expires at %v after the refusal, want it unchanged, %v (since the Unix epoch)", gotExpiry, expiry)
	}
}

func TestRefusalAfterLoweringLimitPreservesDebt(t *testing.T) {
	ctx := context.Background()
	l, rdb, tenant := newBucket(t)
	mustAllow(t, l, tenant, 100_000, 150_000)
	if got := ttlSeconds(t, rdb, tenant); got != 90 {
		t.Fatalf("initial TTL = %d s, want 90", got)
	}

	mustRefuse(t, l, tenant, 1_000, 1_000, 3060*time.Second)
	ttl, err := rdb.PTTL(ctx, key(tenant)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if want := 3060 * time.Second; ttl > want || ttl <= want-500*time.Millisecond {
		t.Errorf("TTL after lowering limit = %v, want just under %v", ttl, want)
	}
	// At 1,000/min a token refills every 60 ms, which a busy machine can take
	// between two calls. Allow the second of refill that the exact wait above
	// already allows.
	if got := balance(t, rdb, tenant); got <= -50_000 || got >= -50_000+1_000.0/60 {
		t.Errorf("balance = %v, want -50000 plus under a second of refill, without a charge", got)
	}
}

func TestAdjustRefundsUpToTheLimitAndChargesIntoDebt(t *testing.T) {
	ctx := context.Background()
	l, rdb, tenant := newBucket(t)
	mustAllow(t, l, tenant, 100_000, 30_000)

	if err := l.Adjust(ctx, tenant, 100_000, 50_000); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, rdb, tenant); got != 100_000 {
		t.Errorf("70,000 + a 50,000 refund: balance = %v, want the limit, 100000", got)
	}

	if err := l.Adjust(ctx, tenant, 100_000, -150_000); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, rdb, tenant); got != -50_000 {
		t.Errorf("100,000 - a 150,000 charge: balance = %v, want -50000", got)
	}
}

// TestAdjustOnAMissingKey: a missing key is a full bucket. A refund leaves it
// missing; a charge starts from the limit.
func TestAdjustOnAMissingKey(t *testing.T) {
	ctx := context.Background()
	l, rdb, tenant := newBucket(t)

	for _, delta := range []int64{0, 5_000} {
		if err := l.Adjust(ctx, tenant, 100_000, delta); err != nil {
			t.Fatalf("Adjust(%d): %v", delta, err)
		}
	}
	if n := rdb.Exists(ctx, key(tenant)).Val(); n != 0 {
		t.Fatal("refunding a full bucket created its key")
	}

	if err := l.Adjust(ctx, tenant, 100_000, -30_000); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, rdb, tenant); got != 70_000 {
		t.Errorf("balance = %v, want 70000", got)
	}
}

// TestAdjustAfterLoweringLimitPreservesDebt: Adjust's caller passes the limit
// it looked up when its request started. Once the limit is lowered, that's the
// old one, and refilling at it would pay off debt the new limit hasn't.
func TestAdjustAfterLoweringLimitPreservesDebt(t *testing.T) {
	ctx := context.Background()
	l, rdb, tenant := newBucket(t)
	mustAllow(t, l, tenant, 100_000, 150_000)
	// Lowered to 1,000/min: 51,000 tokens to refill.
	mustRefuse(t, l, tenant, 1_000, 1_000, 3060*time.Second)

	// A request admitted at the old limit ends, refunding 5,000.
	if err := l.Adjust(ctx, tenant, 100_000, 5_000); err != nil {
		t.Fatal(err)
	}
	// As in TestRefusalAfterLoweringLimitPreservesDebt, allow a second of
	// refill at 1,000/min.
	if got := balance(t, rdb, tenant); got <= -45_000 || got >= -45_000+1_000.0/60 {
		t.Errorf("balance = %v, want -45000 plus under a second of refill at 1,000/min", got)
	}
	ttl, err := rdb.PTTL(ctx, key(tenant)).Result()
	if err != nil {
		t.Fatal(err)
	}
	// 46,000 tokens to refill at 1,000/min.
	if want := 2760 * time.Second; ttl > want || ttl <= want-500*time.Millisecond {
		t.Errorf("TTL = %v, want just under %v", ttl, want)
	}
}

// redisNow reads Redis's clock, which the scripts go by, in seconds.
func redisNow(t *testing.T, rdb *redis.Client) float64 {
	t.Helper()
	now, err := rdb.Time(context.Background()).Result()
	if err != nil {
		t.Fatal(err)
	}
	return float64(now.UnixMicro()) / 1e6
}

// TestAdjustRefillsAtTheBucketsLimit: a bucket stored at 60,000/min, empty 3 s
// ago, has refilled 3,000 tokens, whatever limit Adjust's caller passes.
func TestAdjustRefillsAtTheBucketsLimit(t *testing.T) {
	ctx := context.Background()
	l, rdb, tenant := newBucket(t)
	if err := rdb.HSet(ctx, key(tenant), "tokens", 0, "ts", redisNow(t, rdb)-3, "limit", 60_000).Err(); err != nil {
		t.Fatal(err)
	}

	if err := l.Adjust(ctx, tenant, 6_000_000, 0); err != nil {
		t.Fatal(err)
	}
	// Plus under a second's refill, 1,000 tokens, for the calls in between.
	if got := balance(t, rdb, tenant); got < 3_000 || got >= 4_000 {
		t.Errorf("balance = %v, want 3000 plus under a second of refill at the bucket's 60,000/min", got)
	}
	if got := rdb.HGet(ctx, key(tenant), "limit").Val(); got != "60000" {
		t.Errorf("limit = %q, want the bucket's own, 60000", got)
	}
}

// TestKeyWithoutAStoredLimit: a key written before the scripts stored the
// limit has none. Adjust settles it at the caller's limit, and a refusal, which
// can't tell whether the limit changed, writes it.
func TestKeyWithoutAStoredLimit(t *testing.T) {
	ctx := context.Background()
	rdb := redistest.New(t)
	l := New(rdb)
	// A bucket as the scripts wrote it before, empty 3 s ago.
	legacy := func() string {
		t.Helper()
		tenant := newTenant(t, rdb)
		if err := rdb.HSet(ctx, key(tenant), "tokens", 0, "ts", redisNow(t, rdb)-3).Err(); err != nil {
			t.Fatal(err)
		}
		return tenant
	}

	tenant := legacy()
	if err := l.Adjust(ctx, tenant, 60_000, 0); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, rdb, tenant); got < 3_000 || got >= 4_000 {
		t.Errorf("after Adjust: balance = %v, want 3000 plus under a second of refill at the caller's 60,000/min", got)
	}

	tenant = legacy()
	// 27,000 tokens to refill: 27 s.
	mustRefuse(t, l, tenant, 60_000, 30_000, 27*time.Second)
	if got := rdb.HGet(ctx, key(tenant), "limit").Val(); got != "60000" {
		t.Errorf("after a refusal: limit = %q, want the caller's, 60000", got)
	}
}

// TestKeyExpiresWhenTheBucketWouldBeFull: the TTL is the time until the
// bucket is full again, rounded up, and at least 1 s.
func TestKeyExpiresWhenTheBucketWouldBeFull(t *testing.T) {
	rdb := redistest.New(t)
	l := New(rdb)
	for _, tc := range []struct {
		name        string
		limit, cost int64
		want        time.Duration
	}{
		{"10,000 of 100,000 left", 100_000, 90_000, 54 * time.Second},
		{"50,000 in debt", 100_000, 150_000, 90 * time.Second},
		{"2.4 s from full", 100_000, 4_000, 3 * time.Second},
		{"full, after a request costing nothing", 100_000, 0, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tenant := newTenant(t, rdb)
			mustAllow(t, l, tenant, tc.limit, tc.cost)
			ttl, err := rdb.PTTL(context.Background(), key(tenant)).Result()
			if err != nil {
				t.Fatal(err)
			}
			// Read a moment after the script set it: well within half a second.
			if ttl > tc.want || ttl <= tc.want-500*time.Millisecond {
				t.Errorf("TTL = %v, want just under %v", ttl, tc.want)
			}
		})
	}
}

// TestMissingKeyReadsAsFull: a key that expired leaves a full bucket, as a
// fresh one does.
func TestMissingKeyReadsAsFull(t *testing.T) {
	l, rdb, tenant := newBucket(t)
	mustAllow(t, l, tenant, 100_000, 100_000)
	mustRefuse(t, l, tenant, 100_000, 100_000, 60*time.Second)

	if err := rdb.Del(context.Background(), key(tenant)).Err(); err != nil { // as expiry would
		t.Fatal(err)
	}
	mustAllow(t, l, tenant, 100_000, 100_000)
}

// ttlSeconds reads the key's TTL in whole seconds: below, they run too long
// for a time.Duration.
func ttlSeconds(t *testing.T, rdb *redis.Client, tenantID string) int64 {
	t.Helper()
	s, err := rdb.Do(context.Background(), "TTL", key(tenantID)).Int64()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestHugeCostIsClamped: a client sets its own output cap, so a cost can be
// anything up to math.MaxInt64. Clamped to MaxCost, it leaves a finite TTL
// that Redis accepted, even on a limit of 1 a minute.
func TestHugeCostIsClamped(t *testing.T) {
	ctx := context.Background()
	l, rdb, tenant := newBucket(t)

	mustAllow(t, l, tenant, 1, math.MaxInt64)
	if got := balance(t, rdb, tenant); got != 1-MaxCost {
		t.Errorf("balance = %v, want 1 - MaxCost = %d", got, int64(1-MaxCost))
	}
	if got, want := ttlSeconds(t, rdb, tenant), int64(60*MaxCost); got < want-1 || got > want {
		t.Errorf("TTL = %d s, want %d", got, want)
	}
	// Paying off MaxCost at 1 token a minute takes longer than a
	// time.Duration holds.
	mustRefuse(t, l, tenant, 1, 1, maxWait)

	if err := l.Adjust(ctx, tenant, 1, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, rdb, tenant); got != 1 {
		t.Errorf("after refunding math.MaxInt64: balance = %v, want the limit, 1", got)
	}
}

// TestDebtStopsAtMaxCost: however many huge charges arrive, the debt stops at
// MaxCost, so the expiry stays one Redis accepts.
func TestDebtStopsAtMaxCost(t *testing.T) {
	ctx := context.Background()
	l, rdb, tenant := newBucket(t)

	for range 3 {
		if err := l.Adjust(ctx, tenant, 1, math.MinInt64); err != nil {
			t.Fatal(err)
		}
	}
	if got := balance(t, rdb, tenant); got != -MaxCost {
		t.Errorf("balance = %v, want -MaxCost = %d", got, int64(-MaxCost))
	}
	if got, want := ttlSeconds(t, rdb, tenant), int64(60*(1+MaxCost)); got < want-1 || got > want {
		t.Errorf("TTL = %d s, want %d", got, want)
	}
}

// TestFractionalBalanceSurvivesExactly: any refill leaves a fractional
// balance, and the scripts must write it back without rounding it.
//
// The seeded ts lies in the future, as if Redis's clock had gone back. That
// freezes the refill, so the result is exact, and checks that the scripts
// count negative elapsed time as zero instead of draining the bucket.
func TestFractionalBalanceSurvivesExactly(t *testing.T) {
	ctx := context.Background()
	l, rdb, tenant := newBucket(t)
	start := 12345.678901234567 // 17 digits; Lua's tostring keeps 14
	want := start - 1_000
	seed := func() {
		t.Helper()
		if err := rdb.HSet(ctx, key(tenant), "tokens", start, "ts", int64(9_999_999_999)).Err(); err != nil {
			t.Fatal(err)
		}
	}

	seed()
	mustAllow(t, l, tenant, 100_000, 1_000)
	if got := balance(t, rdb, tenant); got != want {
		t.Errorf("after Allow: balance = %v, want exactly %v", got, want)
	}

	seed()
	if err := l.Adjust(ctx, tenant, 100_000, -1_000); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, rdb, tenant); got != want {
		t.Errorf("after Adjust: balance = %v, want exactly %v", got, want)
	}
}
