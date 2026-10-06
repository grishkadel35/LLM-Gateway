package usage

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strconv"
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

// TestWriterStoresEveryField reads a row back, NULLs included.
func TestWriterStoresEveryField(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	w := newWriter(conn, discard(), 10, 100, time.Hour)
	w.start()

	priced := testRow(tn, key, "req_priced")
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
		isCached, streamed                        bool
		latency                                   int
		createdAt                                 time.Time
	}
	read := func(requestID string) stored {
		var s stored
		err := conn.QueryRow(`
			SELECT provider_request_id, tenant_id, api_key_id, provider, model, endpoint, status,
			       input_tokens, cached_input_tokens, cache_write_tokens, output_tokens,
			       cost_micros, cached, streamed, latency_ms, created_at
			FROM usage_logs WHERE request_id = $1`, requestID,
		).Scan(&s.providerRequestID, &s.tenantID, &s.keyID, &s.provider, &s.model, &s.endpoint, &s.status,
			&s.input, &s.cached, &s.cacheWrite, &s.output, &s.cost, &s.isCached, &s.streamed, &s.latency, &s.createdAt)
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
	if p.input != 15 || p.cached != 5 || p.cacheWrite != 3 || p.output != 17 || !p.cost.Valid || p.cost.Int64 != 75 {
		t.Errorf("tokens and cost = %+v", p)
	}
	if !p.createdAt.Equal(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("created_at = %v, want the request's time, not the flush time", p.createdAt)
	}

	u := read("req_unpriced")
	if u.cost.Valid || u.providerRequestID.Valid {
		t.Errorf("cost = %v, provider_request_id = %v; want both NULL", u.cost, u.providerRequestID)
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
