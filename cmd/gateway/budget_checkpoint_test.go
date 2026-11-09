package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/grishkadel/llm-gateway/internal/budget"
	"github.com/grishkadel/llm-gateway/internal/cache"
	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
	"github.com/grishkadel/llm-gateway/internal/metrics"
	"github.com/grishkadel/llm-gateway/internal/pricing"
	"github.com/grishkadel/llm-gateway/internal/ratelimit"
	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// The Week 5 checkpoint: budgets through the gateway with main's parts, down
// to the Redis client openRedis builds, against an upstream whose answers
// cost a known amount.

// pricedInput and pricedOutput are the tokens pricedUpstream's every answer
// reports: 50,050 µ$ on gpt-4o, at $2.50 and $10 per million.
const (
	pricedInput  = 20
	pricedOutput = 5_000
)

// pricedCap is the output cap of every request cappedChat makes. On gpt-4o,
// at $10 per million output tokens, it reserves about $0.10, twice what
// pricedUpstream's answer then costs.
const pricedCap = 10_000

// cappedChat is a chat request for model, capped at pricedCap output tokens.
// It sets no temperature, so the response cache never takes it.
func cappedChat(model string) string {
	return fmt.Sprintf(`{"model":%q,"max_tokens":%d,"messages":[{"role":"user","content":"hi"}]}`, model, pricedCap)
}

// pricedCosts returns what a request with body reserves on provider for
// model, and what pricedUpstream's answer to it costs there, both at the
// prices in force at at, by the functions Budget and recordUsage price them
// with. body must be one of cappedChat's.
func pricedCosts(t *testing.T, provider, model, body string, at time.Time) (reserve, cost int64) {
	t.Helper()
	reserve, ok := pricing.Reserve(provider, model, reserveTokens(body), pricedCap, 1, at)
	if !ok {
		t.Fatalf("%s/%s should be priced", provider, model)
	}
	// The answer names no model, so it is priced as the one the request asked
	// for.
	answer := usage.Result{Usage: usage.Usage{Model: model, Input: pricedInput, Output: pricedOutput}, Status: http.StatusOK,
		Complete: true, UsageComplete: true}
	if cost, ok = pricing.ResultCost(provider, false, answer, model, at); !ok {
		t.Fatalf("%s/%s should be priced", provider, model)
	}
	return reserve, cost
}

// pricedUpstream is a provider in the OpenAI format whose every answer
// reports pricedInput and pricedOutput tokens, so each request costs a known
// amount on the model it asked for. While hold is set, it holds each request
// it gets until the test lets it go.
type pricedUpstream struct {
	url  string
	hold atomic.Bool
	// arrivals receives a value as each held request arrives. It has room for
	// more requests than any test here sends, so the upstream never waits on
	// it, even when the budget lets through more than it should: a broken
	// reservation step fails the test instead of hanging it.
	arrivals chan struct{}
	// release lets one held request go for each value sent on it. Any still
	// held when the test ends are let go then.
	release chan struct{}
}

func newPricedUpstream(t *testing.T) *pricedUpstream {
	t.Helper()
	u := &pricedUpstream{arrivals: make(chan struct{}, 64), release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.hold.Load() {
			u.arrivals <- struct{}{}
			<-u.release
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-budget","object":"chat.completion","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			pricedInput, pricedOutput, pricedInput+pricedOutput)
	}))
	// Cleanups run last-registered first, so held requests are let go before
	// Close, which waits for them.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(u.release) })
	u.url = srv.URL
	return u
}

// warned80 is the Warn Budget logs at the first reservation that takes a
// tenant's month to 80% of its budget or more: once for each hash the ledger
// builds.
const warned80 = "tenant's spend has reached 80% of its monthly budget"

// budgetGateway is the gateway with main's parts: the tenant store and the
// usage writer on Postgres, and the response cache, the rate limiter and the
// budget ledger on Redis, on the client openRedis builds. It serves one
// tenant.
type budgetGateway struct {
	http.Handler
	conn    *sql.DB
	rdb     *redis.Client
	writer  *usage.Writer
	metrics *metrics.Metrics
	// tenantID and key are the tenant's ID and gateway key.
	tenantID, key string
	// hash is the tenant's hash for this month in the ledger.
	hash string
	// warns is how many times the test expects each Warn the gateway logs, by
	// message, warned80's included. Any other line fails the test.
	warns map[string]int
}

// newBudgetGateway builds a budgetGateway from cfg on a fresh database, with
// a new tenant whose rate limit no test here reaches. Without REDIS_URL or
// DATABASE_URL the test is skipped. Every test shares the one Redis, so as
// the test ends it deletes only the tenant's own keys: its hash for this
// month, the hash's missed counter, and its rate limit bucket.
//
// The gateway logs at Warn and above. As the test ends, any line the test
// doesn't expect (see warns) fails it, saying what can cause it.
func newBudgetGateway(t *testing.T, cfg *config.Config) *budgetGateway {
	t.Helper()

	// Redis first, so that without REDIS_URL no database is made.
	rdb := gatewayRedis(t)
	conn := dbtest.New(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatalf("Migrate() returned error: %v", err)
	}
	store := stores{tenant.NewStore(conn), usage.NewReports(conn)}
	tn, key, err := store.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}

	start, _ := budget.Period(time.Now())
	g := &budgetGateway{
		conn:     conn,
		rdb:      rdb,
		metrics:  metrics.New(),
		tenantID: tn.ID,
		key:      key.Plaintext,
		hash:     "budget:{" + tn.ID + "}:" + start.Format("2006-01"),
		warns:    map[string]int{warned80: 0},
	}
	t.Cleanup(func() { rdb.Del(context.Background(), g.hash, g.hash+":missed", "ratelimit:{"+tn.ID+"}") })

	var logs lockedBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	t.Cleanup(func() { g.checkLogs(t, logs.String()) })
	g.writer = usage.NewWriter(conn, logger)
	// Registered after the check above, so it runs first: whatever the writer
	// logs as it drains is checked too.
	t.Cleanup(func() { g.writer.Close(context.Background()) })

	// No request here sets a temperature, so the cache takes none, and an
	// entry would last a minute anyway.
	r, err := router(cfg, store, testAdminKey, recordUsage(g.writer, cfg.Providers, logger), ratelimit.New(rdb),
		cache.New(rdb, time.Minute), budget.New(rdb, conn, g.writer.Pending), g.metrics, nil, nil, logger)
	if err != nil {
		t.Fatalf("router() returned error: %v", err)
	}
	g.Handler = r

	// 50 requests at once charge the limiter about half a million tokens: far
	// below this, so it never refuses one. Its bucket is full again, and
	// gone, well within a second.
	g.patch(t, `{"rate_limit_tokens_per_min":100000000}`)
	return g
}

// checkLogs fails the test on each line of logs, the gateway's JSON log at
// Warn and above, that g.warns doesn't expect, and on each Warn it expects
// that was logged another number of times.
func (g *budgetGateway) checkLogs(t *testing.T, logs string) {
	t.Helper()
	got := map[string]int{}
	var unexpected []string
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var entry struct{ Msg string }
		_ = json.Unmarshal([]byte(line), &entry)
		if _, expected := g.warns[entry.Msg]; expected {
			got[entry.Msg]++
		} else {
			unexpected = append(unexpected, line)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("the gateway logged lines the test doesn't expect (below). \"budget settlement failed\" means the "+
			"ledger kept a reservation in place of the charge, so spent and the rows disagree: Settle has 100 ms, so a "+
			"loaded machine can cause it. \"budget check failed\" is a budget failing closed, which a Redis script over "+
			"100 ms or a rebuild over 2 s causes. The rate limiter and the cache fail open after 100 ms, with a Warn:\n%s",
			strings.Join(unexpected, "\n"))
	}
	for msg, want := range g.warns {
		if got[msg] != want {
			t.Errorf("%q was logged %d times, want %d", msg, got[msg], want)
		}
	}
}

// post sends body to path as the tenant, and returns the response.
func (g *budgetGateway) post(path, body string) *httptest.ResponseRecorder {
	return serve(g, postAs(g.key, path, body))
}

// admin sends a request to the admin API, and returns the response.
func (g *budgetGateway) admin(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAdminKey)
	return serve(g, req)
}

// patch sets the tenant's limits in body, a JSON object, through the admin
// API.
func (g *budgetGateway) patch(t *testing.T, body string) {
	t.Helper()
	if rec := g.admin(http.MethodPatch, "/admin/tenants/"+g.tenantID, body); rec.Code != http.StatusOK {
		t.Fatalf("PATCH %s: status %d; body %s", body, rec.Code, rec.Body)
	}
}

// setBudget sets the tenant's monthly budget to micros µ$.
func (g *budgetGateway) setBudget(t *testing.T, micros int64) {
	t.Helper()
	g.patch(t, fmt.Sprintf(`{"budget_micros":%d}`, micros))
}

// spent returns the tenant's spend for the month, as the admin API's budget
// report gives it: the ledger's, rebuilt first if its hash is missing.
func (g *budgetGateway) spent(t *testing.T) int64 {
	t.Helper()
	rec := g.admin(http.MethodGet, "/admin/tenants/"+g.tenantID+"/budget", "")
	failedClosed(t, rec)
	var report struct {
		SpentMicros int64 `json:"spent_micros"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &report); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("budget report: status %d, body %s (%v); want 200 and JSON", rec.Code, rec.Body, err)
	}
	return report.SpentMicros
}

// loseMonth deletes the tenant's hash for this month and its missed counter,
// as a Redis restart would, and fails the test if there was no hash.
func (g *budgetGateway) loseMonth(t *testing.T) {
	t.Helper()
	n, err := g.rdb.Del(context.Background(), g.hash, g.hash+":missed").Result()
	if err != nil || n == 0 {
		t.Fatalf("deleting %s: %d keys deleted (%v); want the hash at least", g.hash, n, err)
	}
}

// lockRows takes an EXCLUSIVE lock on usage_logs until the function it
// returns, or the end of the test, releases it. The lock blocks the writer's
// inserts but not a SELECT, such as a rebuild's SUM. So while it is held, the
// rows the gateway queues stay in the writer's queue, and no batch can commit
// between a rebuild's two reads, the queue and then the SUM, where it would
// be counted in both.
func (g *budgetGateway) lockRows(t *testing.T) (unlock func()) {
	t.Helper()
	tx, err := g.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// Registered after newBudgetGateway's cleanups, so it runs before them:
	// closing the writer waits on the lock.
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.Exec(`LOCK TABLE usage_logs IN EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback() }
}

// waitCommitted waits until none of the tenant's rows are left in the
// writer's queue: their inserts have committed. The writer flushes every
// second, and Pending has nothing to wait on, so it is checked every 10 ms,
// for up to 5 s.
func (g *budgetGateway) waitCommitted(t *testing.T) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(5 * time.Second)
	for g.writer.Pending(g.tenantID) != 0 {
		select {
		case <-tick.C:
		case <-timeout:
			t.Fatalf("after 5 s the writer still holds %d µ$ of the tenant's rows; it flushes every second",
				g.writer.Pending(g.tenantID))
		}
	}
}

// closeWriter closes the usage writer, which inserts every row still queued.
func (g *budgetGateway) closeWriter(t *testing.T) {
	t.Helper()
	if err := g.writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// committed returns how many of the tenant's usage rows are in usage_logs,
// and what they cost in all.
func (g *budgetGateway) committed(t *testing.T) (rows, cost int64) {
	t.Helper()
	err := g.conn.QueryRow(`SELECT count(*), COALESCE(SUM(cost_micros), 0) FROM usage_logs WHERE tenant_id = $1`,
		g.tenantID).Scan(&rows, &cost)
	if err != nil {
		t.Fatal(err)
	}
	return rows, cost
}

// failedClosed fails the test if rec is a 503 budget_unavailable, saying what
// causes one: budgets fail closed, so a slow Redis refuses a request where
// the rate limiter and the cache would let it through with a Warn.
func failedClosed(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Error struct{ Type string } `json:"error"`
	}
	if rec.Code == http.StatusServiceUnavailable && json.Unmarshal(rec.Body.Bytes(), &body) == nil &&
		body.Error.Type == "budget_unavailable" {
		t.Fatalf("status 503 budget_unavailable: budgets fail closed, and a Redis script over 100 ms or a rebuild "+
			"over 2 s gives this, so a loaded machine can cause it; body %s", rec.Body)
	}
}

// checkAdmitted fails the test unless rec, the response to what, is a 200.
func checkAdmitted(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	failedClosed(t, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d, want 200; body %s", what, rec.Code, rec.Body)
	}
}

// checkRefused fails the test unless rec is the gateway's 402 for a request
// reserving reserve, with spent spent of budgetMicros, in the month holding
// at: every field of its error, the message aside.
func checkRefused(t *testing.T, rec *httptest.ResponseRecorder, budgetMicros, spent, reserve int64, at time.Time) {
	t.Helper()
	failedClosed(t, rec)
	got := overBudget(t, rec)
	delete(got, "message")
	start, end := budget.Period(at)
	want := map[string]any{
		"type":             "budget_exceeded",
		"budget_micros":    float64(budgetMicros),
		"spent_micros":     float64(spent),
		"remaining_micros": float64(budgetMicros - spent),
		"reserve_micros":   float64(reserve),
		"period":           start.Format("2006-01"),
		"resets_at":        end.Format(time.RFC3339),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("402 error = %v\nwant %v", got, want)
	}
}

// TestBudgetOneDollar: the roadmap's checkpoint. A tenant with a $1 budget
// sends requests of a known cost, one after another. Each gets in while what
// is left of the budget covers its reservation, and the first that finds it
// doesn't gets 402, with the numbers behind it. The usage rows then sum to at
// most $1, and to the ledger's spend.
func TestBudgetOneDollar(t *testing.T) {
	u := newPricedUpstream(t)
	g := newBudgetGateway(t, mockConfigAt(t, u.url))
	body := cappedChat("gpt-4o")
	at := time.Now()
	reserve, cost := pricedCosts(t, "openai", "gpt-4o", body, at)

	const budgetMicros = 1_000_000
	g.setBudget(t, budgetMicros)
	// Each request settles before the next is sent, so request k finds (k-1)
	// costs spent, and gets in if its reservation fits on top. At 100,068 µ$
	// reserved and 50,050 spent each, 18 do, and the 19th finds 99,100 left.
	admitted := (budgetMicros-reserve)/cost + 1
	// The last reservation admitted takes spent past budget - cost, which is
	// over 80% of the budget.
	g.warns[warned80] = 1

	for k := range admitted {
		checkAdmitted(t, g.post("/openai/v1/chat/completions", body), fmt.Sprintf("request %d of %d", k+1, admitted+1))
	}
	spent := admitted * cost
	checkRefused(t, g.post("/openai/v1/chat/completions", body), budgetMicros, spent, reserve, at)

	g.closeWriter(t)
	rows, sum := g.committed(t)
	if rows != admitted || sum != spent {
		t.Errorf("%d usage rows costing %d µ$ in all, want %d costing %d", rows, sum, admitted, spent)
	}
	if sum > budgetMicros {
		t.Errorf("the usage rows cost %d µ$ in all, over the budget of %d", sum, budgetMicros)
	}
	if got := g.spent(t); got != sum {
		t.Errorf("spent_micros = %d, want the rows' %d", got, sum)
	}
}

// TestBudgetNoOverspendUnderLoad: the checkpoint's concurrency half. Of 50
// requests sent at once against a budget with room for exactly 10 of their
// reservations, exactly 10 reach the provider, and 40 get 402. Redis runs
// each reservation whole, so no two requests can take the same part of the
// budget. The provider holds every request it gets until all 50 have reached
// it or been refused, so none settles while the others are checked. Once it
// lets go, the rows sum to at most the budget, and to the ledger's spend.
func TestBudgetNoOverspendUnderLoad(t *testing.T) {
	u := newPricedUpstream(t)
	g := newBudgetGateway(t, mockConfigAt(t, u.url))
	body := cappedChat("gpt-4o")
	at := time.Now()
	reserve, cost := pricedCosts(t, "openai", "gpt-4o", body, at)

	// 1 µ$ short of room for 11.
	const sent, n = 50, 10
	budgetMicros := (n+1)*reserve - 1
	g.setBudget(t, budgetMicros)
	// The month's hash is built first, so the 50 race on their reservations
	// alone.
	if spent := g.spent(t); spent != 0 {
		t.Fatalf("spent_micros = %d before any request, want 0", spent)
	}
	// The 9th reservation takes spent past 80% of the budget.
	g.warns[warned80] = 1

	// Ten connections, opened before the requests go, serve their tenant
	// lookups. Left alone, the pool would open one for each of the 50 at once,
	// and under -race that can hold a Redis call past its 100 ms deadline: the
	// test then fails on a Warn or a 503, though no budget went wrong.
	// Go note: Conn takes a connection from the pool, opening one if none is
	// idle, and its Close puts the connection back.
	g.conn.SetMaxOpenConns(10)
	g.conn.SetMaxIdleConns(10)
	var conns []*sql.Conn
	for range 10 {
		c, err := g.conn.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Close()
	}

	u.hold.Store(true)
	// Go note: a receive from a closed channel never blocks, so closing start
	// lets all 50 goroutines go at once.
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, sent)
	for range sent {
		req := postAs(g.key, "/openai/v1/chat/completions", body)
		go func() {
			<-start
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, req)
			responses <- rec
		}()
	}
	close(start)

	// Until the upstream lets go, each request either reaches it or gets a
	// response, never both.
	admitted := 0
	var refused []*httptest.ResponseRecorder
	for admitted+len(refused) < sent {
		select {
		case <-u.arrivals:
			admitted++
		case rec := <-responses:
			refused = append(refused, rec)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out with %d requests at the upstream and %d answered, of %d", admitted, len(refused), sent)
		}
	}
	// Let the admitted ones go, and wait for them, so they have settled.
	for range admitted {
		u.release <- struct{}{}
	}
	for range admitted {
		checkAdmitted(t, receive(t, responses, "a released response"), "a released request")
	}

	if admitted != n {
		t.Errorf("%d of %d simultaneous requests reached the upstream and %d were answered without; want exactly %d and %d",
			admitted, sent, len(refused), n, sent-n)
	}
	// A request is refused only once all 10 have reserved, none of which has
	// settled, so every 402 finds the same spend.
	for _, rec := range refused {
		checkRefused(t, rec, budgetMicros, n*reserve, reserve, at)
	}

	spent := g.spent(t)
	g.closeWriter(t)
	rows, sum := g.committed(t)
	if spent != n*cost || rows != n || sum != spent {
		t.Errorf("spent_micros %d; %d usage rows costing %d µ$ in all; want %d, and %d rows costing that",
			spent, rows, sum, n*cost, n)
	}
	if sum > budgetMicros {
		t.Errorf("the usage rows cost %d µ$ in all, over the budget of %d", sum, budgetMicros)
	}
}

// TestBudgetRedisLossAtTheBoundary: a Redis restart loses the tenant's month,
// and the ledger rebuilds it from the usage rows, both those in Postgres and
// those still in the writer's queue. With the budget set at a boundary, the
// next request is admitted or refused exactly as before the loss: refused,
// with the same 402, when the budget is 1 µ$ short of one more reservation,
// and admitted once, then refused, when it has room for exactly one.
func TestBudgetRedisLossAtTheBoundary(t *testing.T) {
	u := newPricedUpstream(t)
	g := newBudgetGateway(t, mockConfigAt(t, u.url))
	body := cappedChat("gpt-4o")
	at := time.Now()
	reserve, cost := pricedCosts(t, "openai", "gpt-4o", body, at)
	// Of the three hashes the test builds, only the last has a reservation
	// that reaches 80% of the budget.
	g.warns[warned80] = 1

	// Three requests, whose rows the writer commits to Postgres.
	for k := range 3 {
		checkAdmitted(t, g.post("/openai/v1/chat/completions", body), fmt.Sprintf("request %d", k+1))
	}
	g.waitCommitted(t)
	// Then the lock holds back the writer's inserts until the end, so the
	// rows of the next two stay in its queue, and no batch can commit inside
	// a rebuild below and be counted twice.
	unlock := g.lockRows(t)
	for k := range 2 {
		checkAdmitted(t, g.post("/openai/v1/chat/completions", body), fmt.Sprintf("request %d", k+4))
	}
	if rows, inRows := g.committed(t); rows != 3 || inRows != 3*cost || g.writer.Pending(g.tenantID) != 2*cost {
		t.Fatalf("%d rows costing %d µ$ in Postgres, and %d µ$ in the writer's queue; want 3 costing %d, and %d",
			rows, inRows, g.writer.Pending(g.tenantID), 3*cost, 2*cost)
	}
	spent := 5 * cost
	if got := g.spent(t); got != spent {
		t.Fatalf("spent_micros = %d before the loss, want %d", got, spent)
	}

	// 1 µ$ short of one more reservation: the same 402 before the loss and
	// after it, from the month the refused request rebuilds.
	short := spent + reserve - 1
	g.setBudget(t, short)
	checkRefused(t, g.post("/openai/v1/chat/completions", body), short, spent, reserve, at)
	g.loseMonth(t)
	checkRefused(t, g.post("/openai/v1/chat/completions", body), short, spent, reserve, at)

	// Room for exactly one: rebuilt, this time by the budget report, the
	// month holds the spend from before the loss, and then admits one more
	// request and refuses the next.
	g.setBudget(t, spent+reserve)
	g.loseMonth(t)
	if got := g.spent(t); got != spent {
		t.Fatalf("spent_micros = %d after the loss, want %d, as before it", got, spent)
	}
	checkAdmitted(t, g.post("/openai/v1/chat/completions", body), "the request with room for it")
	checkRefused(t, g.post("/openai/v1/chat/completions", body), spent+reserve, spent+cost, reserve, at)

	unlock()
	g.closeWriter(t)
	rows, sum := g.committed(t)
	if rows != 6 || sum != spent+cost {
		t.Errorf("%d usage rows costing %d µ$ in all, want 6 costing %d", rows, sum, spent+cost)
	}
	if got := g.spent(t); got != sum {
		t.Errorf("spent_micros = %d, want the rows' %d", got, sum)
	}
}

// TestBudgetRedisLossInFlight: a request in flight when Redis loses its
// tenant's month reserved in a hash that is gone. The month is rebuilt
// without its reservation, as the request has no usage row yet, and with a
// new gen. So once the request ends, it settles its whole cost into the
// rebuilt month, and spent comes to the sum of the rows.
func TestBudgetRedisLossInFlight(t *testing.T) {
	u := newPricedUpstream(t)
	g := newBudgetGateway(t, mockConfigAt(t, u.url))
	body := cappedChat("gpt-4o")
	reserve, cost := pricedCosts(t, "openai", "gpt-4o", body, time.Now())

	// The lock keeps every row in the writer's queue until the end, so no
	// batch can commit inside the rebuild and be counted twice.
	unlock := g.lockRows(t)
	for k := range 2 {
		checkAdmitted(t, g.post("/openai/v1/chat/completions", body), fmt.Sprintf("request %d", k+1))
	}

	u.hold.Store(true)
	held := serveAsync(g, postAs(g.key, "/openai/v1/chat/completions", body))
	receive(t, u.arrivals, "the request at the upstream")
	if got := g.spent(t); got != 2*cost+reserve {
		t.Fatalf("spent_micros = %d with the request in flight, want %d: two costs and its reservation", got, 2*cost+reserve)
	}
	g.loseMonth(t)
	if got := g.spent(t); got != 2*cost {
		t.Fatalf("spent_micros = %d rebuilt, want %d: the two rows in the writer's queue", got, 2*cost)
	}

	u.release <- struct{}{}
	checkAdmitted(t, receive(t, held, "the held response"), "the held request")
	if got := g.spent(t); got != 3*cost {
		t.Errorf("spent_micros = %d once the request settled, want %d: its whole cost added to the rebuilt month", got, 3*cost)
	}

	unlock()
	g.closeWriter(t)
	if rows, sum := g.committed(t); rows != 3 || sum != 3*cost {
		t.Errorf("%d usage rows costing %d µ$ in all, want 3 costing %d", rows, sum, 3*cost)
	}
}

// TestBudgetFreeOnASpentBudget: once a tenant has spent its budget, a paid
// provider gets 402, and a free one, which is never budgeted, still answers.
// Its usage row costs nothing, and the spend stays where it was.
func TestBudgetFreeOnASpentBudget(t *testing.T) {
	ollama := newFakeOllama(t, ollamaKnobs{})
	u := newPricedUpstream(t)
	t.Setenv("TEST_GROQ_KEY", "sk-groq")
	g := newBudgetGateway(t, loadConfig(t, fmt.Sprintf(freeFallbackConfig, ollama.server.URL, u.url)))
	body := cappedChat("qwen/qwen3.8-27b")
	at := time.Now()
	reserve, cost := pricedCosts(t, "groq", "qwen/qwen3.8-27b", body, at)

	// Room for one request to groq, whose reservation takes spent to the
	// whole budget. Once it has settled, the budget is cut to what it cost.
	g.setBudget(t, reserve)
	g.warns[warned80] = 1
	checkAdmitted(t, g.post("/groq/v1/chat/completions", body), "the first request to groq")
	g.setBudget(t, cost)

	checkRefused(t, g.post("/groq/v1/chat/completions", body), cost, cost, reserve, at)
	checkAdmitted(t, g.post("/ollama/v1/chat/completions", cappedChat("qwen3.5:9b")), "the request to ollama")
	if n := len(ollama.arrivals); n != 1 {
		t.Errorf("ollama got %d requests, want 1", n)
	}
	if got := g.spent(t); got != cost {
		t.Errorf("spent_micros = %d, want %d, as before the request to ollama", got, cost)
	}

	g.closeWriter(t)
	if rows, sum := g.committed(t); rows != 2 || sum != cost {
		t.Errorf("%d usage rows costing %d µ$ in all, want 2 costing %d: groq's, and ollama's at 0", rows, sum, cost)
	}
}

// TestBudgetFallbackAtGroqsPrice: on the ollama → groq route, with ollama
// failing, groq's attempt reserves at groq's price for the model the fallback
// maps to, and holds that in the spend while it runs. It then settles at
// groq's cost of its usage, so the spend rises by exactly the cost on groq's
// usage row.
func TestBudgetFallbackAtGroqsPrice(t *testing.T) {
	ollama := newFakeOllama(t, ollamaKnobs{status: http.StatusInternalServerError})
	u := newPricedUpstream(t)
	t.Setenv("TEST_GROQ_KEY", "sk-groq")
	g := newBudgetGateway(t, loadConfig(t, fmt.Sprintf(freeFallbackConfig, ollama.server.URL, u.url)))
	g.warns["falling back"] = 1
	// The request the client sent, priced as groq's attempt sends it: for the
	// model qwen3.5:9b maps to.
	body := cappedChat("qwen3.5:9b")
	reserve, cost := pricedCosts(t, "groq", "qwen/qwen3.8-27b", body, time.Now())

	before := g.spent(t)
	u.hold.Store(true)
	done := serveAsync(g, postAs(g.key, "/ollama/v1/chat/completions", body))
	receive(t, u.arrivals, "groq's attempt at the upstream")
	if got := g.spent(t); got != before+reserve {
		t.Errorf("spent_micros = %d with groq's attempt in flight, want %d: its reservation at groq's price", got, before+reserve)
	}
	u.release <- struct{}{}
	rec := receive(t, done, "the response")
	checkAdmitted(t, rec, "the request")
	if rec.Header().Get("X-Gateway-Provider") != "groq" || rec.Header().Get("X-Gateway-Fallback") != "true" {
		t.Errorf("X-Gateway-Provider %q, X-Gateway-Fallback %q; want groq, true",
			rec.Header().Get("X-Gateway-Provider"), rec.Header().Get("X-Gateway-Fallback"))
	}
	after := g.spent(t)

	g.closeWriter(t)
	var groqCost int64
	if err := g.conn.QueryRow(`SELECT cost_micros FROM usage_logs WHERE tenant_id = $1 AND provider = 'groq'`,
		g.tenantID).Scan(&groqCost); err != nil {
		t.Fatalf("groq's usage row: %v", err)
	}
	if groqCost != cost || after-before != groqCost {
		t.Errorf("groq's row costs %d µ$, and spent_micros rose by %d; want both %d, groq's cost of its usage",
			groqCost, after-before, cost)
	}
	if rows, sum := g.committed(t); rows != 2 || sum != groqCost {
		t.Errorf("%d usage rows costing %d µ$ in all, want 2 costing %d: groq's, and ollama's at 0", rows, sum, groqCost)
	}
}
