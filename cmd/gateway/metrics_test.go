package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/grishkadel/llm-gateway/internal/middleware"
	"github.com/grishkadel/llm-gateway/internal/mockprovider"
	"github.com/grishkadel/llm-gateway/internal/tenant"
)

// labelPair matches one label on a /metrics line: name="value".
var labelPair = regexp.MustCompile(`(\w+)="([^"]*)"`)

// samples returns the samples of the metric name in text, a scrape of
// /metrics, each under its label values in the order of labels, joined by
// commas. A sample with other labels fails the test.
func samples(t *testing.T, text, name string, labels ...string) map[string]float64 {
	t.Helper()
	got := map[string]float64{}
	for line := range strings.Lines(text) {
		rest, ok := strings.CutPrefix(line, name+"{")
		if !ok {
			continue
		}
		pairs, value, _ := strings.Cut(rest, "} ")
		found := map[string]string{}
		for _, m := range labelPair.FindAllStringSubmatch(pairs, -1) {
			found[m[1]] = m[2]
		}
		key := make([]string, len(labels))
		for i, l := range labels {
			key[i] = found[l]
		}
		if len(found) != len(labels) {
			t.Errorf("%s has labels %v, want %v", name, found, labels)
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("%s: %v", strings.TrimSpace(line), err)
		}
		got[strings.Join(key, ",")] = v
	}
	return got
}

// checkSamples fails the test unless got, a metric's samples, are want.
func checkSamples(t *testing.T, name string, got, want map[string]float64) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Errorf("%s:\n got %v\nwant %v", name, got, want)
	}
}

// metricsConfig is a config file with a route for each format: openai, groq
// and gemini to the mock at the URL in its first argument, and anthropic to
// its second. Three more are free: ollama to the URL in its third, busy, an
// Ollama answering 429, to its fourth, and flaky, an Ollama answering 500,
// to its fifth, falling back to groq for qwen3.5:9b.
const metricsConfig = `
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
  gemini:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: x-goog-api-key
    format: gemini
  ollama:
    url: %[3]s
    auth: none
    format: openai
    free: true
  busy:
    url: %[4]s
    auth: none
    format: openai
    free: true
  flaky:
    url: %[5]s
    auth: none
    format: openai
    free: true
    fallback:
      enabled: true
      provider: groq
      models:
        "qwen3.5:9b": qwen/qwen3.8-27b
`

// aliasUpstream starts a provider in the Anthropic format whose every answer
// names a model the price table doesn't know, claude-sonnet-4-latest, so the
// gateway prices and labels it by the model the request asked for. It
// returns its URL.
func aliasUpstream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_alias","type":"message","role":"assistant","model":"claude-sonnet-4-latest",`+
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":30,"output_tokens":50}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestMetricsThroughGatewayExactly: through the gateway with main's parts, a
// script of requests, and every sample of the counters it moves.
//   - Requests count under the route, the tenant (none for a 401) and the
//     table's name for the model asked of the route, or other.
//   - The cache's decisions, the gateway's own 429s and the budgets'
//     refusals count once each, under the tenant. A refusal names the
//     attempt's provider, after a fallback the fallback's.
//   - Tokens and cost, by tenant, the provider that produced them and the
//     model that priced them, are the sums of the matching usage rows.
func TestMetricsThroughGatewayExactly(t *testing.T) {
	mock := httptest.NewServer(mockprovider.Handler())
	t.Cleanup(mock.Close)
	ollama := newFakeOllama(t, ollamaKnobs{})
	busy := newFakeOllama(t, ollamaKnobs{status: http.StatusTooManyRequests})
	flaky := newFakeOllama(t, ollamaKnobs{status: http.StatusInternalServerError})
	t.Setenv("TEST_MOCK_KEY", "mock")
	g := newLineGateway(t, loadConfig(t, fmt.Sprintf(metricsConfig, mock.URL, aliasUpstream(t), ollama.server.URL, busy.server.URL, flaky.server.URL)),
		map[string]int{"falling back": 2, "model not priced; request refused": 1})

	acme, acmeKey := g.newTenant(t, "acme", tenant.Update{})
	// One token a minute: its first request leaves its bucket in debt.
	oneToken := int64(1)
	beta, betaKey := g.newTenant(t, "beta", tenant.Update{RateLimitTokensPerMin: &oneToken})
	var nothing int64
	broke, brokeKey := g.newTenant(t, "broke", tenant.Update{BudgetMicros: &nothing})
	keys := map[string]string{acme: acmeKey, beta: betaKey, broke: brokeKey, "none": "gw_not-a-key"}

	stream := func(model string) string {
		return `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	}
	const openai, groq, flakyPath = "/openai/v1/chat/completions", "/groq/v1/chat/completions", "/flaky/v1/chat/completions"
	script := []struct {
		what       string
		tenant     string // the tenant label; the request carries its key
		path, body string
		cacheMode  string // X-Gateway-Cache to send, if any
		status     int
		model      string // gateway_requests_total's model
		cache      string // the cache's decision, "" for none
		limited    bool   // the gateway's own 429
		// The provider of the attempt a budget refused, and why.
		refusedBy, reason string
		usage             string // the model label of the request's usage rows
	}{
		// The answer names a model the table doesn't know: its usage counts
		// under the one asked for.
		{what: "anthropic", tenant: acme, path: "/anthropic/v1/messages",
			body:   `{"model":"claude-sonnet-4-20250514","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			status: http.StatusOK, model: "claude-sonnet-4", cache: "bypass", usage: "claude-sonnet-4"},
		{what: "a dated model's stream", tenant: acme, path: openai, body: stream("gpt-4o-2024-08-06"),
			status: http.StatusOK, model: "gpt-4o", cache: "bypass", usage: "gpt-4o"},
		{what: "miss", tenant: acme, path: openai, body: zeroTemperature,
			status: http.StatusOK, model: "gpt-4o", cache: "miss", usage: "gpt-4o"},
		{what: "hit", tenant: acme, path: openai, body: zeroTemperature,
			status: http.StatusOK, model: "gpt-4o", cache: "hit", usage: "gpt-4o"},
		{what: "a snapshot priced on its own", tenant: acme, path: openai, body: chatBody("gpt-4o-2024-05-13"),
			status: http.StatusOK, model: "gpt-4o-2024-05-13", cache: "bypass", usage: "gpt-4o-2024-05-13"},
		{what: "gemini's stream", tenant: acme, path: "/gemini/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse", body: `{}`,
			status: http.StatusOK, model: "gemini-3.8-flash", cache: "bypass", usage: "gemini-3.8-flash"},
		{what: "ollama", tenant: acme, path: "/ollama/v1/chat/completions", body: chatBody("qwen3.5:9b"),
			status: http.StatusOK, model: "other", cache: "bypass", usage: "other"},
		// flaky's 500 has no tokens; groq's answer has, under its model.
		{what: "fallback", tenant: acme, path: flakyPath, body: chatBody("qwen3.5:9b"),
			status: http.StatusOK, model: "other", cache: "bypass", usage: "qwen/qwen3.8-27b"},
		{what: "a provider's 429", tenant: acme, path: "/busy/v1/chat/completions", body: chatBody("qwen3.5:9b"),
			status: http.StatusTooManyRequests, model: "other", cache: "bypass", usage: "other"},
		{what: "a bad X-Gateway-Cache", tenant: acme, path: openai, body: chatBody("gpt-4o"), cacheMode: "maybe",
			status: http.StatusBadRequest, model: "gpt-4o"},
		{what: "unknown model", tenant: acme, path: groq, body: chatBody("llama-3.3-70b-versatile"),
			status: http.StatusBadRequest, model: "other", cache: "bypass", refusedBy: "groq", reason: "unknown_model"},
		{what: "beta's first", tenant: beta, path: groq, body: stream("qwen/qwen3.8-27b"),
			status: http.StatusOK, model: "qwen/qwen3.8-27b", cache: "bypass", usage: "qwen/qwen3.8-27b"},
		{what: "the gateway's 429", tenant: beta, path: openai, body: chatBody("gpt-4o"),
			status: http.StatusTooManyRequests, model: "gpt-4o", cache: "bypass", limited: true},
		{what: "402", tenant: broke, path: openai, body: chatBody("gpt-4o"),
			status: http.StatusPaymentRequired, model: "gpt-4o", cache: "bypass", refusedBy: "openai", reason: "exceeded"},
		{what: "fallback refused", tenant: broke, path: flakyPath, body: chatBody("qwen3.5:9b"),
			status: http.StatusPaymentRequired, model: "other", cache: "bypass", refusedBy: "groq", reason: "exceeded"},
		{what: "401", tenant: "none", path: openai, body: chatBody("gpt-4o"),
			status: http.StatusUnauthorized, model: "other"},
	}

	requests, cache, limited, refused := map[string]float64{}, map[string]float64{}, map[string]float64{}, map[string]float64{}
	usageModel := map[string]string{} // by request ID
	for _, s := range script {
		req := postAs(keys[s.tenant], s.path, s.body)
		if s.cacheMode != "" {
			req.Header.Set(middleware.HeaderCache, s.cacheMode)
		}
		rec := serve(g, req)
		failedClosed(t, rec)
		if rec.Code != s.status {
			t.Fatalf("%s: status %d, want %d; body %s", s.what, rec.Code, s.status, rec.Body)
		}
		usageModel[rec.Header().Get("X-Request-ID")] = s.usage

		route := strings.Split(s.path, "/")[1]
		requests[strings.Join([]string{s.tenant, route, s.model, strconv.Itoa(s.status)}, ",")]++
		if s.cache != "" {
			cache[strings.Join([]string{s.tenant, route, s.cache}, ",")]++
		}
		if s.limited {
			limited[s.tenant+","+route]++
		}
		if s.refusedBy != "" {
			refused[strings.Join([]string{s.tenant, s.refusedBy, s.reason}, ",")]++
		}
	}

	// The writer inserts its rows in batches, off the request path: Close
	// inserts the rest.
	if err := g.writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	tokens, cost := map[string]float64{}, map[string]float64{}
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
		// The mock reports no 1-hour writes, the row's other half.
		add(tokens, key+",cache_write", cacheWrite)
		add(tokens, key+",output", output)
		add(cost, key, micros.Int64)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(tokens) == 0 || len(cost) == 0 {
		t.Fatalf("no tokens (%v) or no cost (%v) in the usage rows", tokens, cost)
	}

	got := scrapeMetrics(t, g)
	checkSamples(t, "gateway_requests_total", samples(t, got, "gateway_requests_total", "tenant", "provider", "model", "status"), requests)
	checkSamples(t, "gateway_cache_requests_total", samples(t, got, "gateway_cache_requests_total", "tenant", "provider", "result"), cache)
	checkSamples(t, "gateway_ratelimit_rejections_total", samples(t, got, "gateway_ratelimit_rejections_total", "tenant", "provider"), limited)
	checkSamples(t, "gateway_budget_rejections_total", samples(t, got, "gateway_budget_rejections_total", "tenant", "provider", "reason"), refused)
	checkSamples(t, "gateway_tokens_total", samples(t, got, "gateway_tokens_total", "tenant", "provider", "model", "type"), tokens)
	checkSamples(t, "gateway_cost_micros_total", samples(t, got, "gateway_cost_micros_total", "tenant", "provider", "model"), cost)
}

// TestBudgetGaugesThroughGateway: through the gateway with main's parts, the
// budget gauges show the tenant's spend and budget as its latest request to a
// paid provider found them in the ledger:
//   - Requests to free providers, answered or not, set neither gauge.
//   - Once a paid request has settled, the spend is the ledger's, after a
//     fallback too, whose attempt settles last.
//   - After a 402, the spend is the 402's spent_micros.
//   - The budget is the tenant's, and a new one shows from the next request.
func TestBudgetGaugesThroughGateway(t *testing.T) {
	mock := httptest.NewServer(mockprovider.Handler())
	t.Cleanup(mock.Close)
	ollama := newFakeOllama(t, ollamaKnobs{})
	busy := newFakeOllama(t, ollamaKnobs{status: http.StatusTooManyRequests})
	flaky := newFakeOllama(t, ollamaKnobs{status: http.StatusInternalServerError})
	t.Setenv("TEST_MOCK_KEY", "mock")
	g := newBudgetGateway(t, loadConfig(t, fmt.Sprintf(metricsConfig, mock.URL, aliasUpstream(t), ollama.server.URL, busy.server.URL, flaky.server.URL)))
	g.warns["falling back"] = 1

	// checkGauges fails the test unless the gauges' samples, scraped after
	// what, are spent and budget.
	checkGauges := func(t *testing.T, what string, spent, budget map[string]float64) {
		t.Helper()
		got := scrapeMetrics(t, g)
		checkSamples(t, "gateway_budget_spent_micros after "+what, samples(t, got, "gateway_budget_spent_micros", "tenant"), spent)
		checkSamples(t, "gateway_budget_micros after "+what, samples(t, got, "gateway_budget_micros", "tenant"), budget)
	}
	tenantAt := func(micros float64) map[string]float64 { return map[string]float64{g.tenantID: micros} }

	checkAdmitted(t, g.post("/ollama/v1/chat/completions", chatBody("qwen3.5:9b")), "the request to ollama")
	if rec := g.post("/busy/v1/chat/completions", chatBody("qwen3.5:9b")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the request to busy: status %d, want its 429; body %s", rec.Code, rec.Body)
	}
	checkGauges(t, "requests to free providers", nil, nil)

	checkAdmitted(t, g.post("/openai/v1/chat/completions", chatBody("gpt-4o")), "the request to openai")
	paid := g.spent(t)
	checkGauges(t, "a paid request", tenantAt(float64(paid)), tenantAt(fiftyDollars))

	checkAdmitted(t, g.post("/flaky/v1/chat/completions", chatBody("qwen3.5:9b")), "the fallback to groq")
	fellBack := g.spent(t)
	if fellBack == paid {
		t.Fatalf("spent_micros = %d after groq's answer, as before it: the test needs an answer that costs something", fellBack)
	}
	checkGauges(t, "a fallback to groq", tenantAt(float64(fellBack)), tenantAt(fiftyDollars))

	// 1 µ$ is less than any reservation, so the next request is refused.
	g.setBudget(t, 1)
	refusal := overBudget(t, g.post("/openai/v1/chat/completions", chatBody("gpt-4o")))
	checkGauges(t, "a 402", tenantAt(refusal["spent_micros"].(float64)), tenantAt(1))
}

// TestMetricsRandomModels: 100 requests for models of random names, on each
// format's route, add no model label but other, so clients can't grow the
// number of series.
func TestMetricsRandomModels(t *testing.T) {
	r, _ := newMockRouter(t)
	routes := []string{"openai", "groq", "anthropic", "gemini"}
	for i := range 100 {
		model := fmt.Sprintf("model-%x", rand.Uint64())
		if i%2 == 1 {
			// With a date, which the table's names are looked up without.
			model += "-2024-08-06"
		}
		route := routes[i%len(routes)]
		path, body := "/"+route+"/v1/chat/completions", chatBody(model)
		switch route {
		case "anthropic":
			path, body = "/anthropic/v1/messages", `{"model":"`+model+`","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
		case "gemini":
			path, body = "/gemini/v1beta/models/"+model+":generateContent", `{}`
		}
		if rec := serve(r, tenantPost(path, body)); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d; body %s", path, rec.Code, rec.Body)
		}
	}

	got := scrapeMetrics(t, r)
	for _, m := range regexp.MustCompile(`model="[^"]*"`).FindAllString(got, -1) {
		if m != `model="other"` {
			t.Errorf("a series with %s", m)
		}
	}
	checkSamples(t, "gateway_requests_total", samples(t, got, "gateway_requests_total", "tenant", "provider", "model", "status"),
		map[string]float64{"tn_test,openai,other,200": 25, "tn_test,groq,other,200": 25, "tn_test,anthropic,other,200": 25, "tn_test,gemini,other,200": 25})
	// Their cost isn't known, so none is counted.
	if s := samples(t, got, "gateway_cost_micros_total", "tenant", "provider", "model"); len(s) != 0 {
		t.Errorf("gateway_cost_micros_total = %v, want no samples", s)
	}
}

// chatBody is a chat request's body for model.
func chatBody(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
}
