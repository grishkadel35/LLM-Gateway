package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/budget"
	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
	"github.com/grishkadel/llm-gateway/internal/metrics"
	"github.com/grishkadel/llm-gateway/internal/middleware"
	"github.com/grishkadel/llm-gateway/internal/mockprovider"
	"github.com/grishkadel/llm-gateway/internal/pricing"
	"github.com/grishkadel/llm-gateway/internal/proxy"
	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// fakeLedger stands in for the Redis ledger, so routing tests need no Redis.
// Each Reserve fails with err when it is set, is refused at spent when refuse
// is, and is admitted otherwise. Each Settle reports spent plus its charge. It
// records each Reserve and Settle, under a lock, so concurrent requests can
// share it. Spent reports spent, or fails with err when it is set, and
// records nothing.
type fakeLedger struct {
	err    error
	refuse bool
	spent  int64
	// started, when set, makes each Reserve send on it as it starts, and then
	// wait for its request's context to be done before it answers: a client
	// that leaves while the ledger works. It fails with err when that is set,
	// and with the context's error when it isn't.
	started chan struct{}

	mu      sync.Mutex
	calls   []reserveCall
	settles []settleCall
}

// reserveCall is one Reserve a fakeLedger received.
type reserveCall struct {
	tenantID       string
	budget, amount int64
	at             time.Time
}

func (f *fakeLedger) Reserve(ctx context.Context, tenantID string, budgetMicros, amount int64, at time.Time) (budget.Reservation, error) {
	if f.started != nil {
		f.started <- struct{}{}
		<-ctx.Done()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, reserveCall{tenantID, budgetMicros, amount, at})
	switch {
	case f.err != nil:
		return budget.Reservation{}, f.err
	case f.started != nil:
		return budget.Reservation{}, ctx.Err()
	case f.refuse:
		return budget.Reservation{Spent: f.spent, Amount: amount}, nil
	}
	return budget.Reservation{Admitted: true, Spent: f.spent + amount, Amount: amount}, nil
}

// settleCall is one Settle a fakeLedger received.
type settleCall struct {
	res    budget.Reservation
	charge int64
	// requestID is the request ID its context carried.
	requestID string
}

// Settle succeeds.
func (f *fakeLedger) Settle(ctx context.Context, r budget.Reservation, charge int64) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settles = append(f.settles, settleCall{r, charge, middleware.RequestIDFrom(ctx)})
	return f.spent + charge, true, nil
}

func (f *fakeLedger) Spent(context.Context, string, time.Time) (int64, error) {
	return f.spent, f.err
}

// recorded returns the Reserve calls so far.
func (f *fakeLedger) recorded() []reserveCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// settled returns the Settle calls so far.
func (f *fakeLedger) settled() []settleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.settles)
}

// budgetedStore is fakeStore with its tenant's monthly budget set to micros.
type budgetedStore struct {
	fakeStore
	micros int64
}

func (s budgetedStore) Lookup(ctx context.Context, key string) (tenant.Tenant, tenant.APIKey, error) {
	tn, k, err := s.fakeStore.Lookup(ctx, key)
	tn.BudgetMicros = s.micros
	return tn, k, err
}

// fiftyDollars is the schema's default budget, $50.
const fiftyDollars = 50_000_000

// loadConfig loads a config file's text.
func loadConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load() returned error: %v", err)
	}
	return cfg
}

// budgetRouter builds the gateway from cfg over store, budgeted by ledger,
// and rate limited by limiter and caching in cacheStore unless they are nil.
func budgetRouter(t *testing.T, cfg *config.Config, store tenantStore, ledger budgetLedger, limiter middleware.Limiter, cacheStore middleware.CacheStore) http.Handler {
	t.Helper()
	r, err := router(cfg, store, testAdminKey, nil, limiter, cacheStore, ledger, metrics.New(), nil, discardLogger)
	if err != nil {
		t.Fatalf("router() returned error: %v", err)
	}
	return r
}

// reserveTokens is the budget's input estimate for a text-only body: a token
// per 3 bytes, rounded up.
func reserveTokens(body string) int64 { return int64(len(body)+2) / 3 }

// checkReserve fails the test unless calls is one Reserve for fakeStore's
// tenant against budgetMicros, of what pricing.Reserve prices the request at
// on provider for model, with body's input, outputCap and choices, at a time
// from before to after.
func checkReserve(t *testing.T, calls []reserveCall, budgetMicros int64, provider, model, body string, outputCap, choices int64, before, after time.Time) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("Reserve calls = %+v, want one", calls)
	}
	c := calls[0]
	if c.at.Before(before) || c.at.After(after) {
		t.Errorf("Reserve at %v, want from %v to %v", c.at, before, after)
	}
	amount, ok := pricing.Reserve(provider, model, reserveTokens(body), outputCap, choices, c.at)
	if !ok {
		t.Fatalf("%s/%s should be priced", provider, model)
	}
	if c.tenantID != "tn_test" || c.budget != budgetMicros || c.amount != amount {
		t.Errorf("Reserve(%q, budget %d, amount %d), want Reserve(%q, budget %d, amount %d)",
			c.tenantID, c.budget, c.amount, "tn_test", budgetMicros, amount)
	}
}

// TestBudgetReservesEachPaidRequest: a request to a paid provider reserves,
// once, exactly what pricing.Reserve prices it at for its provider and model,
// from its body's input, its output cap and its choices, and then reaches the
// provider.
func TestBudgetReservesEachPaidRequest(t *testing.T) {
	cases := []struct {
		name, provider, model, path, body string
		outputCap, choices                int64
	}{
		{"openai", "openai", "gpt-4o", "/openai/v1/chat/completions",
			`{"model":"gpt-4o","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`, 100, 1},
		{"groq, two choices", "groq", "openai/gpt-oss-120b", "/groq/v1/chat/completions",
			`{"model":"openai/gpt-oss-120b","n":2,"max_completion_tokens":50,"messages":[{"role":"user","content":"hello"}]}`, 100, 2},
		{"anthropic", "anthropic", "claude-sonnet-4", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`, 64, 1},
		{"gemini, the model in the path", "gemini", "gemini-3.8-flash", "/gemini/v1beta/models/gemini-3.8-flash:generateContent",
			`{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":200}}`, 200, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
			ledger := &fakeLedger{}
			r := budgetRouter(t, mockConfigAt(t, upstream), budgetedStore{micros: fiftyDollars}, ledger, nil, nil)

			before := time.Now()
			rec := serve(r, tenantPost(tc.path, tc.body))
			after := time.Now()

			if rec.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("status %d, provider calls %d; want 200, 1; body %s", rec.Code, calls.Load(), rec.Body)
			}
			checkReserve(t, ledger.recorded(), fiftyDollars, tc.provider, tc.model, tc.body, tc.outputCap, tc.choices, before, after)
		})
	}
}

// TestBudgetReservesUpToMaxOutput: a request with no output cap reserves the
// model's maximum output for each choice it asks for, and so does one whose
// cap is above that maximum.
func TestBudgetReservesUpToMaxOutput(t *testing.T) {
	cases := []struct {
		name, provider, model, path, body string
		// maxOutput is the model's MaxOutput times the choices.
		maxOutput, choices int64
	}{
		{"gpt-4o, no cap", "openai", "gpt-4o", "/openai/v1/chat/completions",
			`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`, 16_384, 1},
		{"gpt-4o, a cap above it", "openai", "gpt-4o", "/openai/v1/chat/completions",
			`{"model":"gpt-4o","max_tokens":1000000,"messages":[{"role":"user","content":"hello"}]}`, 16_384, 1},
		{"gpt-4o, no cap, two choices", "openai", "gpt-4o", "/openai/v1/chat/completions",
			`{"model":"gpt-4o","n":2,"messages":[{"role":"user","content":"hello"}]}`, 2 * 16_384, 2},
		{"claude-sonnet-4, no cap", "anthropic", "claude-sonnet-4", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}]}`, 64_000, 1},
		{"gemini-3.8-flash, no cap", "gemini", "gemini-3.8-flash", "/gemini/v1beta/models/gemini-3.8-flash:generateContent",
			`{"contents":[{"parts":[{"text":"hello"}]}]}`, 65_536, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &fakeLedger{}
			r := budgetRouter(t, mockConfig(t), budgetedStore{micros: fiftyDollars}, ledger, nil, nil)

			before := time.Now()
			rec := serve(r, tenantPost(tc.path, tc.body))
			after := time.Now()

			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200; body %s", rec.Code, rec.Body)
			}
			checkReserve(t, ledger.recorded(), fiftyDollars, tc.provider, tc.model, tc.body, tc.maxOutput, tc.choices, before, after)
		})
	}
}

// TestBudgetSkipsFreeProviders: a free provider's request reaches it with no
// Reserve, even while the ledger refuses everything or fails.
func TestBudgetSkipsFreeProviders(t *testing.T) {
	cases := []struct {
		name   string
		ledger *fakeLedger
	}{
		{"ledger refusing", &fakeLedger{refuse: true}},
		{"ledger failing", &fakeLedger{err: errors.New("redis: connection refused")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ollama := newFakeOllama(t, ollamaKnobs{})
			cfg := loadConfig(t, fmt.Sprintf(ollamaConfig+"    free: true\n", ollama.server.URL))
			r := budgetRouter(t, cfg, budgetedStore{micros: fiftyDollars}, tc.ledger, nil, nil)

			if rec := serve(r, chatRequest("ollama", "qwen3.5:9b")); rec.Code != http.StatusOK || len(ollama.arrivals) != 1 {
				t.Errorf("status %d, Ollama got %d requests; want 200, 1", rec.Code, len(ollama.arrivals))
			}
			if calls := tc.ledger.recorded(); len(calls) != 0 {
				t.Errorf("Reserve calls = %+v, want none", calls)
			}
		})
	}
}

// overBudget returns the error in rec, failing the test unless rec is the
// gateway's 402.
func overBudget(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Error map[string]any `json:"error"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusPaymentRequired || err != nil || body.Error["type"] != "budget_exceeded" {
		t.Fatalf("status %d, want 402 budget_exceeded; body %s", rec.Code, rec.Body)
	}
	return body.Error
}

// TestBudgetRefusalThroughGateway: a request the ledger refuses gets 402 with
// every field, the reservation and the period priced as the request arrived,
// and a message that says what would help. The provider never sees it, and
// the rate limiter gets its whole charge back.
func TestBudgetRefusalThroughGateway(t *testing.T) {
	// No cap: priced at gpt-4o's maximum output, 16,384, about $0.16.
	const body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	cases := []struct {
		name          string
		budget, spent int64
		// says is a part of the message that only its case has.
		says string
	}{
		{"spent: the budget resets", fiftyDollars, fiftyDollars - 4_000, "left of the gateway's $50.00 monthly budget"},
		{"over the whole budget: a lower output cap", 100_000, 0, "set a lower output cap"},
		{"budget 0: free providers only", 0, 0, "free providers only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
			ledger := &fakeLedger{refuse: true, spent: tc.spent}
			limiter := &fakeLimiter{allowed: true}
			r := budgetRouter(t, mockConfigAt(t, upstream), budgetedStore{micros: tc.budget}, ledger, limiter, nil)

			before := time.Now()
			rec := serve(r, openaiChat(body))
			after := time.Now()

			got := overBudget(t, rec)
			if calls.Load() != 0 {
				t.Errorf("the provider got %d requests, want none", calls.Load())
			}
			if v := rec.Header().Values("Retry-After"); v != nil {
				t.Errorf("Retry-After = %q, want none", v)
			}
			reserves := ledger.recorded()
			checkReserve(t, reserves, tc.budget, "openai", "gpt-4o", body, 0, 1, before, after)
			if allows, adjusts := limiter.recorded(); len(allows) != 1 || len(adjusts) != 1 || adjusts[0].delta != allows[0].cost {
				t.Errorf("Allow calls %+v, Adjust calls %+v; want one of each, refunding the whole charge", allows, adjusts)
			}

			msg, _ := got["message"].(string)
			if !strings.Contains(msg, tc.says) {
				t.Errorf("message %q doesn't say %q", msg, tc.says)
			}
			delete(got, "message")
			start, end := budget.Period(reserves[0].at)
			want := map[string]any{
				"type":             "budget_exceeded",
				"budget_micros":    float64(tc.budget),
				"spent_micros":     float64(tc.spent),
				"remaining_micros": float64(tc.budget - tc.spent),
				"reserve_micros":   float64(reserves[0].amount),
				"period":           start.Format("2006-01"),
				"resets_at":        end.Format(time.RFC3339),
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("error = %v\nwant %v", got, want)
			}
		})
	}
}

// TestBudgetUnpricedModelThroughGateway: a model the gateway has no price for
// on its provider, or no model at all, gets 400 unknown_model, naming both.
// Nothing is reserved, and the provider never sees it.
func TestBudgetUnpricedModelThroughGateway(t *testing.T) {
	cases := []struct {
		name, path, body, provider, model string
	}{
		{"Groq's Llama", "/groq/v1/chat/completions", `{"model":"llama-3.3-70b-versatile","messages":[]}`, "groq", "llama-3.3-70b-versatile"},
		{"a Gemini alias", "/gemini/v1beta/models/gemini-flash-latest:generateContent", `{"contents":[]}`, "gemini", "gemini-flash-latest"},
		{"no model", "/openai/v1/chat/completions", `{"messages":[]}`, "openai", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
			ledger := &fakeLedger{}
			r := budgetRouter(t, mockConfigAt(t, upstream), budgetedStore{micros: fiftyDollars}, ledger, nil, nil)

			rec := serve(r, tenantPost(tc.path, tc.body))
			var got struct {
				Error struct{ Type, Provider, Model string } `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body %s: %v", rec.Body, err)
			}
			if e := got.Error; rec.Code != http.StatusBadRequest || e.Type != "unknown_model" || e.Provider != tc.provider || e.Model != tc.model {
				t.Errorf("status %d, error %+v; want 400 unknown_model naming %s and %q", rec.Code, e, tc.provider, tc.model)
			}
			if reserves := ledger.recorded(); len(reserves) != 0 || calls.Load() != 0 {
				t.Errorf("Reserve calls %+v, provider calls %d; want neither", reserves, calls.Load())
			}
		})
	}
}

// TestBudgetFailsClosedThroughGateway: when the ledger fails, a paid request
// gets 503 budget_unavailable, and the provider never sees it.
func TestBudgetFailsClosedThroughGateway(t *testing.T) {
	upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
	ledger := &fakeLedger{err: errors.New("redis: i/o timeout")}
	r := budgetRouter(t, mockConfigAt(t, upstream), budgetedStore{micros: fiftyDollars}, ledger, nil, nil)

	rec := serve(r, openaiChat(`{"model":"gpt-4o","messages":[]}`))
	var got struct {
		Error struct{ Type string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if rec.Code != http.StatusServiceUnavailable || got.Error.Type != "budget_unavailable" {
		t.Errorf("status %d, error type %q; want 503 budget_unavailable", rec.Code, got.Error.Type)
	}
	if reserves := ledger.recorded(); len(reserves) != 1 || calls.Load() != 0 {
		t.Errorf("Reserve calls %+v, provider calls %d; want one Reserve and no provider call", reserves, calls.Load())
	}
}

// TestBudgetReserveFailsThroughGateway: when Reserve fails, the answer depends
// on whom the failure is down to. A client that leaves while Reserve runs gets
// 499 with no body, and one Info line from Budget with no Error beside it. A
// ledger that fails, or times out (an error wrapping context.DeadlineExceeded,
// with the request's own context still live), gets 503 and an Error line.
// Either way the provider never sees the request, nothing is settled, and the
// rate limiter gets its whole charge back.
func TestBudgetReserveFailsThroughGateway(t *testing.T) {
	cases := []struct {
		name string
		// ledger fails Reserve. If it has a started channel, the client leaves
		// once Reserve has started.
		ledger     *fakeLedger
		status     int
		level, msg string
	}{
		{"client leaves", &fakeLedger{started: make(chan struct{}, 1)}, proxy.StatusClientClosedRequest,
			"INFO", "client closed request during budget check"},
		{"ledger error", &fakeLedger{err: errors.New("redis: connection refused")}, http.StatusServiceUnavailable,
			"ERROR", "budget check failed; request refused"},
		{"ledger timeout", &fakeLedger{err: fmt.Errorf("budget: rebuilding: %w", context.DeadlineExceeded)}, http.StatusServiceUnavailable,
			"ERROR", "budget check failed; request refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
			var logs lockedBuffer
			limiter := &fakeLimiter{allowed: true}
			r, err := router(mockConfigAt(t, upstream), budgetedStore{micros: fiftyDollars}, testAdminKey, nil, limiter, nil, tc.ledger,
				metrics.New(), nil, slog.New(slog.NewJSONHandler(&logs, nil)))
			if err != nil {
				t.Fatalf("router() returned error: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := serveAsync(r, openaiChat(`{"model":"gpt-4o","messages":[]}`).WithContext(ctx))
			if tc.ledger.started != nil {
				receive(t, tc.ledger.started, "Reserve to start")
				cancel()
			}
			rec := receive(t, done, "the response")

			if rec.Code != tc.status || tc.status == proxy.StatusClientClosedRequest && rec.Body.Len() != 0 {
				t.Errorf("status %d, body %q; want %d, and no body for a client that left", rec.Code, rec.Body, tc.status)
			}
			if reserves, settles := tc.ledger.recorded(), tc.ledger.settled(); len(reserves) != 1 || len(settles) != 0 || calls.Load() != 0 {
				t.Errorf("Reserve calls %+v, Settle calls %+v, provider calls %d; want one Reserve and neither of the others",
					reserves, settles, calls.Load())
			}
			if allows, adjusts := limiter.recorded(); len(allows) != 1 || len(adjusts) != 1 || adjusts[0].delta != allows[0].cost {
				t.Errorf("Allow calls %+v, Adjust calls %+v; want one of each, refunding the whole charge", allows, adjusts)
			}

			// Besides the access log's "request" line, one line: Budget's.
			var lines []map[string]any
			for line := range strings.Lines(logs.String()) {
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatalf("log line %q: %v", line, err)
				}
				if entry["msg"] != "request" {
					lines = append(lines, entry)
				}
			}
			if len(lines) != 1 || lines[0]["level"] != tc.level || lines[0]["msg"] != tc.msg || lines[0]["request_id"] != rec.Header().Get("X-Request-ID") ||
				lines[0]["tenant_id"] != "tn_test" || lines[0]["provider"] != "openai" {
				t.Errorf("log lines = %v, want one %s %q with the request ID, tenant and provider", lines, tc.level, tc.msg)
			}
		})
	}
}

// TestBudgetCacheHitReservesNothing: a cache hit never reaches the budget.
// Only the miss is reserved, and only it reaches the provider.
func TestBudgetCacheHitReservesNothing(t *testing.T) {
	upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
	t.Setenv("TEST_MOCK_KEY", "mock")
	ledger := &fakeLedger{}
	r := budgetRouter(t, loadConfig(t, fmt.Sprintf(cacheConfig, upstream)), budgetedStore{micros: fiftyDollars}, ledger, nil, &fakeCache{})

	for _, want := range []string{"miss", "hit"} {
		if rec := serve(r, openaiChat(zeroTemperature)); rec.Code != http.StatusOK || verdict(rec) != want {
			t.Fatalf("status %d, X-Gateway-Cache %q; want 200, %s", rec.Code, verdict(rec), want)
		}
	}
	if reserves := ledger.recorded(); len(reserves) != 1 || calls.Load() != 1 {
		t.Errorf("Reserve calls %+v, provider calls %d; want one each, for the miss", reserves, calls.Load())
	}
}

// freeFallbackConfig is fallbackConfig with ollama free, as config.yaml has
// it: ollama, which falls back to groq for qwen3.5:9b, and groq, at the URLs
// it is formatted with.
const freeFallbackConfig = `
providers:
  ollama:
    url: %s
    auth: none
    format: openai
    free: true
    fallback:
      enabled: true
      provider: groq
      models:
        "qwen3.5:9b": qwen/qwen3.8-27b
  groq:
    url: %s/openai
    key: ${TEST_GROQ_KEY}
    auth: bearer
    format: openai
`

// TestBudgetFallback: on the ollama → groq route, the free ollama attempt
// reserves nothing. Only when ollama fails does groq's attempt reserve, at
// groq's price for the model the fallback maps to. If groq's reservation is
// refused, the client gets the 402, with the headers naming groq as the
// fallback that answered.
func TestBudgetFallback(t *testing.T) {
	const body = `{"model":"qwen3.5:9b","messages":[{"role":"user","content":"ping"}]}`
	cases := []struct {
		name               string
		ollamaStatus       int // 0 for an answer
		refuse             bool
		status             int
		provider, fallback string
		reserved           bool
		groqCalls          int64
	}{
		{"ollama answers: nothing reserved", 0, false, http.StatusOK, "ollama", "false", false, 0},
		{"ollama fails: groq reserves the mapped model", http.StatusInternalServerError, false, http.StatusOK, "groq", "true", true, 1},
		{"groq refused: its 402", http.StatusInternalServerError, true, http.StatusPaymentRequired, "groq", "true", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ollama := newFakeOllama(t, ollamaKnobs{status: tc.ollamaStatus})
			groq, groqCalls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
			t.Setenv("TEST_GROQ_KEY", "sk-groq")
			ledger := &fakeLedger{refuse: tc.refuse}
			r := budgetRouter(t, loadConfig(t, fmt.Sprintf(freeFallbackConfig, ollama.server.URL, groq)), budgetedStore{micros: fiftyDollars}, ledger, nil, nil)

			before := time.Now()
			rec := serve(r, tenantPost("/ollama/v1/chat/completions", body))
			after := time.Now()

			if rec.Code != tc.status || rec.Header().Get("X-Gateway-Provider") != tc.provider || rec.Header().Get("X-Gateway-Fallback") != tc.fallback {
				t.Errorf("status %d, X-Gateway-Provider %q, X-Gateway-Fallback %q; want %d, %s, %s; body %s", rec.Code,
					rec.Header().Get("X-Gateway-Provider"), rec.Header().Get("X-Gateway-Fallback"), tc.status, tc.provider, tc.fallback, rec.Body)
			}
			if tc.status == http.StatusPaymentRequired {
				overBudget(t, rec)
			}
			if n := groqCalls.Load(); n != tc.groqCalls {
				t.Errorf("groq got %d requests, want %d", n, tc.groqCalls)
			}
			reserves := ledger.recorded()
			if !tc.reserved {
				if len(reserves) != 0 {
					t.Errorf("Reserve calls = %+v, want none", reserves)
				}
				return
			}
			// Uncapped, so at the mapped model's maximum output.
			checkReserve(t, reserves, fiftyDollars, "groq", "qwen/qwen3.8-27b", body, 0, 1, before, after)
		})
	}
}

// settledOnce fails the test unless ledger got one Reserve and one Settle,
// for the request requestID, swapping the reservation that Reserve made. It
// returns the reservation's amount and the charge.
func settledOnce(t *testing.T, ledger *fakeLedger, requestID string) (amount, charge int64) {
	t.Helper()
	reserves, settles := ledger.recorded(), ledger.settled()
	if len(reserves) != 1 || len(settles) != 1 {
		t.Fatalf("Reserve calls %+v, Settle calls %+v; want one of each", reserves, settles)
	}
	s, amount := settles[0], reserves[0].amount
	// The reservation as the fake's Reserve made it, from a spend of 0.
	want := budget.Reservation{Admitted: true, Spent: amount, Amount: amount}
	if s.res != want || s.requestID != requestID {
		t.Errorf("Settle(%+v) for request %q, want Settle(%+v) for %q", s.res, s.requestID, want, requestID)
	}
	return amount, s.charge
}

// openaiConfig is a config file with one paid provider, openai, at the URL it
// is formatted with.
const openaiConfig = `
providers:
  openai:
    url: %s
    key: ${TEST_MOCK_KEY}
    auth: bearer
    format: openai
`

// openaiAt loads openaiConfig at url, with extra appended to openai's
// settings.
func openaiAt(t *testing.T, url, extra string) *config.Config {
	t.Helper()
	t.Setenv("TEST_MOCK_KEY", "mock")
	return loadConfig(t, fmt.Sprintf(openaiConfig, url)+extra)
}

// rowsTenant creates a tenant in a fresh database, and returns the stores over
// the database, its connection, a usage writer on it and the tenant's key. The
// writer is closed as the test ends, if the test hasn't closed it already to
// read the rows. Without DATABASE_URL the test is skipped.
func rowsTenant(t *testing.T) (store stores, conn *sql.DB, writer *usage.Writer, key tenant.NewKey) {
	t.Helper()
	conn = dbtest.New(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatalf("Migrate() returned error: %v", err)
	}
	store = stores{tenant.NewStore(conn), usage.NewReports(conn)}
	_, key, err := store.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	writer = usage.NewWriter(conn, discardLogger)
	// Registered after the database's cleanups, so it runs before them.
	t.Cleanup(func() { writer.Close(context.Background()) })
	return store, conn, writer, key
}

// rowsGateway builds the gateway from cfg, budgeted by ledger, over rowsTenant's
// tenant. The real recordUsage writes each request's usage row there through
// writer, which a test closes before reading the rows. It returns the tenant's
// key too. Without DATABASE_URL the test is skipped.
func rowsGateway(t *testing.T, cfg *config.Config, ledger budgetLedger) (r http.Handler, conn *sql.DB, writer *usage.Writer, key string) {
	t.Helper()
	store, conn, writer, k := rowsTenant(t)
	r, err := router(cfg, store, testAdminKey, recordUsage(writer, cfg.Providers, discardLogger), nil, nil, ledger, metrics.New(), nil, discardLogger)
	if err != nil {
		t.Fatalf("router() returned error: %v", err)
	}
	return r, conn, writer, k.Plaintext
}

// rowCost returns cost_micros from the usage_logs row of the request
// requestID.
func rowCost(t *testing.T, conn *sql.DB, requestID string) sql.NullInt64 {
	t.Helper()
	var cost sql.NullInt64
	if err := conn.QueryRow(`SELECT cost_micros FROM usage_logs WHERE request_id = $1`, requestID).Scan(&cost); err != nil {
		t.Fatalf("no usage row for request %s: %v", requestID, err)
	}
	return cost
}

// TestBudgetSettlesAtTheRowsCost: every format, streamed or not, through the
// gateway to the mock, against Postgres. Each request is settled once,
// swapping the reservation Reserve made for exactly the cost on its usage
// row.
func TestBudgetSettlesAtTheRowsCost(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"openai", "/openai/v1/chat/completions", `{"model":"gpt-4o","messages":[]}`},
		{"openai stream", "/openai/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`},
		{"groq", "/groq/v1/chat/completions", `{"model":"openai/gpt-oss-120b","messages":[]}`},
		{"groq stream", "/groq/v1/chat/completions", `{"model":"openai/gpt-oss-120b","stream":true,"messages":[]}`},
		{"anthropic", "/anthropic/v1/messages", `{"model":"claude-sonnet-4","max_tokens":64,"messages":[]}`},
		{"anthropic stream", "/anthropic/v1/messages", `{"model":"claude-sonnet-4","max_tokens":64,"stream":true,"messages":[]}`},
		{"gemini", "/gemini/v1beta/models/gemini-3.8-flash:generateContent", `{}`},
		{"gemini sse", "/gemini/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse", `{}`},
		{"gemini json stream", "/gemini/v1beta/models/gemini-3.8-flash:streamGenerateContent", `{}`},
	}
	ledger := &fakeLedger{}
	r, conn, writer, key := rowsGateway(t, mockConfig(t), ledger)

	ids := make([]string, len(cases))
	for i, tc := range cases {
		rec := serve(r, postAs(key, tc.path, tc.body))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d; body %s", tc.name, rec.Code, rec.Body)
		}
		ids[i] = rec.Header().Get("X-Request-ID")
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	reserves, settles := ledger.recorded(), ledger.settled()
	if len(reserves) != len(cases) || len(settles) != len(cases) {
		t.Fatalf("%d Reserve and %d Settle calls, want one of each per request (%d)", len(reserves), len(settles), len(cases))
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := settles[i]
			// The reservation as the fake's Reserve made it, from a spend of 0.
			want := budget.Reservation{Admitted: true, Spent: reserves[i].amount, Amount: reserves[i].amount}
			if s.res != want || s.requestID != ids[i] {
				t.Errorf("Settle(%+v) for request %q, want Settle(%+v) for %q", s.res, s.requestID, want, ids[i])
			}
			if cost := rowCost(t, conn, ids[i]); !cost.Valid || cost.Int64 <= 0 || s.charge != cost.Int64 {
				t.Errorf("charge %d, row's cost %v; want a priced row, and the charge its cost", s.charge, cost)
			}
		})
	}
}

// TestBudgetRefundsProviderErrors: a provider that refuses the request, with a
// 4xx or a 5xx, reports no tokens, so the attempt is charged 0 and its whole
// reservation comes back.
func TestBudgetRefundsProviderErrors(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			ollama := newFakeOllama(t, ollamaKnobs{status: status})
			ledger := &fakeLedger{}
			r := budgetRouter(t, openaiAt(t, ollama.server.URL, ""), budgetedStore{micros: fiftyDollars}, ledger, nil, nil)
			rec := serve(r, chatRequest("openai", "gpt-4o"))
			if rec.Code != status {
				t.Fatalf("status = %d, want %d", rec.Code, status)
			}
			if _, charge := settledOnce(t, ledger, rec.Header().Get("X-Request-ID")); charge != 0 {
				t.Errorf("charge = %d, want 0", charge)
			}
		})
	}
}

// TestBudgetUnansweredKeepsTheReservation: a stream the client left, and a
// request the provider never answered in time, may still have been billed, so
// each is charged exactly its reservation. The stream's usage so far costs
// less. The stream runs behind a real server, so ReverseProxy aborts it with
// panic(http.ErrAbortHandler), as it would in production.
func TestBudgetUnansweredKeepsTheReservation(t *testing.T) {
	t.Run("stream cut short", func(t *testing.T) {
		ledger := &fakeLedger{}
		// One input token seen, a few µ$, where the reservation is about 100.
		r := budgetRouter(t, openaiAt(t, sseUpstream(t, 1), ""), budgetedStore{micros: fiftyDollars}, ledger, nil, nil)
		// done closes when the gateway is through with the request, abort
		// included.
		done := make(chan struct{})
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			defer close(done)
			r.ServeHTTP(w, req)
		}))
		t.Cleanup(gw.Close)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/openai/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o","stream":true,"max_tokens":5,"messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testTenantKey)
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

		if amount, charge := settledOnce(t, ledger, resp.Header.Get("X-Request-ID")); charge != amount {
			t.Errorf("charge = %d, want the reservation, %d", charge, amount)
		}
	})

	t.Run("504", func(t *testing.T) {
		ollama := newFakeOllama(t, ollamaKnobs{hold: true})
		ledger := &fakeLedger{}
		r := budgetRouter(t, openaiAt(t, ollama.server.URL, "    timeout: 1\n"), budgetedStore{micros: fiftyDollars}, ledger, nil, nil)
		rec := serve(r, chatRequest("openai", "gpt-4o"))
		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusGatewayTimeout)
		}
		if amount, charge := settledOnce(t, ledger, rec.Header().Get("X-Request-ID")); charge != amount {
			t.Errorf("charge = %d, want the reservation, %d", charge, amount)
		}
	})
}

// TestBudgetRefundsBusyRequest: a request that got no concurrency slot never
// reached the provider, and nothing reports usage for it, so it is charged 0.
func TestBudgetRefundsBusyRequest(t *testing.T) {
	ollama := newFakeOllama(t, ollamaKnobs{hold: true})
	ledger := &fakeLedger{}
	r := budgetRouter(t, openaiAt(t, ollama.server.URL, "    max_concurrency: 1\n    queue_timeout: 1\n"), budgetedStore{micros: fiftyDollars}, ledger, nil, nil)

	first := serveAsync(r, chatRequest("openai", "gpt-4o"))
	receive(t, ollama.arrivals, "the first request at the provider")
	rec := serve(r, chatRequest("openai", "gpt-4o"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	// The first request still holds its slot, so only the busy one has
	// settled.
	reserves, settles := ledger.recorded(), ledger.settled()
	if len(reserves) != 2 || len(settles) != 1 || settles[0].requestID != rec.Header().Get("X-Request-ID") || settles[0].charge != 0 {
		t.Errorf("Reserve calls %+v, Settle calls %+v; want two Reserves, and the busy request settled at 0", reserves, settles)
	}
	ollama.release()
	receive(t, first, "the first response")
}

// TestBudgetFallbackSettlesOnce: when ollama fails and groq answers, only
// groq's attempt is settled, at groq's cost of the mock's usage for the model
// the fallback maps to. The free ollama attempt reserved nothing, so it
// settles nothing.
func TestBudgetFallbackSettlesOnce(t *testing.T) {
	ollama := newFakeOllama(t, ollamaKnobs{status: http.StatusInternalServerError})
	groq, _ := countingUpstream(t, mockprovider.Handler().ServeHTTP)
	t.Setenv("TEST_GROQ_KEY", "sk-groq")
	ledger := &fakeLedger{}
	r := budgetRouter(t, loadConfig(t, fmt.Sprintf(freeFallbackConfig, ollama.server.URL, groq)), budgetedStore{micros: fiftyDollars}, ledger, nil, nil)

	rec := serve(r, chatRequest("ollama", "qwen3.5:9b"))
	if rec.Code != http.StatusOK || rec.Header().Get("X-Gateway-Fallback") != "true" {
		t.Fatalf("status %d, X-Gateway-Fallback %q; want 200 from the fallback", rec.Code, rec.Header().Get("X-Gateway-Fallback"))
	}

	_, charge := settledOnce(t, ledger, rec.Header().Get("X-Request-ID"))
	mock := usage.Usage{
		Model:       "qwen/qwen3.8-27b",
		Input:       mockprovider.PromptTokens - mockprovider.CachedPromptTokens,
		CachedInput: mockprovider.CachedPromptTokens,
		Output:      mockprovider.OutputTokens,
	}
	want, ok := pricing.Cost("groq", mock, "", ledger.recorded()[0].at)
	if !ok || charge != want {
		t.Errorf("charge = %d, want groq's cost of the mock's usage, %d", charge, want)
	}
}

// TestBudgetRefundsAFailedDial: a request whose dial failed never left the
// gateway, so it is charged 0, and its usage row costs 0.
func TestBudgetRefundsAFailedDial(t *testing.T) {
	ledger := &fakeLedger{}
	// Port 1 on loopback: nothing listens there, so the dial is refused.
	r, conn, writer, key := rowsGateway(t, openaiAt(t, "http://127.0.0.1:1", ""), ledger)
	rec := serve(r, postAs(key, "/openai/v1/chat/completions", `{"model":"gpt-4o","messages":[]}`))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	id := rec.Header().Get("X-Request-ID")
	if _, charge := settledOnce(t, ledger, id); charge != 0 {
		t.Errorf("charge = %d, want 0", charge)
	}
	if cost := rowCost(t, conn, id); !cost.Valid || cost.Int64 != 0 {
		t.Errorf("row's cost = %v, want 0", cost)
	}
}

// TestBudgetReportThroughGateway: the admin API's budget report over the real
// ledger in the real Redis, after paid requests through the gateway to the
// mock. The first report, before any request, builds the tenant's month. A
// report reserves nothing, so asking again gives the same spend, the one the
// ledger holds. Once the usage rows are written, with nothing in flight, it is
// their sum: the ledger and the rows price each response by the same function.
func TestBudgetReportThroughGateway(t *testing.T) {
	// Redis first, so that without REDIS_URL no database is made.
	rdb := gatewayRedis(t)
	store, conn, writer, key := rowsTenant(t)
	tenantID := key.TenantID
	ledger := budget.New(rdb, conn, writer.Pending)

	// Every test shares the one Redis, so this one deletes its own keys only:
	// the tenant's hash for this month, and the hash's missed counter.
	start, _ := budget.Period(time.Now())
	hash := "budget:{" + tenantID + "}:" + start.Format("2006-01")
	t.Cleanup(func() { rdb.Del(context.Background(), hash, hash+":missed") })

	cfg := mockConfig(t)
	r, err := router(cfg, store, testAdminKey, recordUsage(writer, cfg.Providers, discardLogger), nil, nil, ledger, metrics.New(), nil, discardLogger)
	if err != nil {
		t.Fatalf("router() returned error: %v", err)
	}

	adminCall := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testAdminKey)
		return serve(r, req)
	}
	type budgetReport struct {
		TenantID        string `json:"tenant_id"`
		BudgetMicros    int64  `json:"budget_micros"`
		SpentMicros     int64  `json:"spent_micros"`
		RemainingMicros int64  `json:"remaining_micros"`
	}
	report := func() budgetReport {
		t.Helper()
		rec := adminCall(http.MethodGet, "/admin/tenants/"+tenantID+"/budget", "")
		var got budgetReport
		if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("budget report: status %d, body %s (%v); want 200 and JSON", rec.Code, rec.Body, err)
		}
		return got
	}

	// $1: more than any one request below reserves, and each is settled before
	// the next is sent.
	const budgetMicros = 1_000_000
	if rec := adminCall(http.MethodPatch, "/admin/tenants/"+tenantID, fmt.Sprintf(`{"budget_micros":%d}`, budgetMicros)); rec.Code != http.StatusOK {
		t.Fatalf("PATCH: status = %d; body %s", rec.Code, rec.Body)
	}
	if got, want := report(), (budgetReport{tenantID, budgetMicros, 0, budgetMicros}); got != want {
		t.Errorf("report before any request = %+v, want %+v", got, want)
	}
	// The route exists on this router, and is still behind the admin key.
	for _, authorization := range []string{"", "Bearer " + key.Plaintext} {
		req := httptest.NewRequest(http.MethodGet, "/admin/tenants/"+tenantID+"/budget", nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		if rec := serve(r, req); rec.Code != http.StatusUnauthorized {
			t.Errorf("report with Authorization %q: status = %d, want %d", authorization, rec.Code, http.StatusUnauthorized)
		}
	}

	for _, tc := range []struct{ path, body string }{
		{"/openai/v1/chat/completions", `{"model":"gpt-4o","messages":[]}`},
		{"/anthropic/v1/messages", `{"model":"claude-sonnet-4","max_tokens":64,"messages":[]}`},
		{"/gemini/v1beta/models/gemini-3.8-flash:generateContent", `{}`},
	} {
		if rec := serve(r, postAs(key.Plaintext, tc.path, tc.body)); rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d; body %s", tc.path, rec.Code, rec.Body)
		}
	}

	first, second := report(), report()
	spent, err := ledger.Spent(context.Background(), tenantID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if want := (budgetReport{tenantID, budgetMicros, spent, budgetMicros - spent}); spent <= 0 || first != want || second != want {
		t.Errorf("reports %+v then %+v, want both %+v, from the ledger's own spend, above 0", first, second, want)
	}

	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var rows int64
	if err := conn.QueryRow(`SELECT COALESCE(SUM(cost_micros), 0) FROM usage_logs WHERE tenant_id = $1`, tenantID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != first.SpentMicros {
		t.Errorf("the tenant's usage rows cost %d µ$ in all, want spent_micros, %d", rows, first.SpentMicros)
	}
}
