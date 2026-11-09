package usage

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
	"github.com/grishkadel/llm-gateway/internal/tenant"
)

// newWriterDB returns a migrated database holding one tenant and key, which
// every usage row must reference.
func newWriterDB(t *testing.T) (*sql.DB, tenant.Tenant, tenant.NewKey) {
	t.Helper()

	conn := dbtest.New(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatalf("Migrate() returned error: %v", err)
	}
	tn, key, err := tenant.NewStore(conn).Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	return conn, tn, key
}

func testRow(tn tenant.Tenant, key tenant.NewKey, requestID string) Row {
	cost := int64(75)
	return Row{
		RequestID: requestID, ProviderRequestID: "req_provider",
		TenantID: tn.ID, APIKeyID: key.ID,
		Provider: "gemini", Model: "gemini-3.8-flash", Endpoint: "/v1beta/models/gemini-3.8-flash:generateContent",
		Status: 200, Usage: Usage{Input: 15, CachedInput: 5, CacheWrite: 2, CacheWrite1h: 1, Output: 17},
		CostMicros: &cost, Streamed: true, LatencyMS: 42,
		CreatedAt: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
	}
}

// withCost returns r priced at micros.
func withCost(r Row, micros int64) Row {
	r.CostMicros = &micros
	return r
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func countRows(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM usage_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitForRows polls until the table holds want rows, failing after a
// deadline: the flush happens on the writer's goroutine.
func waitForRows(t *testing.T, conn *sql.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := countRows(t, conn)
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage_logs has %d rows, want %d", n, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForPending polls until w counts want micro-dollars of tenantID's rows as
// pending, failing after a deadline: the writer's goroutine takes a batch off
// the count just after it commits.
func waitForPending(t *testing.T, w *Writer, tenantID string, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := w.Pending(tenantID)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Pending(%s) = %d, want %d", tenantID, got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForBlockedInsert polls until a statement in the test database is waiting
// on a lock, failing after a deadline. The only one that can is the writer's
// INSERT, held up by a lock the test took on usage_logs.
func waitForBlockedInsert(t *testing.T, conn *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting int
		err := conn.QueryRow(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the writer's INSERT never waited on the table lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWriterFlushesFullBatch(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	// An interval far longer than the test: only the batch size can flush.
	w := newWriter(conn, discard(), 1000, 100, time.Hour)
	w.start()
	defer w.Close(context.Background())

	for i := range 100 {
		w.Write(testRow(tn, key, "req_"+strconv.Itoa(i)))
	}
	waitForRows(t, conn, 100)
}

func TestWriterFlushesOnInterval(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	w := newWriter(conn, discard(), 1000, 100, 50*time.Millisecond)
	w.start()
	defer w.Close(context.Background())

	for i := range 3 {
		w.Write(testRow(tn, key, "req_"+strconv.Itoa(i)))
	}
	waitForRows(t, conn, 3)
}

// TestWriterDrainsOnClose: rows still queued at shutdown are written before
// Close returns, not lost.
func TestWriterDrainsOnClose(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	w := newWriter(conn, discard(), 1000, 100, time.Hour)
	w.start()

	for i := range 5 {
		w.Write(testRow(tn, key, "req_"+strconv.Itoa(i)))
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}
	if n := countRows(t, conn); n != 5 {
		t.Errorf("after Close, usage_logs has %d rows, want 5", n)
	}
}

// TestWriterNeverBlocks: a full queue drops the row and counts it, rather
// than holding up the response that produced it. So does a write after Close.
func TestWriterNeverBlocks(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	// Not started: nothing drains the one-slot queue.
	w := newWriter(conn, discard(), 1, 100, time.Hour)

	done := make(chan struct{})
	go func() {
		w.Write(testRow(tn, key, "req_1"))
		w.Write(testRow(tn, key, "req_2"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on a full queue")
	}
	if got := w.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1", got)
	}

	w.start()
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.Write(testRow(tn, key, "req_3"))
	if got := w.Dropped(); got != 2 {
		t.Errorf("after a write past Close, Dropped() = %d, want 2", got)
	}
	if n := countRows(t, conn); n != 1 {
		t.Errorf("usage_logs has %d rows, want 1", n)
	}
}

// TestWriterQueued: Queued is how many rows wait in the queue. A lock on the
// table holds the insert up, so the batch the writer is inserting has left the
// queue, the rows after it fill the queue, and one that finds it full is
// dropped without changing it. Close drains it.
func TestWriterQueued(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	// Two rows fill the batch, so the writer flushes at once. The queue holds
	// three.
	w := newWriter(conn, discard(), 3, 2, time.Hour)
	w.start()
	defer w.Close(context.Background())

	lock, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err := lock.Exec(`LOCK TABLE usage_logs IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	w.Write(testRow(tn, key, "req_1"))
	w.Write(testRow(tn, key, "req_2"))
	waitForBlockedInsert(t, conn)
	if got := w.Queued(); got != 0 {
		t.Errorf("with the first batch in its insert, Queued() = %d, want 0", got)
	}

	for i := 3; i <= 5; i++ {
		w.Write(testRow(tn, key, "req_"+strconv.Itoa(i)))
	}
	if got := w.Queued(); got != 3 {
		t.Errorf("with the queue full, Queued() = %d, want 3", got)
	}

	w.Write(testRow(tn, key, "req_6")) // the queue is full
	if queued, dropped := w.Queued(), w.Dropped(); queued != 3 || dropped != 1 {
		t.Errorf("after a row found the queue full, Queued() = %d and Dropped() = %d, want 3 and 1", queued, dropped)
	}

	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.Queued(); got != 0 {
		t.Errorf("after Close, Queued() = %d, want 0", got)
	}
	if n := countRows(t, conn); n != 5 {
		t.Errorf("usage_logs has %d rows, want the 5 that were queued", n)
	}
}

// TestWriterStoresEveryField reads a row back, NULLs included.
func TestWriterStoresEveryField(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	w := newWriter(conn, discard(), 10, 100, time.Hour)
	w.start()

	priced := testRow(tn, key, "req_priced")
	priced.UsageComplete = true
	unpriced := testRow(tn, key, "req_unpriced")
	unpriced.CostMicros = nil
	unpriced.ProviderRequestID = ""
	w.Write(priced)
	w.Write(unpriced)
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	type stored struct {
		providerRequestID                         sql.NullString
		tenantID, provider, model, endpoint       string
		keyID                                     int64
		status, input, cached, cacheWrite, output int
		cost                                      sql.NullInt64
		usageComplete, isCached, streamed         bool
		latency                                   int
		createdAt                                 time.Time
	}
	read := func(requestID string) stored {
		var s stored
		err := conn.QueryRow(`
			SELECT provider_request_id, tenant_id, api_key_id, provider, model, endpoint, status,
			       input_tokens, cached_input_tokens, cache_write_tokens, output_tokens,
			       cost_micros, usage_complete, cached, streamed, latency_ms, created_at
			FROM usage_logs WHERE request_id = $1`, requestID,
		).Scan(&s.providerRequestID, &s.tenantID, &s.keyID, &s.provider, &s.model, &s.endpoint, &s.status,
			&s.input, &s.cached, &s.cacheWrite, &s.output, &s.cost, &s.usageComplete, &s.isCached, &s.streamed, &s.latency, &s.createdAt)
		if err != nil {
			t.Fatalf("reading %s: %v", requestID, err)
		}
		return s
	}

	p := read("req_priced")
	if p.providerRequestID.String != "req_provider" || p.tenantID != tn.ID || p.keyID != key.ID ||
		p.provider != "gemini" || p.model != "gemini-3.8-flash" || p.status != 200 || !p.streamed || p.isCached || p.latency != 42 {
		t.Errorf("row = %+v, want the fields written", p)
	}
	// Both cache-write TTLs go in the one column; pricing already used the
	// split.
	if p.input != 15 || p.cached != 5 || p.cacheWrite != 3 || p.output != 17 || !p.cost.Valid || p.cost.Int64 != 75 || !p.usageComplete {
		t.Errorf("tokens, cost and usage_complete = %+v", p)
	}
	if !p.createdAt.Equal(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("created_at = %v, want the request's time, not the flush time", p.createdAt)
	}

	u := read("req_unpriced")
	if u.cost.Valid || u.providerRequestID.Valid || u.usageComplete {
		t.Errorf("cost = %v, provider_request_id = %v, usage_complete = %v; want both NULL, and false", u.cost, u.providerRequestID, u.usageComplete)
	}
}

// TestWriterStoresUnsafeText: model and endpoint come from the client and
// provider headers from upstream. A NUL byte or invalid UTF-8 there must not
// make Postgres reject the row, let alone the batch around it.
func TestWriterStoresUnsafeText(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	w := newWriter(conn, discard(), 10, 100, time.Hour)
	w.start()

	good := testRow(tn, key, "req_good")
	bad := testRow(tn, key, "req_bad")
	bad.Model = "gpt\x00-4o"
	bad.Endpoint = "/v1beta/models/\xff:generateContent"
	bad.ProviderRequestID = "id\x00"
	w.Write(good)
	w.Write(bad)
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	if n := countRows(t, conn); n != 2 {
		t.Fatalf("usage_logs has %d rows, want both", n)
	}
	var model, endpoint string
	if err := conn.QueryRow(`SELECT model, endpoint FROM usage_logs WHERE request_id = 'req_bad'`).Scan(&model, &endpoint); err != nil {
		t.Fatal(err)
	}
	if model != "gpt-4o" || endpoint != "/v1beta/models/�:generateContent" {
		t.Errorf("model, endpoint = %q, %q; want NUL removed and bad UTF-8 replaced", model, endpoint)
	}
}

// TestWriterIsolatesBadRow: if Postgres still rejects one row, the rest of
// its batch, other tenants' rows included, is written anyway.
func TestWriterIsolatesBadRow(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	w := newWriter(conn, discard(), 10, 100, time.Hour)
	w.start()

	orphan := testRow(tn, key, "req_orphan")
	orphan.TenantID = "tn_does_not_exist" // violates the foreign key
	w.Write(testRow(tn, key, "req_1"))
	w.Write(orphan)
	w.Write(testRow(tn, key, "req_2"))
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	if n := countRows(t, conn); n != 2 {
		t.Errorf("usage_logs has %d rows, want the 2 good ones", n)
	}
}

// TestWriterPendingCountsQueuedRows: Pending is the cost of a tenant's rows
// from Write until their batch commits. An interval of an hour and a batch of
// 100 hold these rows until Close flushes them.
func TestWriterPendingCountsQueuedRows(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	other, otherKey, err := tenant.NewStore(conn).Create(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	w := newWriter(conn, discard(), 1000, 100, time.Hour)
	w.start()

	w.Write(withCost(testRow(tn, key, "req_1"), 100))
	w.Write(withCost(testRow(tn, key, "req_2"), 20))
	w.Write(withCost(testRow(other, otherKey, "req_3"), 7))

	if n := countRows(t, conn); n != 0 {
		t.Fatalf("usage_logs has %d rows, want the 3 still queued", n)
	}
	if got := w.Pending(tn.ID); got != 120 {
		t.Errorf("Pending(%s) = %d, want 120", tn.ID, got)
	}
	// Another tenant's rows don't count, and a tenant with none has 0.
	if got := w.Pending(other.ID); got != 7 {
		t.Errorf("Pending(%s) = %d, want 7", other.ID, got)
	}
	if got := w.Pending("tn_unknown"); got != 0 {
		t.Errorf("Pending(tn_unknown) = %d, want 0", got)
	}

	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, conn); n != 3 {
		t.Errorf("after Close, usage_logs has %d rows, want 3", n)
	}
	for _, id := range []string{tn.ID, other.ID} {
		if got := w.Pending(id); got != 0 {
			t.Errorf("after Close, Pending(%s) = %d, want 0", id, got)
		}
	}
}

// TestWriterPendingSkipsRowsWithoutCost: a row with no cost (a model with no
// price) adds nothing while it is queued, and nor does a cost of 0 (a cache
// hit). Writing one takes nothing off the count either.
func TestWriterPendingSkipsRowsWithoutCost(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	w := newWriter(conn, discard(), 1000, 100, time.Hour)
	w.start()

	unpriced := testRow(tn, key, "req_unpriced")
	unpriced.CostMicros = nil
	w.Write(unpriced)
	w.Write(withCost(testRow(tn, key, "req_hit"), 0))
	if got := w.Pending(tn.ID); got != 0 {
		t.Errorf("with two rows queued that cost nothing, Pending = %d, want 0", got)
	}

	w.Write(withCost(testRow(tn, key, "req_priced"), 30))
	if got := w.Pending(tn.ID); got != 30 {
		t.Errorf("Pending = %d, want 30, the priced row's alone", got)
	}

	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.Pending(tn.ID); got != 0 {
		t.Errorf("after Close, Pending = %d, want 0", got)
	}
	if n := countRows(t, conn); n != 3 {
		t.Errorf("after Close, usage_logs has %d rows, want all 3", n)
	}
}

// TestWriterPendingCountsUntilCommit: a row still counts while the insert that
// holds it waits on Postgres, and stops counting only once the batch commits. A
// lock on the table holds the insert up.
func TestWriterPendingCountsUntilCommit(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	// Two rows fill the batch, so the writer flushes at once.
	w := newWriter(conn, discard(), 10, 2, time.Hour)
	w.start()
	defer w.Close(context.Background())

	lock, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err := lock.Exec(`LOCK TABLE usage_logs IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	w.Write(withCost(testRow(tn, key, "req_1"), 100))
	w.Write(withCost(testRow(tn, key, "req_2"), 20))
	waitForBlockedInsert(t, conn)
	if got := w.Pending(tn.ID); got != 120 {
		t.Errorf("with the insert waiting, Pending = %d, want 120", got)
	}

	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	waitForRows(t, conn, 2)
	waitForPending(t, w, tn.ID, 0)
}

// TestWriterPendingCountsBeforeQueueing: Write counts a row before it queues
// it, so a flush that finishes with the row at once finds the cost already
// added. Holding the count's lock keeps Write from counting, and it must not
// have queued the row either. Nothing here touches Postgres.
func TestWriterPendingCountsBeforeQueueing(t *testing.T) {
	// Never started: there is no flush, and no database for one to write to.
	w := newWriter(nil, discard(), 10, 100, time.Hour)
	cost := int64(75)

	w.pendingMu.Lock()
	written := make(chan struct{})
	go func() {
		w.Write(Row{RequestID: "req_1", TenantID: "tn_1", CostMicros: &cost})
		close(written)
	}()
	// Long enough for a Write that doesn't wait for the count to queue the row.
	time.Sleep(50 * time.Millisecond)
	if n := len(w.rows); n != 0 {
		t.Errorf("Write queued %d rows while it could not yet count them, want 0", n)
	}
	w.pendingMu.Unlock()

	select {
	case <-written:
	case <-time.After(2 * time.Second):
		t.Fatal("Write never finished")
	}
	if got := w.Pending("tn_1"); got != 75 {
		t.Errorf("Pending(tn_1) = %d, want 75", got)
	}
	if n := len(w.rows); n != 1 {
		t.Errorf("the queue holds %d rows, want 1", n)
	}
}

// TestWriterPendingIgnoresDroppedRows: a row Write drops, whether the queue is
// full or the writer is closed, never counts.
func TestWriterPendingIgnoresDroppedRows(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	// Not started: nothing drains the one-slot queue.
	w := newWriter(conn, discard(), 1, 100, time.Hour)

	w.Write(withCost(testRow(tn, key, "req_1"), 100))
	w.Write(withCost(testRow(tn, key, "req_2"), 20)) // the queue is full
	if got := w.Pending(tn.ID); got != 100 {
		t.Errorf("after a row was dropped for a full queue, Pending = %d, want 100, the queued row's alone", got)
	}

	w.start()
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.Write(withCost(testRow(tn, key, "req_3"), 7)) // the writer is closed
	if got := w.Pending(tn.ID); got != 0 {
		t.Errorf("after Close and a write past it, Pending = %d, want 0", got)
	}
	if got := w.Dropped(); got != 2 {
		t.Errorf("Dropped() = %d, want 2", got)
	}
}

// TestWriterPendingDropsRejectedRows: a row Postgres rejects stops counting
// once the writer gives it up, whether it shared its batch (and was retried on
// its own) or was alone in it.
func TestWriterPendingDropsRejectedRows(t *testing.T) {
	cases := []struct {
		name string
		good bool // whether a row Postgres accepts shares the batch
	}{
		{"in a batch with a good row", true},
		{"alone", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, tn, key := newWriterDB(t)
			w := newWriter(conn, discard(), 10, 100, time.Hour)
			w.start()

			orphan := withCost(testRow(tn, key, "req_orphan"), 40)
			orphan.TenantID = "tn_does_not_exist" // violates the foreign key
			wantRows := 0
			if tc.good {
				w.Write(withCost(testRow(tn, key, "req_1"), 100))
				wantRows = 1
			}
			w.Write(orphan)
			if got := w.Pending(orphan.TenantID); got != 40 {
				t.Errorf("while queued, Pending(%s) = %d, want 40", orphan.TenantID, got)
			}

			if err := w.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{orphan.TenantID, tn.ID} {
				if got := w.Pending(id); got != 0 {
					t.Errorf("after the writer gave the row up, Pending(%s) = %d, want 0", id, got)
				}
			}
			if n := countRows(t, conn); n != wantRows {
				t.Errorf("usage_logs has %d rows, want %d", n, wantRows)
			}
		})
	}
}

// TestWriterPendingUnderConcurrentWrites runs Write, Pending and flushes at the
// same time, which is what -race is for. It holds Pending to what the budget's
// rebuild needs from it: reading Pending and then summing usage_logs never
// misses a row whose Write has returned. A batch that commits between the two
// reads is counted twice, which is allowed.
func TestWriterPendingUnderConcurrentWrites(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	// Small batches and a short interval keep flushes overlapping the writes.
	w := newWriter(conn, discard(), DefaultQueue, 7, 2*time.Millisecond)
	w.start()

	const writers, perWriter = 4, 100
	const rejected = "tn_does_not_exist"
	// Most rows are priced and tn's. Every 10th has no cost, and every 50th is
	// for a tenant Postgres rejects, which the writer gives up.
	rowFor := func(g, i int) Row {
		row := withCost(testRow(tn, key, "req_"+strconv.Itoa(g)+"_"+strconv.Itoa(i)), int64(1+i%9))
		switch {
		case i%50 == 0:
			row.TenantID = rejected
		case i%10 == 0:
			row.CostMicros = nil
		}
		return row
	}
	var wantRows, wantCost int64
	for g := range writers {
		for i := range perWriter {
			if row := rowFor(g, i); row.TenantID == tn.ID {
				wantRows++
				if row.CostMicros != nil {
					wantCost += *row.CostMicros
				}
			}
		}
	}

	var returned atomic.Int64 // the cost of tn's rows whose Write has returned
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Loaded before Pending: Write counted each of these rows before it
			// returned.
			written := returned.Load()
			queued := w.Pending(tn.ID)
			var stored int64
			err := conn.QueryRow(`SELECT COALESCE(SUM(cost_micros), 0)::bigint FROM usage_logs WHERE tenant_id = $1`, tn.ID).Scan(&stored)
			if err != nil {
				t.Error(err)
				return
			}
			if queued < 0 || queued > wantCost || queued+stored < written {
				t.Errorf("Pending = %d and usage_logs holds %d, but Write has returned for %d (all the rows cost %d)", queued, stored, written, wantCost)
				return
			}
		}
	})
	// Pending alone, as often as it can be read.
	readers.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if queued := w.Pending(tn.ID); queued < 0 || queued > wantCost {
				t.Errorf("Pending = %d, want 0 to %d", queued, wantCost)
				return
			}
		}
	})

	var writing sync.WaitGroup
	for g := range writers {
		writing.Go(func() {
			for i := range perWriter {
				row := rowFor(g, i)
				w.Write(row)
				if row.TenantID == tn.ID && row.CostMicros != nil {
					returned.Add(*row.CostMicros)
				}
			}
		})
	}
	writing.Wait()
	err := w.Close(context.Background())
	close(stop)
	readers.Wait()
	if err != nil {
		t.Fatal(err)
	}

	if got := w.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d, want 0", got)
	}
	for _, id := range []string{tn.ID, rejected} {
		if got := w.Pending(id); got != 0 {
			t.Errorf("after Close, Pending(%s) = %d, want 0", id, got)
		}
	}
	if n := countRows(t, conn); int64(n) != wantRows {
		t.Errorf("usage_logs has %d rows, want %d", n, wantRows)
	}
}
