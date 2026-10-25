package budget

import (
	"context"
	"crypto/rand"
	"database/sql"
	"maps"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
	"github.com/grishkadel/llm-gateway/internal/redistest"
	"github.com/grishkadel/llm-gateway/internal/tenant"
)

// fixture is one test's Ledger, over the shared test Redis and a migrated
// database of the test's own, with a tenant no other test uses.
type fixture struct {
	*Ledger
	rdb      *redis.Client
	conn     *sql.DB
	tenantID string
	// at is when the test's requests arrive. The ledger never reads the
	// clock, so a test that runs across the end of a month still works in
	// at's.
	at time.Time
}

// newFixture returns a fixture whose rebuilds take queued as the cost of the
// tenant's queued rows; nil means none. Rebuilds call queued on a goroutine of
// their own, where a test may report with t.Error but not stop with t.Fatal.
func newFixture(t *testing.T, queued func(tenantID string) int64) *fixture {
	t.Helper()
	conn := dbtest.New(t)
	if _, err := db.Migrate(context.Background(), conn); err != nil {
		t.Fatalf("Migrate() returned error: %v", err)
	}
	if queued == nil {
		queued = func(string) int64 { return 0 }
	}
	rdb := redistest.New(t)
	f := &fixture{Ledger: New(rdb, conn, queued), rdb: rdb, conn: conn, at: time.Now()}
	f.tenantID = f.addTenant(t)
	return f
}

// newTenant returns a tenant ID no other test uses. Its hash for the month
// holding at, and the hash's missed counter, are deleted when the test ends.
func newTenant(t *testing.T, rdb *redis.Client, at time.Time) string {
	t.Helper()
	tenantID := "tn_test_" + rand.Text()
	start, _ := Period(at)
	k := key(tenantID, start)
	t.Cleanup(func() { rdb.Del(context.Background(), k, missedKey(k)) })
	return tenantID
}

// addTenant adds a new tenant to the database, with an API key for its usage
// rows to name.
func (f *fixture) addTenant(t *testing.T) string {
	t.Helper()
	tenantID := newTenant(t, f.rdb, f.at)
	if _, err := f.conn.Exec(`INSERT INTO tenants (id, name) VALUES ($1, 'test')`, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conn.Exec(`INSERT INTO api_keys (tenant_id, key_hash, key_prefix) VALUES ($1, $2, 'gw_test')`,
		tenantID, []byte(tenantID)); err != nil {
		t.Fatal(err)
	}
	return tenantID
}

// addRow inserts a usage row for tenantID that arrived at created and cost
// cost µ$: an int64, or nil for NULL. It reports a failure with t.Error, so a
// queued func may call it.
func (f *fixture) addRow(t *testing.T, tenantID string, cost any, created time.Time) {
	t.Helper()
	_, err := f.conn.Exec(`
		INSERT INTO usage_logs (request_id, tenant_id, api_key_id, provider, model, endpoint,
			status, input_tokens, output_tokens, cost_micros, latency_ms, created_at)
		SELECT 'req_test', tenant_id, id, 'openai', 'gpt-5.5', '/v1/chat/completions',
			200, 0, 0, $2::bigint, 0, $3::timestamptz
		FROM api_keys WHERE tenant_id = $1`,
		tenantID, cost, created)
	if err != nil {
		t.Errorf("adding a usage row: %v", err)
	}
}

// hashKey names the tenant's hash for at's month.
func (f *fixture) hashKey() string {
	start, _ := Period(f.at)
	return key(f.tenantID, start)
}

// hash returns the tenant's hash for at's month, as Redis holds it.
func (f *fixture) hash(t *testing.T) map[string]string {
	t.Helper()
	h, err := f.rdb.HGetAll(context.Background(), f.hashKey()).Result()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// wantSpent checks the hash's spent, as Redis stores it.
func (f *fixture) wantSpent(t *testing.T, want int64) {
	t.Helper()
	if got := f.hash(t)["spent"]; got != strconv.FormatInt(want, 10) {
		t.Errorf("spent = %q, want %d", got, want)
	}
}

// wantMissed checks the hash's missed counter, as Redis stores it.
func (f *fixture) wantMissed(t *testing.T, want string) {
	t.Helper()
	if got := f.rdb.Get(context.Background(), missedKey(f.hashKey())).Val(); got != want {
		t.Errorf("missed counter = %q, want %q", got, want)
	}
}

// wantExpiry checks that k expires a day after at's month ends, so that a
// request that arrives on the month's last day still settles into the month.
func (f *fixture) wantExpiry(t *testing.T, k string) {
	t.Helper()
	got, err := f.rdb.ExpireTime(context.Background(), k).Result()
	if err != nil {
		t.Fatal(err)
	}
	_, end := Period(f.at)
	if want := end.AddDate(0, 0, 1); got != time.Duration(want.Unix())*time.Second {
		t.Errorf("%s expires at %v, want %v", k, time.Unix(int64(got/time.Second), 0).UTC(), want)
	}
}

// deleteHash deletes the tenant's hash, as a Redis restart would.
func (f *fixture) deleteHash(t *testing.T) {
	t.Helper()
	if err := f.rdb.Del(context.Background(), f.hashKey()).Err(); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) mustReserve(t *testing.T, budget, amount int64) Reservation {
	t.Helper()
	r, err := f.Reserve(context.Background(), f.tenantID, budget, amount, f.at)
	if err != nil || !r.Admitted {
		t.Fatalf("Reserve(budget %d, amount %d) = %+v, %v; want admitted", budget, amount, r, err)
	}
	return r
}

func (f *fixture) mustRefuse(t *testing.T, budget, amount int64) Reservation {
	t.Helper()
	r, err := f.Reserve(context.Background(), f.tenantID, budget, amount, f.at)
	if err != nil || r.Admitted {
		t.Fatalf("Reserve(budget %d, amount %d) = %+v, %v; want refused", budget, amount, r, err)
	}
	return r
}

func (f *fixture) mustSettle(t *testing.T, r Reservation, charge int64) {
	t.Helper()
	if err := f.Settle(context.Background(), r, charge); err != nil {
		t.Fatalf("Settle(charge %d) returned error: %v", charge, err)
	}
}

func TestPeriodIsTheUTCMonth(t *testing.T) {
	for _, tc := range []struct{ at, start, end string }{
		{"2026-10-15T12:00:00Z", "2026-10-01T00:00:00Z", "2026-11-01T00:00:00Z"},
		{"2026-10-01T00:00:00Z", "2026-10-01T00:00:00Z", "2026-11-01T00:00:00Z"},
		{"2026-12-31T23:59:59.999999999Z", "2026-12-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		// Still October in New York, but November in UTC.
		{"2026-10-31T22:30:00-04:00", "2026-11-01T00:00:00Z", "2026-12-01T00:00:00Z"},
		{"2028-02-29T12:00:00Z", "2028-02-01T00:00:00Z", "2028-03-01T00:00:00Z"},
	} {
		at, err := time.Parse(time.RFC3339Nano, tc.at)
		if err != nil {
			t.Fatal(err)
		}
		// Formatted, a time outside UTC would show its offset.
		start, end := Period(at)
		if got, gotEnd := start.Format(time.RFC3339), end.Format(time.RFC3339); got != tc.start || gotEnd != tc.end {
			t.Errorf("Period(%s) = %s, %s; want %s, %s", tc.at, got, gotEnd, tc.start, tc.end)
		}
	}
}

func TestKeyNamesTheTenantAndMonth(t *testing.T) {
	start, _ := Period(time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC))
	k := key("tn_1a2b", start)
	if want := "budget:{tn_1a2b}:2026-10"; k != want {
		t.Errorf("key = %q, want %q", k, want)
	}
	if got, want := missedKey(k), "budget:{tn_1a2b}:2026-10:missed"; got != want {
		t.Errorf("missedKey = %q, want %q", got, want)
	}
}

// TestBadArgumentsNeverReachRedis uses a Ledger with no client: if a call
// reached Redis, the nil client would panic.
func TestBadArgumentsNeverReachRedis(t *testing.T) {
	ctx := context.Background()
	l := New(nil, nil, nil)
	for _, tc := range []struct{ budget, amount int64 }{
		{-1, 1}, {tenant.MaxBudgetMicros + 1, 1}, {1_000, -1}, {1_000, tenant.MaxBudgetMicros + 1},
	} {
		if r, err := l.Reserve(ctx, "tn_test", tc.budget, tc.amount, time.Now()); err == nil || r.Admitted {
			t.Errorf("Reserve(budget %d, amount %d) = %+v, %v; want an error", tc.budget, tc.amount, r, err)
		}
	}
	if err := l.Settle(ctx, Reservation{Admitted: true}, -1); err == nil {
		t.Error("Settle(charge -1): want an error")
	}
}

// TestReserveAdmitsUpToTheBudget: a reservation that fills the budget exactly
// is admitted, and one a micro-dollar over is refused. The refusal reports
// spent and leaves the hash as it was, expiry included.
func TestReserveAdmitsUpToTheBudget(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	if r := f.mustReserve(t, 1_000, 400); r.Spent != 400 || r.Amount != 400 {
		t.Errorf("after the first reservation: %+v, want spent 400 and amount 400", r)
	}
	if r := f.mustReserve(t, 1_000, 600); r.Spent != 1_000 {
		t.Errorf("after the second reservation: spent = %d, want 1000", r.Spent)
	}

	fields := f.hash(t)
	expiry := f.rdb.ExpireTime(ctx, f.hashKey()).Val()
	if r := f.mustRefuse(t, 1_000, 1); r.Spent != 1_000 {
		t.Errorf("refused with spent %d, want 1000", r.Spent)
	}
	if got := f.hash(t); !maps.Equal(got, fields) {
		t.Errorf("hash after the refusal = %v, want it unchanged, %v", got, fields)
	}
	if got := f.rdb.ExpireTime(ctx, f.hashKey()).Val(); got != expiry {
		t.Errorf("hash expires at %v after the refusal, want it unchanged, %v (since the Unix epoch)", got, expiry)
	}
}

// TestConcurrentReservationsCannotOverAdmit: 50 reservations of 1,000 at once
// against a budget with room for 10. The hash starts missing, so they also
// race to rebuild it.
func TestConcurrentReservationsCannotOverAdmit(t *testing.T) {
	f := newFixture(t, nil)
	results := make([]Reservation, 50)
	errs := make([]error, 50)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			<-start
			results[i], errs[i] = f.Reserve(context.Background(), f.tenantID, 10_000, 1_000, f.at)
		})
	}
	close(start)
	wg.Wait()

	admitted := 0
	for i, r := range results {
		switch {
		case errs[i] != nil:
			t.Fatal(errs[i])
		case r.Admitted:
			admitted++
		case r.Spent != 10_000:
			t.Errorf("refused with spent %d, want 10000: only a full budget refuses 1,000", r.Spent)
		}
	}
	if admitted != 10 {
		t.Errorf("admitted %d of 50 reservations of 1,000 against a budget of 10,000, want exactly 10", admitted)
	}
	f.wantSpent(t, 10_000)
}

func TestZeroBudgetRefusesAnyCost(t *testing.T) {
	f := newFixture(t, nil)
	for _, amount := range []int64{1, tenant.MaxBudgetMicros} {
		if r := f.mustRefuse(t, 0, amount); r.Spent != 0 {
			t.Errorf("refused %d with spent %d, want 0", amount, r.Spent)
		}
	}
}

// TestExactAtTheTopOfTheRange: at 1e15, a double still tells apart integers
// one apart, so the scripts admit and refuse to the micro-dollar.
func TestExactAtTheTopOfTheRange(t *testing.T) {
	f := newFixture(t, nil)
	f.mustReserve(t, tenant.MaxBudgetMicros, tenant.MaxBudgetMicros-1)
	if r := f.mustRefuse(t, tenant.MaxBudgetMicros, 2); r.Spent != tenant.MaxBudgetMicros-1 {
		t.Errorf("refused with spent %d, want %d", r.Spent, int64(tenant.MaxBudgetMicros-1))
	}
	f.mustReserve(t, tenant.MaxBudgetMicros, 1)
	f.wantSpent(t, tenant.MaxBudgetMicros)

	// Settlements and rebuilds have no cap, so spent can pass 2^53, where
	// doubles skip odd integers. A refusal still reports it exactly.
	if err := f.rdb.HSet(context.Background(), f.hashKey(), "spent", "9007199254740993").Err(); err != nil {
		t.Fatal(err)
	}
	if r := f.mustRefuse(t, tenant.MaxBudgetMicros, 0); r.Spent != 1<<53+1 {
		t.Errorf("refused with spent %d, want 2^53 + 1", r.Spent)
	}
}

// TestRebuildCountsTheMonthsRowsAndTheQueue: a missing hash starts from the
// tenant's rows for the month, both ends included, plus its queued cost.
func TestRebuildCountsTheMonthsRowsAndTheQueue(t *testing.T) {
	var f *fixture
	f = newFixture(t, func(tenantID string) int64 {
		if tenantID != f.tenantID {
			t.Errorf("queued(%q), want the tenant rebuilt, %q", tenantID, f.tenantID)
		}
		return 7
	})
	start, end := Period(f.at)
	other := f.addTenant(t)
	for _, row := range []struct {
		tenantID string
		cost     any
		created  time.Time
	}{
		{f.tenantID, int64(100), start},
		{f.tenantID, int64(20), f.at},
		{f.tenantID, nil, f.at}, // unpriced: counts as 0
		{f.tenantID, int64(3), end.Add(-time.Microsecond)},
		{f.tenantID, int64(1_000), start.Add(-time.Microsecond)}, // the month before
		{f.tenantID, int64(10_000), end},                         // the month after
		{other, int64(100_000), f.at},
	} {
		f.addRow(t, row.tenantID, row.cost, row.created)
	}

	spent, err := f.Spent(context.Background(), f.tenantID, f.at)
	if err != nil || spent != 130 {
		t.Errorf("Spent() = %d, %v; want 130: 100 + 20 + 3 in the month, plus 7 queued", spent, err)
	}
	f.wantSpent(t, 130)
}

// TestRebuiltHashExpiresADayAfterTheMonth: a request that arrives on the
// month's last day still settles into the month's hash.
func TestRebuiltHashExpiresADayAfterTheMonth(t *testing.T) {
	f := newFixture(t, nil)
	f.mustReserve(t, 1_000, 10)
	f.wantExpiry(t, f.hashKey())
}

// TestRebuildReadsTheQueueFirst: a batch commits as the rebuild reads the
// queue, so its row is gone from the queue and only the SUM can count it.
// Reading the queue after the SUM would miss it.
func TestRebuildReadsTheQueueFirst(t *testing.T) {
	var f *fixture
	f = newFixture(t, func(string) int64 {
		f.addRow(t, f.tenantID, int64(500), f.at)
		return 0
	})
	if r := f.mustReserve(t, 1_000, 100); r.Spent != 600 {
		t.Errorf("spent = %d, want 600: the committed row's 500, plus the reservation", r.Spent)
	}
}

// TestConcurrentRebuildsShareOneQuery: two reservations find the hash missing
// at once and rebuild it together. Both count in the one hash.
func TestConcurrentRebuildsShareOneQuery(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	f := newFixture(t, func(string) int64 {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return 100
	})

	var rs [2]Reservation
	var errs [2]error
	var wg sync.WaitGroup
	reserve := func(i int) {
		rs[i], errs[i] = f.Reserve(context.Background(), f.tenantID, 1_000, 10, f.at)
	}
	wg.Go(func() { reserve(0) })
	<-entered // the first is rebuilding
	wg.Go(func() { reserve(1) })
	// Ample time for the second to find the hash missing, which takes one
	// script, and join the rebuild.
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil || !rs[i].Admitted {
			t.Fatalf("reservation %d = %+v, %v; want admitted", i, rs[i], err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("rebuilt %d times, want once, shared", n)
	}
	if got := []int64{min(rs[0].Spent, rs[1].Spent), max(rs[0].Spent, rs[1].Spent)}; got[0] != 110 || got[1] != 120 {
		t.Errorf("reservations saw spent %v, want 110 and 120", got)
	}
	f.wantSpent(t, 120)
}

// TestTwoLedgersAgreeOnOneHash: two gateway replicas, each with its own queue,
// rebuild the same missing hash at once and get different totals. The hash
// takes whichever is written first, and both reservations count in it.
func TestTwoLedgersAgreeOnOneHash(t *testing.T) {
	// Each rebuild waits in queued until both are rebuilding, so both find
	// the hash missing.
	var both sync.WaitGroup
	both.Add(2)
	queued := func(total int64) func(string) int64 {
		return func(string) int64 {
			both.Done()
			both.Wait()
			return total
		}
	}
	f := newFixture(t, queued(100))
	ledgers := []*Ledger{f.Ledger, New(f.rdb, f.conn, queued(200))}

	var rs [2]Reservation
	var errs [2]error
	var wg sync.WaitGroup
	for i, l := range ledgers {
		wg.Go(func() { rs[i], errs[i] = l.Reserve(context.Background(), f.tenantID, 1_000, 10, f.at) })
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil || !rs[i].Admitted {
			t.Fatalf("reservation %d = %+v, %v; want admitted", i, rs[i], err)
		}
	}
	spent, err := strconv.ParseInt(f.hash(t)["spent"], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if spent != 120 && spent != 220 {
		t.Fatalf("spent = %d, want 120 or 220: one rebuild's total, plus both reservations", spent)
	}
	if lo, hi := min(rs[0].Spent, rs[1].Spent), max(rs[0].Spent, rs[1].Spent); lo != spent-10 || hi != spent {
		t.Errorf("reservations saw spent %d and %d, want %d and %d", lo, hi, spent-10, spent)
	}
	if rs[0].gen != rs[1].gen {
		t.Errorf("reservations hold gens %d and %d, want the one hash's", rs[0].gen, rs[1].gen)
	}
}

// TestRebuildOutlivesItsCaller: the caller that starts a rebuild's reads may
// give up, as when its client leaves, while others wait on the same reads. So
// the reads run on.
func TestRebuildOutlivesItsCaller(t *testing.T) {
	f := newFixture(t, func(string) int64 { return 7 })
	f.addRow(t, f.tenantID, int64(100), f.at)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start, end := Period(f.at)
	if got, err := f.tally(ctx, f.hashKey(), f.tenantID, start, end); err != nil || got != (tallied{spent: 107}) {
		t.Errorf("tally() with its caller gone = %+v, %v; want spent 107", got, err)
	}
}

// TestSettleDuringARebuild: a request that reserved in the lost hash ends
// while the hash is rebuilt, just after the rebuild has read the queue. Its
// usage row reached the queue too late for that read, and Postgres too late
// for the SUM, and its settle found no hash to add to. The rebuild must see
// the settle in the missed counter and read again, so the request counts,
// once. Reserve and Spent take the same path.
func TestSettleDuringARebuild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spent func(f *fixture) (int64, error)
		want  int64
	}{
		{"Reserve", func(f *fixture) (int64, error) {
			r, err := f.Reserve(context.Background(), f.tenantID, 1_000, 50, f.at)
			return r.Spent, err
		}, 170}, // the request's 120, plus this reservation's 50
		{"Spent", func(f *fixture) (int64, error) {
			return f.Spent(context.Background(), f.tenantID, f.at)
		}, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *fixture
			var old Reservation
			var queue atomic.Int64 // the cost the usage writer holds
			var armed atomic.Bool
			var reads atomic.Int32
			f = newFixture(t, func(string) int64 {
				reads.Add(1)
				q := queue.Load()
				if armed.CompareAndSwap(true, false) {
					// Just after this read, the old request ends: its row is
					// queued, and it settles against the missing hash.
					queue.Store(120)
					if err := f.Settle(context.Background(), old, 120); err != nil {
						t.Errorf("Settle() returned error: %v", err)
					}
				}
				return q
			})
			old = f.mustReserve(t, 1_000, 300)
			f.deleteHash(t)
			reads.Store(0)
			armed.Store(true)

			if spent, err := tc.spent(f); err != nil || spent != tc.want {
				t.Errorf("%s() = %d, %v; want %d", tc.name, spent, err, tc.want)
			}
			if n := reads.Load(); n != 2 {
				t.Errorf("read the queue %d times, want twice: again after the settle", n)
			}
			f.wantSpent(t, tc.want)
			f.wantMissed(t, "1")
		})
	}
}

// TestRebuildGivesUpAtTheTimeLimit: a settle finds the hash missing during
// every rebuild, so no rebuild can build it. Reserve and Spent each fail once
// rebuildTimeout has passed, having built nothing.
func TestRebuildGivesUpAtTheTimeLimit(t *testing.T) {
	var f *fixture
	var old Reservation
	var armed atomic.Bool
	f = newFixture(t, func(string) int64 {
		// The last of these reads can still be running when the test ends,
		// so it leaves t alone. A settle that failed would let a rebuild
		// through, which the test reports.
		if armed.Load() {
			_ = f.Settle(context.Background(), old, 0)
		}
		return 0
	})
	old = f.mustReserve(t, 1_000, 300)
	f.deleteHash(t)
	armed.Store(true)
	defer armed.Store(false)

	ctx := context.Background()
	for name, call := range map[string]func() error{
		"Reserve": func() error { _, err := f.Reserve(ctx, f.tenantID, 1_000, 10, f.at); return err },
		"Spent":   func() error { _, err := f.Spent(ctx, f.tenantID, f.at); return err },
	} {
		begin := time.Now()
		err := call()
		if elapsed := time.Since(begin); err == nil || elapsed < rebuildTimeout || elapsed > rebuildTimeout+time.Second {
			t.Errorf("%s() while settles keep finding the hash missing = %v after %v; want an error after about %v", name, err, elapsed, rebuildTimeout)
		}
	}
	if n := f.rdb.Exists(ctx, f.hashKey()).Val(); n != 0 {
		t.Errorf("built the hash: %v", f.hash(t))
	}
}

// TestPostgresErrorIsNeverAnAdmission: a rebuild that fails builds no hash,
// and admits nothing.
func TestPostgresErrorIsNeverAnAdmission(t *testing.T) {
	ctx := context.Background()
	rdb := redistest.New(t)
	at := time.Now()
	tenantID := newTenant(t, rdb, at)
	// A database without the schema: the SUM fails.
	l := New(rdb, dbtest.New(t), func(string) int64 { return 0 })

	if r, err := l.Reserve(ctx, tenantID, 1_000, 10, at); err == nil || r.Admitted {
		t.Errorf("Reserve() = %+v, %v; want an error", r, err)
	}
	if _, err := l.Spent(ctx, tenantID, at); err == nil {
		t.Error("Spent(): want an error")
	}
	start, _ := Period(at)
	if n := rdb.Exists(ctx, key(tenantID, start)).Val(); n != 0 {
		t.Error("a failed rebuild built the hash")
	}
}

// closedAddr returns an address that refuses connections.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	return ln.Addr().String()
}

// hungAddr returns an address that takes connections and never answers, like
// a Redis or Postgres that hangs. Nothing accepts them, but the kernel
// completes them all the same.
func hungAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// redisAt returns a client for a Redis at addr, set up as the gateway's is.
func redisAt(t *testing.T, addr string) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

// TestRedisErrorIsNeverAnAdmission: every call fails while Redis is down.
func TestRedisErrorIsNeverAnAdmission(t *testing.T) {
	ctx := context.Background()
	l := New(redisAt(t, closedAddr(t)), nil, nil)
	at := time.Now()
	start, _ := Period(at)

	if r, err := l.Reserve(ctx, "tn_test", 1_000, 10, at); err == nil || r.Admitted {
		t.Errorf("Reserve() = %+v, %v; want an error", r, err)
	}
	if _, err := l.Spent(ctx, "tn_test", at); err == nil {
		t.Error("Spent(): want an error")
	}
	r := Reservation{Admitted: true, Amount: 10, key: key("tn_test", start), gen: 1}
	if err := l.Settle(ctx, r, 5); err == nil {
		t.Error("Settle(): want an error")
	}
}

// TestHungRedisTimesOut: the ledger bounds each script itself, so a Redis that
// never answers costs a call 100 ms, not the client's own timeouts of
// seconds.
func TestHungRedisTimesOut(t *testing.T) {
	ctx := context.Background()
	l := New(redisAt(t, hungAddr(t)), nil, nil)
	at := time.Now()
	start, _ := Period(at)
	r := Reservation{Admitted: true, Amount: 10, key: key("tn_test", start), gen: 1}

	for name, call := range map[string]func() error{
		"Reserve": func() error { _, err := l.Reserve(ctx, "tn_test", 1_000, 10, at); return err },
		"Spent":   func() error { _, err := l.Spent(ctx, "tn_test", at); return err },
		"Settle":  func() error { return l.Settle(ctx, r, 5) },
	} {
		begin := time.Now()
		err := call()
		if elapsed := time.Since(begin); err == nil || elapsed < scriptTimeout || elapsed > time.Second {
			t.Errorf("%s() on a Redis that never answers = %v after %v; want an error after about %v", name, err, elapsed, scriptTimeout)
		}
	}
}

// TestHungPostgresTimesOut: a rebuild that Postgres never answers fails after
// 2 s, and builds no hash.
func TestHungPostgresTimesOut(t *testing.T) {
	ctx := context.Background()
	rdb := redistest.New(t)
	at := time.Now()
	tenantID := newTenant(t, rdb, at)
	conn, err := sql.Open("pgx", "postgres://gateway:gateway@"+hungAddr(t)+"/gateway?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	l := New(rdb, conn, func(string) int64 { return 0 })

	begin := time.Now()
	r, err := l.Reserve(ctx, tenantID, 1_000, 10, at)
	if elapsed := time.Since(begin); err == nil || r.Admitted || elapsed < rebuildTimeout || elapsed > rebuildTimeout+time.Second {
		t.Errorf("Reserve() on a Postgres that never answers = %+v, %v after %v; want an error after about %v", r, err, elapsed, rebuildTimeout)
	}
	start, _ := Period(at)
	if n := rdb.Exists(ctx, key(tenantID, start)).Val(); n != 0 {
		t.Error("a failed rebuild built the hash")
	}
}

// TestSettleSwapsTheReservationForTheCharge: a request that cost less than it
// reserved gives the rest back, and one that cost more adds the difference.
func TestSettleSwapsTheReservationForTheCharge(t *testing.T) {
	f := newFixture(t, nil)
	f.mustSettle(t, f.mustReserve(t, 1_000, 300), 120)
	f.wantSpent(t, 120)
	f.mustSettle(t, f.mustReserve(t, 1_000, 300), 500)
	f.wantSpent(t, 620)
}

// TestSettleOnAMissingHashBuildsNoHash: built by Settle, the hash would hold
// one request and none of the month before it. Settle counts itself in the
// hash's missed counter instead, which expires with the hash it stands for.
func TestSettleOnAMissingHashBuildsNoHash(t *testing.T) {
	f := newFixture(t, nil)
	r := f.mustReserve(t, 1_000, 300)
	f.deleteHash(t)
	f.mustSettle(t, r, 120)
	f.wantMissed(t, "1")
	f.mustSettle(t, r, 120)
	f.wantMissed(t, "2")
	if n := f.rdb.Exists(context.Background(), f.hashKey()).Val(); n != 0 {
		t.Errorf("Settle built the missing hash: %v", f.hash(t))
	}
	f.wantExpiry(t, missedKey(f.hashKey()))
}

// TestSettleAfterARebuildAddsTheWholeCharge: the hash is lost and rebuilt
// while a request is in flight. The new hash doesn't hold that request's
// reservation, so its whole cost is added.
func TestSettleAfterARebuildAddsTheWholeCharge(t *testing.T) {
	f := newFixture(t, nil)
	r := f.mustReserve(t, 1_000, 300)
	f.deleteHash(t)
	if again := f.mustReserve(t, 1_000, 50); again.gen == r.gen {
		t.Fatalf("the rebuilt hash kept gen %d", r.gen)
	}
	f.mustSettle(t, r, 120)
	f.wantSpent(t, 170)
}

func TestWarnsOnceAt80Percent(t *testing.T) {
	f := newFixture(t, nil)
	for _, step := range []struct {
		amount int64
		warn   bool
	}{
		{799, false}, // 79.9%
		{1, true},    // 80.0%
		{1, false},
		{199, false}, // 100%
	} {
		if r := f.mustReserve(t, 1_000, step.amount); r.Warn != step.warn {
			t.Errorf("reserving %d took spent to %d: Warn = %v, want %v", step.amount, r.Spent, r.Warn, step.warn)
		}
	}
}

// TestSpentReservesNothingAndNeverWarns: Spent rebuilds a missing hash, here
// one already at 90%, without reserving or setting warned. So the next
// reservation still reports the 80%.
func TestSpentReservesNothingAndNeverWarns(t *testing.T) {
	f := newFixture(t, func(string) int64 { return 900 })
	for range 2 {
		if spent, err := f.Spent(context.Background(), f.tenantID, f.at); err != nil || spent != 900 {
			t.Errorf("Spent() = %d, %v; want 900", spent, err)
		}
	}
	h := f.hash(t)
	if _, warned := h["warned"]; warned || h["spent"] != "900" {
		t.Errorf("hash after Spent = %v, want spent 900 and no warned", h)
	}
	if r := f.mustReserve(t, 1_000, 10); !r.Warn {
		t.Error("the first reservation after Spent, at 91%, didn't warn")
	}
}

// TestRebuiltHashWarnsAgain: warned lives in the hash, so a rebuilt one
// reports the 80% again.
func TestRebuiltHashWarnsAgain(t *testing.T) {
	var queued atomic.Int64
	f := newFixture(t, func(string) int64 { return queued.Load() })
	if r := f.mustReserve(t, 1_000, 800); !r.Warn {
		t.Fatal("the first reservation to 80% didn't warn")
	}
	f.deleteHash(t)
	queued.Store(800) // that request's usage row, still queued
	if r := f.mustReserve(t, 1_000, 1); !r.Warn || r.Spent != 801 {
		t.Errorf("first reservation in the rebuilt hash = %+v, want spent 801 and Warn", r)
	}
}
