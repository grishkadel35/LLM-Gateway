// Package budget keeps each tenant's spend for the month in Redis, so that a
// paid request can be refused before any provider sees it when the most it
// could cost doesn't fit what is left of its tenant's budget.
//
// Money is integer micro-dollars (µ$). A tenant's month is one Redis hash (see
// key) holding:
//   - spent: the month's settled costs, plus the reservations in flight
//   - gen: a random token, new each time the hash is built
//   - warned: set once a reservation has taken spent to 80% of the budget
//
// Before a paid request goes out, Reserve adds the most it could cost to
// spent, if that fits the budget. Once the request is over, Settle swaps the
// reservation for what it really cost. Each is one Lua script (reserve.lua,
// settle.lua), and Redis runs a script atomically, so two requests can't both
// take the last of a budget.
//
// The record is usage_logs in Postgres, and a hash is only a running total of
// it. A missing hash (Redis lost it, or the month is new) is rebuilt from the
// cost of the tenant's rows still queued for Postgres plus the month's rows
// already there, and gets a new gen. A request that reserved in the old hash
// then settles its whole cost into the new one, which doesn't hold its
// reservation. One that settles while the hash is missing writes no hash: its
// usage row is queued by then, for the rebuild to count. It bumps the hash's
// missed counter instead (see missedKey), and a rebuild that finds the counter
// moved when it comes to write starts its reads again, as they may have missed
// that row.
//
// A rebuild counts everything this gateway's queue and Postgres hold when it
// reads them, and a row that moves between the two at least once. It misses
// rows queued on other gateway replicas, and rows the usage writer dropped.
// And the reservations of requests in flight when a hash went missing count
// again only once those requests settle.
//
// Every Redis or Postgres error is returned, and the caller fails closed.
package budget

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed" // for the //go:embed scripts below
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/grishkadel/llm-gateway/internal/tenant"
)

const (
	// scriptTimeout bounds each script. Redis answers in about a millisecond;
	// one that takes 100 ms is treated as down.
	scriptTimeout = 100 * time.Millisecond
	// rebuildTimeout bounds one rebuild's reads: the counter, the queue and
	// one indexed SUM. It also bounds a caller's whole rebuild, however many
	// times the reads start again.
	rebuildTimeout = 2 * time.Second
)

//go:embed reserve.lua
var reserveLua string

//go:embed settle.lua
var settleLua string

// Go note: a Redis hash is one key holding named fields, like a small map,
// read and written a field at a time. The scripts use three commands on one:
// HINCRBY adds to a field as a 64-bit integer, exactly; HSETNX sets a field
// only if it isn't set yet; and EXPIREAT deletes the key at a given Unix time,
// where EXPIRE counts seconds from now.
var (
	reserveScript = redis.NewScript(reserveLua)
	settleScript  = redis.NewScript(settleLua)
)

// reserve.lua's reply starts with one of these, or with 1 for an admission
// that doesn't warn. Its header has the rest.
const (
	replyRebuild = -2
	replyMissing = -1
	replyRefused = 0
	replyWarn    = 2
)

// settle.lua replies with this for a missing hash, and otherwise with the
// hash's spent, which is never negative.
const settleMissing = -1

// Ledger keeps tenants' monthly spend in Redis, and rebuilds it from Postgres
// when it's missing.
type Ledger struct {
	rdb    redis.Cmdable
	db     *sql.DB
	queued func(tenantID string) int64

	// Go note: a singleflight.Group runs a function once for any number of
	// callers that ask under the same key at the same time: the first caller
	// runs it, and the rest wait for it and share its result. After a Redis
	// restart, all of a tenant's requests find its hash missing at once, and
	// make one query between them, not one each.
	rebuilds singleflight.Group
}

// New returns a Ledger that keeps its hashes in rdb, usually a *redis.Client,
// and rebuilds them from usage_logs in db plus queued(tenantID): the cost of
// the tenant's usage rows queued for db and not yet inserted. In the gateway
// that is (*usage.Writer).Pending. queued must be safe for concurrent use.
//
// rdb must not retry commands: in go-redis, set MaxRetries: -1 (0 means the
// default, 3). The scripts aren't idempotent: after a lost reply, a retry
// would reserve or settle twice. It must also honour context deadlines
// (ContextTimeoutEnabled), or the timeouts here don't reach Redis.
func New(rdb redis.Cmdable, db *sql.DB, queued func(tenantID string) int64) *Ledger {
	return &Ledger{rdb: rdb, db: db, queued: queued}
}

// Period returns the UTC calendar month holding at: from start, inclusive, to
// end, exclusive. It is the budget's period.
func Period(at time.Time) (start, end time.Time) {
	y, m, _ := at.UTC().Date()
	start = time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// key names a tenant's hash for the month that starts at start, such as
// budget:{tn_1a2b}:2026-10. The braces mark the tenant ID as the key's Redis
// Cluster hash tag, as for the rate limiter.
func key(tenantID string, start time.Time) string {
	return "budget:{" + tenantID + "}:" + start.Format("2006-01")
}

// missedKey names the missed counter of the hash k: how many settles have
// found k missing. It shares k's hash tag, as the keys of one script must all
// sit in one Redis Cluster slot.
func missedKey(k string) string {
	return k + ":missed"
}

// Reservation is what Reserve did: whether the request may go ahead, and what
// Settle needs once it's over.
type Reservation struct {
	// Admitted reports that the reservation fit the budget and was added to
	// spent. A refused one wrote nothing, and has nothing to settle.
	Admitted bool
	// Spent is the month's spend in µ$: as the script left it when admitted,
	// this reservation included, or as it found it when refused.
	Spent int64
	// Warn reports that this was the first reservation in the hash to take
	// spent to 80% of the budget or more. The caller logs the warning.
	Warn bool
	// Amount is the reservation, in µ$.
	Amount int64

	// key and gen name the hash the reservation went into. expireAt is when
	// that hash expires, in Unix seconds, and with it its missed counter.
	key      string
	gen      int64
	expireAt int64
}

// Reserve adds amount, the most a request could cost, to its tenant's spend
// for the month holding at, if spent + amount is within budget. at is when the
// request arrived, the time its usage row records. A refusal writes nothing;
// an admitted reservation is settled with Settle once its request is over.
//
// A missing hash is rebuilt first (see the package doc). Each script gets
// 100 ms, and the rebuild 2 s, however many times it starts its reads again.
// Any error, from Redis or Postgres, is returned with nothing admitted.
//
// budget and amount must be between 0 and tenant.MaxBudgetMicros (1e15 µ$),
// which keeps every number reserve.lua compares below 2^53, where Lua's
// doubles are exact. Anything else is an error and never reaches Redis.
func (l *Ledger) Reserve(ctx context.Context, tenantID string, budget, amount int64, at time.Time) (Reservation, error) {
	if budget < 0 || budget > tenant.MaxBudgetMicros {
		return Reservation{}, fmt.Errorf("budget: budget must be between 0 and %d µ$, got %d", int64(tenant.MaxBudgetMicros), budget)
	}
	if amount < 0 || amount > tenant.MaxBudgetMicros {
		return Reservation{}, fmt.Errorf("budget: amount must be between 0 and %d µ$, got %d", int64(tenant.MaxBudgetMicros), amount)
	}
	return l.reserve(ctx, tenantID, budget, amount, at)
}

// Settle swaps r's reservation for charge, what its request really cost, in
// µ$: spent moves by charge - r.Amount. r must be admitted. It returns the
// month's spend in µ$ as the settlement left it, with known true, or known
// false when the hash is missing, as no spend is known then.
//
// If the hash was rebuilt since r was made, it doesn't hold the reservation,
// and the whole charge is added. If it's missing, Settle writes no hash: the
// next rebuild counts the request from its usage row, so the row must be
// queued before Settle runs. Settle bumps the hash's missed counter instead,
// so that a rebuild under way, which may have read the queue before the row
// was in it, starts its reads again.
//
// ctx should outlive the request, since a client that has left must still be
// settled: context.WithoutCancel gives one. The script gets 100 ms. A
// negative charge is an error and never reaches Redis.
func (l *Ledger) Settle(ctx context.Context, r Reservation, charge int64) (spent int64, known bool, err error) {
	if charge < 0 {
		return 0, false, fmt.Errorf("budget: charge must not be negative, got %d", charge)
	}
	ctx, cancel := context.WithTimeout(ctx, scriptTimeout)
	defer cancel()
	// r.Amount is between 0 and tenant.MaxBudgetMicros, so charge - r.Amount
	// can't overflow.
	keys := []string{r.key, missedKey(r.key)}
	spent, err = settleScript.Run(ctx, l.rdb, keys, r.gen, charge-r.Amount, charge, r.expireAt).Int64()
	if err != nil || spent == settleMissing {
		return 0, false, err
	}
	return spent, true, nil
}

// Spent returns the tenant's spend for the month holding at, in µ$: settled
// costs plus the reservations in flight. A missing hash is rebuilt, as by
// Reserve. Spent reserves nothing and never sets warned: it reserves 0 against
// a budget of -1, which is refused, as spent is never negative.
func (l *Ledger) Spent(ctx context.Context, tenantID string, at time.Time) (int64, error) {
	r, err := l.reserve(ctx, tenantID, -1, 0, at)
	return r.Spent, err
}

// reserve is Reserve without the argument checks, so Spent can pass a budget
// of -1.
func (l *Ledger) reserve(ctx context.Context, tenantID string, budget, amount int64, at time.Time) (Reservation, error) {
	start, end := Period(at)
	// The hash lasts until a day after the month ends, so a request that
	// arrives on its last day still settles into it.
	r := Reservation{Amount: amount, key: key(tenantID, start), expireAt: end.AddDate(0, 0, 1).Unix()}
	reply, err := l.runReserve(ctx, r.key, budget, amount)
	if err != nil {
		return Reservation{}, err
	}
	if reply[0] == replyMissing {
		if reply, err = l.rebuild(ctx, r, tenantID, start, end, budget); err != nil {
			return Reservation{}, err
		}
	}

	r.Spent = reply[1]
	if reply[0] != replyRefused {
		r.Admitted, r.Warn, r.gen = true, reply[0] == replyWarn, reply[2]
	}
	return r, nil
}

// runReserve runs reserve.lua on the hash k with args, for at most
// scriptTimeout. The reply is all integers, gen included. spent comes back as
// a number or as the string Redis stores, which Int64Slice parses alike.
func (l *Ledger) runReserve(ctx context.Context, k string, args ...any) ([]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, scriptTimeout)
	defer cancel()
	return reserveScript.Run(ctx, l.rdb, []string{k, missedKey(k)}, args...).Int64Slice()
}

// rebuild builds the missing hash r.key from what tally reads, and makes r's
// reservation against budget in it. It returns reserve.lua's reply.
//
// reserve.lua builds the hash only if no settle has found it missing since
// tally read the missed counter. Such a settle's usage row may have reached
// the queue after tally read it, and Postgres after the SUM, so tally reads
// again. The whole loop gets rebuildTimeout, and then fails.
func (l *Ledger) rebuild(ctx context.Context, r Reservation, tenantID string, start, end time.Time, budget int64) ([]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, rebuildTimeout)
	defer cancel()
	for {
		// DoChan is Do with the result sent on a channel, so a caller can
		// stop waiting when its own time runs out while the tally goes on
		// for the others.
		ch := l.rebuilds.DoChan(r.key, func() (any, error) {
			return l.tally(ctx, r.key, tenantID, start, end)
		})
		var res singleflight.Result
		select {
		case res = <-ch:
		case <-ctx.Done():
			res.Err = ctx.Err()
		}
		if res.Err != nil {
			return nil, fmt.Errorf("budget: rebuilding %s: %w", r.key, res.Err)
		}

		t := res.Val.(tallied)
		reply, err := l.runReserve(ctx, r.key, budget, r.Amount, t.spent, newGen(), r.expireAt, t.missed)
		if err != nil || reply[0] != replyRebuild {
			return reply, err
		}
	}
}

// tallied is what tally read.
type tallied struct {
	spent  int64 // what the missing hash starts from
	missed int64 // its missed counter, read first
}

// tally reads what the missing hash k starts from: the cost of the tenant's
// rows still queued, plus its rows in usage_logs for the month from start to
// end. It reads the hash's missed counter before either, for reserve.lua to
// check that no settle has found the hash missing since.
//
// The queue is read before Postgres. A batch that commits between the two
// reads is then counted twice, which only over-counts. Read the other way
// round, the batch would be in neither: committed after the SUM, and gone from
// the queue.
//
// Concurrent rebuilds of k share one tally (see rebuild). It has its own
// rebuildTimeout, under context.WithoutCancel, since the caller that started
// it may give up while others still wait on its result.
func (l *Ledger) tally(ctx context.Context, k, tenantID string, start, end time.Time) (tallied, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rebuildTimeout)
	defer cancel()
	missed, err := l.rdb.Get(ctx, missedKey(k)).Int64()
	if err != nil && !errors.Is(err, redis.Nil) { // a missing counter is 0
		return tallied{}, err
	}
	queued := l.queued(tenantID)
	var sum int64
	err = l.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_micros), 0) FROM usage_logs
		WHERE tenant_id = $1 AND created_at >= $2 AND created_at < $3`,
		tenantID, start, end,
	).Scan(&sum)
	return tallied{spent: queued + sum, missed: missed}, err
}

// newGen returns a gen for a new hash: 63 random bits, so a rebuilt hash
// never shares its predecessor's.
func newGen() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.BigEndian.Uint64(b[:]) >> 1)
}
