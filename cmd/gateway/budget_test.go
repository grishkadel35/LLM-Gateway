package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/budget"
	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/metrics"
	"github.com/grishkadel/llm-gateway/internal/middleware"
	"github.com/grishkadel/llm-gateway/internal/mockprovider"
	"github.com/grishkadel/llm-gateway/internal/pricing"
	"github.com/grishkadel/llm-gateway/internal/tenant"
)

// fakeLedger stands in for the Redis ledger, so routing tests need no Redis.
// Each Reserve fails with err when it is set, is refused at spent when refuse
// is, and is admitted otherwise. It records each Reserve, under a lock, so
// concurrent requests can share it.
type fakeLedger struct {
	err    error
	refuse bool
	spent  int64

	mu    sync.Mutex
	calls []reserveCall
}

// reserveCall is one Reserve a fakeLedger received.
type reserveCall struct {
	tenantID       string
	budget, amount int64
	at             time.Time
}

func (f *fakeLedger) Reserve(_ context.Context, tenantID string, budgetMicros, amount int64, at time.Time) (budget.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, reserveCall{tenantID, budgetMicros, amount, at})
	switch {
	case f.err != nil:
		return budget.Reservation{}, f.err
	case f.refuse:
		return budget.Reservation{Spent: f.spent, Amount: amount}, nil
	}
	return budget.Reservation{Admitted: true, Spent: f.spent + amount, Amount: amount}, nil
}

// Settle succeeds, and records nothing.
func (f *fakeLedger) Settle(context.Context, budget.Reservation, int64) error { return nil }

// recorded returns the Reserve calls so far.
func (f *fakeLedger) recorded() []reserveCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
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
func budgetRouter(t *testing.T, cfg *config.Config, store tenantStore, ledger middleware.Ledger, limiter middleware.Limiter, cacheStore middleware.CacheStore) http.Handler {
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
