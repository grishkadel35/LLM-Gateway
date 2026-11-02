package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/grishkadel/llm-gateway/internal/budget"
	"github.com/grishkadel/llm-gateway/internal/cache"
	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
	"github.com/grishkadel/llm-gateway/internal/metrics"
	"github.com/grishkadel/llm-gateway/internal/mockprovider"
	"github.com/grishkadel/llm-gateway/internal/ratelimit"
	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// The request line, through the gateway with main's parts: each request's
// line says who sent it, the route it took and the provider that answered,
// and its tokens and cost_micros are its usage row's.

// lineGateway is the gateway with main's parts, its whole log captured:
// tenants and usage rows in Postgres, and the response cache, the rate
// limiter and the budget ledger in Redis, on the client openRedis builds.
type lineGateway struct {
	http.Handler
	conn    *sql.DB
	rdb     *redis.Client
	store   stores
	writer  *usage.Writer
	logs    *lockedBuffer
	metrics *metrics.Metrics
}

// newLineGateway builds a lineGateway from cfg on a fresh database. Without
// REDIS_URL or DATABASE_URL the test is skipped.
//
// The gateway logs at Info, and as the test ends any Warn or Error the test
// doesn't expect fails it: warns is how many times it expects each, by
// message. The cache and the rate limiter fail open, and budgets fail closed,
// when a Redis call takes over 100 ms, each with a Warn or an Error. On a slow
// machine that would pass for a line that is wrong, so the failure says so.
func newLineGateway(t *testing.T, cfg *config.Config, warns map[string]int) *lineGateway {
	t.Helper()

	// Redis first, so that without REDIS_URL no database is made.
	rdb := gatewayRedis(t)
	conn := dbtest.New(t)
	if _, err := db.Migrate(context.Background(), conn); err != nil {
		t.Fatalf("Migrate() returned error: %v", err)
	}

	g := &lineGateway{conn: conn, rdb: rdb, store: stores{tenant.NewStore(conn), usage.NewReports(conn)}, logs: &lockedBuffer{}, metrics: metrics.New()}
	logger := slog.New(slog.NewJSONHandler(g.logs, nil))
	t.Cleanup(func() { checkWarns(t, g.logs.String(), warns) })
	g.writer = usage.NewWriter(conn, logger)
	// Registered after the check above, so it runs first: whatever the writer
	// logs as it drains is checked too.
	t.Cleanup(func() { g.writer.Close(context.Background()) })

	r, err := router(cfg, g.store, testAdminKey, recordUsage(g.writer, cfg.Providers, logger), ratelimit.New(rdb),
		cache.New(rdb, time.Minute), budget.New(rdb, conn, g.writer.Pending), g.metrics, nil, nil, logger)
	if err != nil {
		t.Fatalf("router() returned error: %v", err)
	}
	g.Handler = r
	return g
}

// checkWarns fails the test on each Warn or Error in logs, the gateway's JSON
// log, that warns doesn't expect, and on each it expects that was logged
// another number of times.
func checkWarns(t *testing.T, logs string, warns map[string]int) {
	t.Helper()
	got := map[string]int{}
	var unexpected []string
	for line := range strings.Lines(logs) {
		var entry struct{ Level, Msg string }
		_ = json.Unmarshal([]byte(line), &entry)
		if entry.Level != "WARN" && entry.Level != "ERROR" {
			continue
		}
		if _, expected := warns[entry.Msg]; expected {
			got[entry.Msg]++
		} else {
			unexpected = append(unexpected, strings.TrimSpace(line))
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("the gateway logged lines the test doesn't expect (below). The cache and the rate limiter fail open, "+
			"and budgets fail closed, when a Redis call takes over 100 ms, so a failure above may be down to a slow "+
			"machine, not the request line:\n%s", strings.Join(unexpected, "\n"))
	}
	for msg, want := range warns {
		if got[msg] != want {
			t.Errorf("%q was logged %d times, want %d", msg, got[msg], want)
		}
	}
}

// newTenant creates a tenant named name, with the limits update sets, and
// returns its ID and key. Every test shares the one Redis, so as the test
// ends the tenant's keys there are deleted: its budget hash for this month,
// the hash's missed counter, and its rate limit bucket. Its cache entries
// expire within a minute.
func (g *lineGateway) newTenant(t *testing.T, name string, update tenant.Update) (id, key string) {
	t.Helper()
	ctx := context.Background()
	tn, k, err := g.store.Create(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.Update(ctx, tn.ID, update); err != nil {
		t.Fatal(err)
	}
	start, _ := budget.Period(time.Now())
	hash := "budget:{" + tn.ID + "}:" + start.Format("2006-01")
	t.Cleanup(func() { g.rdb.Del(context.Background(), hash, hash+":missed", "ratelimit:{"+tn.ID+"}") })
	return tn.ID, k.Plaintext
}

// line returns the request line the gateway logged for the request with ID
// id, failing the test unless there is exactly one.
func (g *lineGateway) line(t *testing.T, id string) map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(g.logs.String()) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if entry["msg"] == "request" && entry["request_id"] == id {
			lines = append(lines, entry)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d request lines for request %s, want 1: %v", len(lines), id, lines)
	}
	return lines[0]
}

// lineRow is what a request line repeats of a usage row.
type lineRow struct {
	Input, CachedInput, CacheWrite, Output int64
	Cost                                   sql.NullInt64
	Cached, Streamed                       bool
}

// row returns the usage row of provider's attempt at the request with ID id.
// The writer must be closed first, so every row is in.
func (g *lineGateway) row(t *testing.T, id, provider string) lineRow {
	t.Helper()
	var r lineRow
	err := g.conn.QueryRow(`
		SELECT input_tokens, cached_input_tokens, cache_write_tokens, output_tokens, cost_micros, cached, streamed
		FROM usage_logs WHERE request_id = $1 AND provider = $2`, id, provider,
	).Scan(&r.Input, &r.CachedInput, &r.CacheWrite, &r.Output, &r.Cost, &r.Cached, &r.Streamed)
	if err != nil {
		t.Fatalf("no usage row for %s's attempt at request %s: %v", provider, id, err)
	}
	return r
}

// fields returns the request line's usage fields that repeat r: its tokens,
// its cost (null when it has none), cached and streamed.
func (r lineRow) fields() map[string]any {
	var cost any
	if r.Cost.Valid {
		cost = float64(r.Cost.Int64)
	}
	return map[string]any{
		"input_tokens":        float64(r.Input),
		"cached_input_tokens": float64(r.CachedInput),
		"cache_write_tokens":  float64(r.CacheWrite),
		"output_tokens":       float64(r.Output),
		"cost_micros":         cost,
		"cached":              r.Cached,
		"streamed":            r.Streamed,
	}
}

// recordedFields are the request line's fields that come from the request's
// Record.
var recordedFields = []string{"tenant_id", "provider", "model", "fallback_provider", "input_tokens", "cached_input_tokens",
	"cache_write_tokens", "output_tokens", "cost_micros", "cached", "streamed"}

// fromRecord returns the fields of line, a request line, that come from the
// request's Record. A field logged as null is there, with a nil value.
func fromRecord(line map[string]any) map[string]any {
	got := map[string]any{}
	for _, k := range recordedFields {
		if v, ok := line[k]; ok {
			got[k] = v
		}
	}
	return got
}

// with returns a copy of fields with more's entries too.
func with(fields map[string]any, more map[string]any) map[string]any {
	m := maps.Clone(fields)
	maps.Copy(m, more)
	return m
}

// lineConfig is a config file with six routes: openai and groq to the URL in
// its first argument, anthropic to its second, ollama, free, to its third,
// falling back to groq for qwen3.5:9b, local, free too, to its fourth, and
// gone, free, to a port nothing listens on.
const lineConfig = `
providers:
  openai:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: bearer
    format: openai
  groq:
    url: %[1]s/openai
    key: ${TEST_MOCK_KEY}
    auth: bearer
    format: openai
  anthropic:
    url: %[2]s
    key: ${TEST_MOCK_KEY}
    auth: x-api-key
    format: anthropic
    headers:
      anthropic-version: "2023-06-01"
  ollama:
    url: %[3]s
    auth: none
    format: openai
    free: true
    fallback:
      enabled: true
      provider: groq
      models:
        "qwen3.5:9b": qwen/qwen3.8-27b
  local:
    url: %[4]s
    auth: none
    format: openai
    free: true
  gone:
    url: http://127.0.0.1:1
    auth: none
    format: openai
    free: true
`

// cacheWritesUpstream starts a provider in the Anthropic format whose every
// answer, for claude-sonnet-4, reports writes to its prompt cache at both
// TTLs: 100 tokens for 5 minutes and 200 for an hour. It returns its URL.
func cacheWritesUpstream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_writes","type":"message","role":"assistant","model":"claude-sonnet-4",`+
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":30,`+
			`"cache_read_input_tokens":40,"cache_creation_input_tokens":300,`+
			`"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200},"output_tokens":50}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestRequestLine: through the gateway with main's parts, a request of each
// kind, and the line each leaves.
//   - Answered by a provider or the cache: the tenant, the route, the model
//     as the client named it, and the usage row's tokens, cost, cached and
//     streamed. After a fallback, the provider that answered, whose row it is.
//   - Refused by the gateway after reading the body (429, 402, 400
//     unknown_model): the tenant, the route and the model, and no usage. So
//     is a fallback's attempt refused after the route's provider failed,
//     though that failure reported its usage.
//   - Refused by Auth: the route alone.
//   - Off the provider routes: none of these fields.
func TestRequestLine(t *testing.T) {
	mock := httptest.NewServer(mockprovider.Handler())
	t.Cleanup(mock.Close)
	ollama := newFakeOllama(t, ollamaKnobs{status: http.StatusInternalServerError})
	local := newFakeOllama(t, ollamaKnobs{})
	t.Setenv("TEST_MOCK_KEY", "mock")
	g := newLineGateway(t, loadConfig(t, fmt.Sprintf(lineConfig, mock.URL, cacheWritesUpstream(t), ollama.server.URL, local.server.URL)),
		map[string]int{"falling back": 2, "model not priced; request refused": 1, "upstream request failed": 1})

	acme, acmeKey := g.newTenant(t, "acme", tenant.Update{})
	// One token a minute: its first request leaves its bucket in debt for half
	// an hour.
	oneToken := int64(1)
	limited, limitedKey := g.newTenant(t, "limited", tenant.Update{RateLimitTokensPerMin: &oneToken})
	var nothing int64
	broke, brokeKey := g.newTenant(t, "broke", tenant.Update{BudgetMicros: &nothing})

	const chat = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	// send sends req, and fails the test unless the response has status and
	// each header in headers (name, then value). It returns the response's
	// request ID.
	send := func(what string, req *http.Request, status int, headers ...string) string {
		t.Helper()
		rec := serve(g, req)
		failedClosed(t, rec)
		if rec.Code != status {
			t.Fatalf("%s: status %d, want %d; body %s", what, rec.Code, status, rec.Body)
		}
		for i := 0; i < len(headers); i += 2 {
			if got := rec.Header().Get(headers[i]); got != headers[i+1] {
				t.Fatalf("%s: %s %q, want %q", what, headers[i], got, headers[i+1])
			}
		}
		return rec.Header().Get("X-Request-ID")
	}
	get := func(path string) *http.Request { return httptest.NewRequest(http.MethodGet, path, nil) }

	paid := send("paid", postAs(acmeKey, "/anthropic/v1/messages",
		`{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`), http.StatusOK)
	stream := send("stream", postAs(acmeKey, "/openai/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`), http.StatusOK)
	miss := send("miss", postAs(acmeKey, "/openai/v1/chat/completions", zeroTemperature), http.StatusOK, "X-Gateway-Cache", "miss")
	hit := send("hit", postAs(acmeKey, "/openai/v1/chat/completions", zeroTemperature), http.StatusOK, "X-Gateway-Cache", "hit")
	fallback := send("fallback", postAs(acmeKey, "/ollama/v1/chat/completions",
		`{"model":"qwen3.5:9b","messages":[{"role":"user","content":"hi"}]}`), http.StatusOK, "X-Gateway-Provider", "groq")
	free := send("free", postAs(acmeKey, "/local/v1/chat/completions",
		`{"model":"qwen3.5:9b","messages":[{"role":"user","content":"hi"}]}`), http.StatusOK)
	// No provider answers: the proxy reports the failed attempt, so the line
	// has its 502's usage row, as for any attempt that ended.
	unanswered := send("unanswered", postAs(acmeKey, "/gone/v1/chat/completions",
		`{"model":"qwen3.5:9b","messages":[{"role":"user","content":"hi"}]}`), http.StatusBadGateway)
	unpriced := send("unknown model", postAs(acmeKey, "/groq/v1/chat/completions",
		`{"model":"llama-3.3-70b-versatile","messages":[{"role":"user","content":"hi"}]}`), http.StatusBadRequest)
	admitted := send("the limited tenant's first", postAs(limitedKey, "/openai/v1/chat/completions", chat), http.StatusOK)
	rateLimited := send("429", postAs(limitedKey, "/openai/v1/chat/completions", chat), http.StatusTooManyRequests)
	overBudget := send("402", postAs(brokeKey, "/openai/v1/chat/completions", chat), http.StatusPaymentRequired)
	// ollama fails, and groq's attempt is refused: ollama's usage was
	// reported, but the client got groq's 402.
	fallbackRefused := send("fallback refused", postAs(brokeKey, "/ollama/v1/chat/completions",
		`{"model":"qwen3.5:9b","messages":[{"role":"user","content":"hi"}]}`), http.StatusPaymentRequired, "X-Gateway-Provider", "groq")
	unauthorized := send("401", postAs("gw_not-a-key", "/openai/v1/chat/completions", chat), http.StatusUnauthorized)
	health := send("/health", get("/health"), http.StatusOK)
	adminReq := get("/admin/tenants/" + acme + "/budget")
	adminReq.Header.Set("Authorization", "Bearer "+testAdminKey)
	admin := send("admin", adminReq, http.StatusOK)
	unknownPath := send("unknown path", get("/nope/v1/chat/completions"), http.StatusNotFound)

	// The writer inserts its rows in batches, off the request path: Close
	// inserts the rest.
	if err := g.writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The rows the answered lines repeat, checked for what makes each case.
	paidRow := g.row(t, paid, "anthropic")
	if paidRow.CacheWrite != 300 || !paidRow.Cost.Valid || paidRow.Cost.Int64 <= 0 || paidRow.Streamed || paidRow.Cached {
		t.Errorf("the paid request's row = %+v, want 100 + 200 cache writes, a cost, not streamed, not cached", paidRow)
	}
	streamRow := g.row(t, stream, "openai")
	if !streamRow.Streamed || !streamRow.Cost.Valid || streamRow.Cost.Int64 <= 0 {
		t.Errorf("the stream's row = %+v, want streamed, with a cost", streamRow)
	}
	hitRow := g.row(t, hit, "openai")
	if !hitRow.Cached || !hitRow.Cost.Valid || hitRow.Cost.Int64 != 0 {
		t.Errorf("the hit's row = %+v, want cached, at cost 0", hitRow)
	}
	freeRow := g.row(t, free, "local")
	if !freeRow.Cost.Valid || freeRow.Cost.Int64 != 0 || freeRow.Output == 0 {
		t.Errorf("the free provider's row = %+v, want its tokens, at cost 0", freeRow)
	}
	groqRow := g.row(t, fallback, "groq")
	if !groqRow.Cost.Valid || groqRow.Cost.Int64 <= 0 || groqRow.Output == 0 {
		t.Errorf("groq's row = %+v, want its tokens, with a cost", groqRow)
	}
	goneRow := g.row(t, unanswered, "gone")
	if !goneRow.Cost.Valid || goneRow.Cost.Int64 != 0 || goneRow.Input != 0 || goneRow.Output != 0 {
		t.Errorf("the unanswered request's row = %+v, want no tokens, at cost 0", goneRow)
	}

	cases := []struct {
		name, id string
		want     map[string]any
	}{
		{"paid", paid, with(paidRow.fields(), map[string]any{"tenant_id": acme, "provider": "anthropic", "model": "claude-sonnet-4"})},
		{"stream", stream, with(streamRow.fields(), map[string]any{"tenant_id": acme, "provider": "openai", "model": "gpt-4o"})},
		{"cache miss", miss, with(g.row(t, miss, "openai").fields(), map[string]any{"tenant_id": acme, "provider": "openai", "model": "gpt-4o"})},
		{"cache hit", hit, with(hitRow.fields(), map[string]any{"tenant_id": acme, "provider": "openai", "model": "gpt-4o"})},
		// The model as the client named it, and groq's row, not ollama's.
		{"fallback", fallback, with(groqRow.fields(), map[string]any{"tenant_id": acme, "provider": "ollama", "model": "qwen3.5:9b",
			"fallback_provider": "groq"})},
		{"free", free, with(freeRow.fields(), map[string]any{"tenant_id": acme, "provider": "local", "model": "qwen3.5:9b"})},
		{"502, never answered", unanswered, with(goneRow.fields(), map[string]any{"tenant_id": acme, "provider": "gone", "model": "qwen3.5:9b"})},
		{"admitted", admitted, with(g.row(t, admitted, "openai").fields(), map[string]any{"tenant_id": limited, "provider": "openai",
			"model": "gpt-4o"})},
		{"400 unknown_model", unpriced, map[string]any{"tenant_id": acme, "provider": "groq", "model": "llama-3.3-70b-versatile", "streamed": false}},
		{"429", rateLimited, map[string]any{"tenant_id": limited, "provider": "openai", "model": "gpt-4o", "streamed": false}},
		{"402", overBudget, map[string]any{"tenant_id": broke, "provider": "openai", "model": "gpt-4o", "streamed": false}},
		{"fallback refused", fallbackRefused, map[string]any{"tenant_id": broke, "provider": "ollama", "model": "qwen3.5:9b", "streamed": false}},
		{"401", unauthorized, map[string]any{"provider": "openai"}},
		{"/health", health, map[string]any{}},
		{"admin", admin, map[string]any{}},
		{"unknown path", unknownPath, map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fromRecord(g.line(t, tc.id)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("fields from the Record = %v\nwant %v", got, tc.want)
			}
		})
	}

	// Off the provider routes, the lines are as they were.
	for _, id := range []string{health, admin, unknownPath} {
		want := []string{"bytes", "duration_ms", "level", "method", "msg", "path", "remote_addr", "request_id", "status", "time"}
		if got := slices.Sorted(maps.Keys(g.line(t, id))); !slices.Equal(got, want) {
			t.Errorf("request %s: line fields = %q, want %q", id, got, want)
		}
	}
}

// TestRequestLineClientLeavesMidStream: a client that leaves mid-stream
// still gets its line, with the usage seen so far and cost_micros null: its
// row's, as the usage is incomplete. The gateway runs behind a real server,
// so ReverseProxy aborts the stream with panic(http.ErrAbortHandler), as it
// would in production, and the line is written as that unwinds.
func TestRequestLineClientLeavesMidStream(t *testing.T) {
	g := newLineGateway(t, openaiAt(t, sseUpstream(t, 1000), ""), map[string]int{"usage incomplete; logged without cost": 1})
	acme, key := g.newTenant(t, "acme", tenant.Update{})

	// done closes when the gateway is through with the request, abort
	// included, and so once the line is written.
	done := make(chan struct{})
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer close(done)
		g.ServeHTTP(w, req)
	}))
	t.Cleanup(gw.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"max_tokens":5,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// The usage chunk went through the gateway's meter on its way here.
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	cancel()
	resp.Body.Close()
	receive(t, done, "the gateway to finish the request")
	if err := g.writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	id := resp.Header.Get("X-Request-ID")
	row := g.row(t, id, "openai")
	if row.Input != 1000 || row.Cost.Valid || !row.Streamed {
		t.Errorf("the row = %+v, want the 1,000 input tokens seen, no cost, streamed", row)
	}
	want := with(row.fields(), map[string]any{"tenant_id": acme, "provider": "openai", "model": "gpt-4o"})
	if got := fromRecord(g.line(t, id)); !reflect.DeepEqual(got, want) {
		t.Errorf("fields from the Record = %v\nwant %v", got, want)
	}
}
