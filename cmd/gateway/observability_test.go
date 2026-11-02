package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/budget"
	"github.com/grishkadel/llm-gateway/internal/cache"
	"github.com/grishkadel/llm-gateway/internal/mockprovider"
	"github.com/grishkadel/llm-gateway/internal/tenant"
)

// TestObservabilityCheckpoint is the Week 6 checkpoint: through the gateway
// with main's parts, a fixed mix of requests from two tenants, against
// upstreams whose answers are known. The gateway reports the mix three ways,
// on /metrics, in usage_logs and in its request lines, and they agree:
//   - Every counter has exactly the samples the mix makes, each set of labels
//     with its value: requests by tenant, route, the model asked of the route
//     and status; the cache's decisions; the gateway's own 429s; the
//     budgets' refusals, under the attempt's provider; and the fallbacks.
//   - Tokens and cost, by tenant, the provider that produced them and the
//     model that priced them, are the sums of the matching usage rows.
//   - Each tenant's budget gauges are its spend in the ledger, which is what
//     its rows cost, and its budget.
//   - Each request line repeats the tokens and cost of its usage row, under
//     the tenant and route its counters have.
//   - Each request on a route is timed once. Each that reached a provider's
//     transport has its overhead observed once, under its route, and each
//     attempt that got a response its TTFB, under the attempt's provider.
//   - A row the usage writer drops is counted on
//     gateway_usage_writer_dropped_total.
func TestObservabilityCheckpoint(t *testing.T) {
	mock := httptest.NewServer(mockprovider.Handler())
	t.Cleanup(mock.Close)
	ollama := newFakeOllama(t, ollamaKnobs{})
	busy := newFakeOllama(t, ollamaKnobs{status: http.StatusTooManyRequests})
	flaky := newFakeOllama(t, ollamaKnobs{status: http.StatusInternalServerError})
	t.Setenv("TEST_MOCK_KEY", "mock")
	// anthropic's upstream writes to its prompt cache at both TTLs, so the
	// mix reports every type of token.
	cfg := loadConfig(t, fmt.Sprintf(metricsConfig, mock.URL, cacheWritesUpstream(t), ollama.server.URL, busy.server.URL, flaky.server.URL))
	g := newLineGateway(t, cfg, map[string]int{
		"falling back":                            2,
		"model not priced; request refused":       1,
		"cache lookup failed; request not cached": 1,
		"usage row dropped":                       1,
	})
	// As run() does: the usage writer's two metrics, read at each scrape.
	g.metrics.UsageWriter(g.writer.Queued, g.writer.Dropped)
	ctx := context.Background()

	// acme has the default limits: 100,000 tokens a minute and $50 a month.
	acme, acmeKey := g.newTenant(t, "acme", tenant.Update{})
	// beta has one token a minute, so its first request that uses any leaves
	// its bucket in debt, and 1,000 µ$ a month, less than groq reserves for a
	// request with no output cap.
	oneToken, budgetMicros := int64(1), int64(1_000)
	beta, betaKey := g.newTenant(t, "beta", tenant.Update{RateLimitTokensPerMin: &oneToken, BudgetMicros: &budgetMicros})
	keys := map[string]string{acme: acmeKey, beta: betaKey, "none": "gw_not-a-key"}

	// A hash where the cache keeps acme's entry for this body, keyed as the
	// proxy forwards it, without the route's prefix. Redis answers the
	// lookup's GET with a WRONGTYPE error, so the lookup fails as it would
	// with Redis down, but at once. It lasts a minute, as an entry would.
	const failingLookup = `{"model":"gpt-4o","temperature":0,"messages":[{"role":"user","content":"bye"}]}`
	poisoned, ok := cache.Key(acme, "openai", httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), []byte(failingLookup))
	if !ok {
		t.Fatal("the failing lookup's body should be cacheable")
	}
	t.Cleanup(func() { g.rdb.Del(context.Background(), poisoned) })
	if err := g.rdb.HSet(ctx, poisoned, "not", "an entry").Err(); err != nil {
		t.Fatal(err)
	}
	g.rdb.Expire(ctx, poisoned, time.Minute)

	type step struct {
		what       string
		tenant     string // the tenant label; the request carries its key
		path, body string
		status     int
		model      string // gateway_requests_total's model
		cache      string // the cache's decision, "" for none
		limited    bool   // the gateway's own 429
		// The provider of the attempt a budget refused, and why.
		refusedBy, reason string
		// The providers the request was handed to, in order. Each answered.
		attempts []string
		usage    string // the model label of the answer's tokens and cost
	}
	const openai, flakyPath = "/openai/v1/chat/completions", "/flaky/v1/chat/completions"
	script := []step{
		// Input, cache reads, cache writes at both TTLs, and output.
		{what: "anthropic", tenant: acme, path: "/anthropic/v1/messages",
			body:   `{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			status: http.StatusOK, model: "claude-sonnet-4", cache: "bypass", attempts: []string{"anthropic"}, usage: "claude-sonnet-4"},
		// A dated model counts under the table's name for it.
		{what: "a dated model's stream", tenant: acme, path: openai,
			body:   `{"model":"gpt-4o-2024-08-06","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			status: http.StatusOK, model: "gpt-4o", cache: "bypass", attempts: []string{"openai"}, usage: "gpt-4o"},
		// The cache's other decisions. The hit reaches no provider, and its
		// row has no tokens and costs nothing.
		{what: "miss", tenant: acme, path: openai, body: zeroTemperature,
			status: http.StatusOK, model: "gpt-4o", cache: "miss", attempts: []string{"openai"}, usage: "gpt-4o"},
		{what: "hit", tenant: acme, path: openai, body: zeroTemperature,
			status: http.StatusOK, model: "gpt-4o", cache: "hit", usage: "gpt-4o"},
		{what: "a failed lookup", tenant: acme, path: openai, body: failingLookup,
			status: http.StatusOK, model: "gpt-4o", cache: "error", attempts: []string{"openai"}, usage: "gpt-4o"},
		// Thinking tokens, which count as output.
		{what: "gemini", tenant: acme, path: "/gemini/v1beta/models/gemini-3.8-flash:generateContent", body: `{}`,
			status: http.StatusOK, model: "gemini-3.8-flash", cache: "bypass", attempts: []string{"gemini"}, usage: "gemini-3.8-flash"},
		// A provider's 429 isn't the gateway's.
		{what: "a provider's 429", tenant: acme, path: "/busy/v1/chat/completions", body: chatBody("qwen3.5:9b"),
			status: http.StatusTooManyRequests, model: "other", cache: "bypass", attempts: []string{"busy"}, usage: "other"},
		// flaky's 500 has no tokens. groq's answer has, under the model groq
		// was asked for, while the request counts under the one asked of
		// flaky.
		{what: "fallback", tenant: acme, path: flakyPath, body: chatBody("qwen3.5:9b"),
			status: http.StatusOK, model: "other", cache: "bypass", attempts: []string{"flaky", "groq"}, usage: "qwen/qwen3.8-27b"},
		{what: "unknown model", tenant: acme, path: "/groq/v1/chat/completions", body: chatBody("llama-3.3-70b-versatile"),
			status: http.StatusBadRequest, model: "other", cache: "bypass", refusedBy: "groq", reason: "unknown_model"},
		// beta's one request to a paid provider: groq's attempt, refused once
		// flaky had failed. So beta's gauges are what the refusal found.
		{what: "fallback refused", tenant: beta, path: flakyPath, body: chatBody("qwen3.5:9b"),
			status: http.StatusPaymentRequired, model: "other", cache: "bypass", attempts: []string{"flaky"}, refusedBy: "groq", reason: "exceeded"},
		// A free provider's tokens count under other, and cost nothing. They
		// put beta's bucket in debt, so its next request is the gateway's 429.
		{what: "free", tenant: beta, path: "/ollama/v1/chat/completions", body: chatBody("qwen3.5:9b"),
			status: http.StatusOK, model: "other", cache: "bypass", attempts: []string{"ollama"}, usage: "other"},
		{what: "the gateway's 429", tenant: beta, path: openai, body: chatBody("gpt-4o"),
			status: http.StatusTooManyRequests, model: "gpt-4o", cache: "bypass", limited: true},
		// Refused before a tenant is known or the body read: tenant none, model
		// other.
		{what: "401", tenant: "none", path: openai, body: chatBody("gpt-4o"),
			status: http.StatusUnauthorized, model: "other"},
	}

	requests, decisions, limited, refused := map[string]float64{}, map[string]float64{}, map[string]float64{}, map[string]float64{}
	// Each provider's duration and TTFB series exist from the start, at 0.
	// Its overhead series appears with its first observation.
	durations, overheads, ttfbs := map[string]float64{}, map[string]float64{}, map[string]float64{}
	for _, name := range cfg.ProviderNames() {
		durations[name], ttfbs[name] = 0, 0
	}
	usageModel := map[string]string{} // by request ID
	// send sends s, and fails the test unless it gets s's status. It adds what
	// s makes of each metric, and returns its request ID.
	send := func(s step) string {
		t.Helper()
		rec := serve(g, postAs(keys[s.tenant], s.path, s.body))
		failedClosed(t, rec)
		if rec.Code != s.status {
			t.Fatalf("%s: status %d, want %d; body %s", s.what, rec.Code, s.status, rec.Body)
		}
		id := rec.Header().Get("X-Request-ID")
		usageModel[id] = s.usage

		route := strings.Split(s.path, "/")[1]
		requests[strings.Join([]string{s.tenant, route, s.model, strconv.Itoa(s.status)}, ",")]++
		durations[route]++
		if s.cache != "" {
			decisions[strings.Join([]string{s.tenant, route, s.cache}, ",")]++
		}
		if s.limited {
			limited[s.tenant+","+route]++
		}
		if s.refusedBy != "" {
			refused[strings.Join([]string{s.tenant, s.refusedBy, s.reason}, ",")]++
		}
		if len(s.attempts) > 0 {
			overheads[route]++
		}
		for _, name := range s.attempts {
			ttfbs[name]++
		}
		return id
	}

	at := time.Now()
	ids := make([]string, len(script))
	for i, s := range script {
		ids[i] = send(s)
	}
	// The writer inserts its rows in batches, off the request path: Close
	// inserts the rest. A row queued after that is dropped, so one more hit,
	// which has no tokens and costs nothing, leaves no row.
	if err := g.writer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	send(step{what: "a hit once the writer has closed", tenant: acme, path: openai, body: zeroTemperature,
		status: http.StatusOK, model: "gpt-4o", cache: "hit"})

	// The rows' tokens and cost, by tenant, provider and the model label of
	// their request's answer, and each tenant's cost. A failed attempt's row
	// has no tokens and costs nothing, so its label doesn't matter.
	tokens, cost, tenantCost := map[string]float64{}, map[string]float64{}, map[string]float64{}
	add := func(m map[string]float64, key string, n int64) {
		if n != 0 {
			m[key] += float64(n)
		}
	}
	rows, err := g.conn.Query(`SELECT request_id, tenant_id, provider, input_tokens, cached_input_tokens, cache_write_tokens,
		output_tokens, cost_micros FROM usage_logs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, tenantID, provider string
		var input, cachedInput, cacheWrite, output int64
		var micros sql.NullInt64
		if err := rows.Scan(&id, &tenantID, &provider, &input, &cachedInput, &cacheWrite, &output, &micros); err != nil {
			t.Fatal(err)
		}
		model, ok := usageModel[id]
		if !ok {
			t.Fatalf("a usage row for request %s, which the script didn't send", id)
		}
		key := strings.Join([]string{tenantID, provider, model}, ",")
		add(tokens, key+",input", input)
		add(tokens, key+",cached_input", cachedInput)
		// Both TTLs' writes, which the row keeps in one column.
		add(tokens, key+",cache_write", cacheWrite)
		add(tokens, key+",output", output)
		add(cost, key, micros.Int64)
		tenantCost[tenantID] += float64(micros.Int64)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(tokens) == 0 || len(cost) == 0 {
		t.Fatalf("no tokens (%v) or no cost (%v) in the usage rows", tokens, cost)
	}

	text := scrapeMetrics(t, g.metrics)
	checkSamples(t, "gateway_requests_total", samples(t, text, "gateway_requests_total", "tenant", "provider", "model", "status"), requests)
	checkSamples(t, "gateway_cache_requests_total", samples(t, text, "gateway_cache_requests_total", "tenant", "provider", "result"), decisions)
	checkSamples(t, "gateway_ratelimit_rejections_total", samples(t, text, "gateway_ratelimit_rejections_total", "tenant", "provider"), limited)
	checkSamples(t, "gateway_budget_rejections_total", samples(t, text, "gateway_budget_rejections_total", "tenant", "provider", "reason"), refused)
	// Both requests to flaky fell back, the refused one too.
	checkSamples(t, "gateway_fallbacks_total", samples(t, text, "gateway_fallbacks_total", "from", "to", "reason"),
		map[string]float64{"flaky,groq,server_error": 2})

	// The scrape's cache_write_1h is added to its cache_write to compare with
	// the rows, which keep the two in one column. The 1-hour share is
	// cacheWritesUpstream's: 200 of the 300 tokens it writes.
	scraped := samples(t, text, "gateway_tokens_total", "tenant", "provider", "model", "type")
	merged, oneHour := map[string]float64{}, map[string]float64{}
	for k, v := range scraped {
		merged[strings.TrimSuffix(k, "_1h")] += v
		if strings.HasSuffix(k, ",cache_write_1h") {
			oneHour[k] = v
		}
	}
	checkSamples(t, "gateway_tokens_total, cache_write_1h counted as cache_write", merged, tokens)
	checkSamples(t, "gateway_tokens_total's cache_write_1h", oneHour, map[string]float64{acme + ",anthropic,claude-sonnet-4,cache_write_1h": 200})
	checkSamples(t, "gateway_cost_micros_total", samples(t, text, "gateway_cost_micros_total", "tenant", "provider", "model"), cost)

	// With no request in flight, a tenant's spend in the ledger is what its
	// rows cost, and its gauges are that spend and its budget, as its latest
	// request to a paid provider found them.
	ledger := budget.New(g.rdb, g.conn, g.writer.Pending)
	spent, budgets := map[string]float64{}, map[string]float64{}
	for _, id := range []string{acme, beta} {
		micros, err := ledger.Spent(ctx, id, at)
		if err != nil {
			t.Fatalf("Spent(%s): %v", id, err)
		}
		tn, err := g.store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		spent[id], budgets[id] = float64(micros), float64(tn.BudgetMicros)
	}
	checkSamples(t, "Ledger.Spent, against the rows' cost by tenant", spent, tenantCost)
	checkSamples(t, "gateway_budget_spent_micros", samples(t, text, "gateway_budget_spent_micros", "tenant"), spent)
	checkSamples(t, "gateway_budget_micros", samples(t, text, "gateway_budget_micros", "tenant"), budgets)

	_, durationCounts := timing(t, text, "gateway_request_duration_seconds")
	_, overheadCounts := timing(t, text, "gateway_overhead_seconds")
	_, ttfbCounts := timing(t, text, "gateway_upstream_ttfb_seconds")
	checkSamples(t, "gateway_request_duration_seconds_count", durationCounts, durations)
	checkSamples(t, "gateway_overhead_seconds_count", overheadCounts, overheads)
	checkSamples(t, "gateway_upstream_ttfb_seconds_count", ttfbCounts, ttfbs)

	// The writer's queue is empty once it has closed, and the hit after that
	// is the one row it dropped.
	for _, want := range []string{"gateway_usage_writer_queue_depth 0", "gateway_usage_writer_dropped_total 1"} {
		if !strings.Contains(text, "\n"+want+"\n") {
			t.Errorf("/metrics has no line %q", want)
		}
	}

	// Each line names the tenant and route its counters have, and repeats the
	// usage row of the response the client got: a hit's, under its route,
	// else the last attempt's, unless a budget refused the request after it.
	for i, s := range script {
		route := strings.Split(s.path, "/")[1]
		want := map[string]any{"provider": route}
		if s.tenant != "none" {
			// Once the body is read, the line says whether it asked for a
			// stream. A row's streamed replaces this.
			want["tenant_id"], want["streamed"] = s.tenant, false
		}
		answered := ""
		switch {
		case s.cache == "hit":
			answered = route
		case len(s.attempts) > 0 && s.refusedBy == "":
			answered = s.attempts[len(s.attempts)-1]
		}
		if answered != "" {
			want = with(want, g.row(t, ids[i], answered).fields())
			if answered != route {
				want["fallback_provider"] = answered
			}
		}
		got := fromRecord(g.line(t, ids[i]))
		// The model as the client named it, which TestRequestLine checks.
		delete(got, "model")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the line's fields from the Record = %v\nwant %v", s.what, got, want)
		}
	}
}
