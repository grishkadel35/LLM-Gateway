package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/budget"
	"github.com/grishkadel/llm-gateway/internal/pricing"
	"github.com/grishkadel/llm-gateway/internal/provider"
	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// fakeLedger stands in for the Redis ledger. Each Reserve fails with err when
// it is set, is refused at spent when refuse is, and is admitted otherwise:
// the first admission warns when warn is set, as the ledger warns once a
// month. It records each Reserve, under a lock, so concurrent requests can
// share it.
type fakeLedger struct {
	err    error
	refuse bool
	spent  int64
	warn   bool

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
	warn := f.warn
	f.warn = false
	return budget.Reservation{Admitted: true, Spent: f.spent + amount, Warn: warn, Amount: amount}, nil
}

// Settle succeeds, and records nothing.
func (f *fakeLedger) Settle(context.Context, budget.Reservation, int64) error { return nil }

// recorded returns the Reserve calls so far.
func (f *fakeLedger) recorded() []reserveCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// acmeWithBudget is acme with a monthly budget of micros.
func acmeWithBudget(micros int64) tenant.Tenant {
	tn := acme
	tn.BudgetMicros = micros
	return tn
}

// reserveTokens is the budget's input estimate for a text-only body: a token
// per 3 bytes, rounded up.
func reserveTokens(body string) int64 { return int64(len(body)+2) / 3 }

// serveBudget sends body as tn's OpenAI-format request through RequestID,
// ReadBody and Budget for the provider name, in front of a handler that
// records whether it was reached and the reservation it saw. It also returns
// the Request ReadBody made. A zero tn sends it with no tenant at all.
func serveBudget(t *testing.T, ledger Ledger, name string, free bool, logger *slog.Logger, tn tenant.Tenant, body string) (rec *httptest.ResponseRecorder, req *usage.Request, reached bool, res *budget.Reservation) {
	t.Helper()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		res = reservationFrom(r.Context())
	})
	// Between ReadBody and Budget, so it sees the Request even when Budget
	// refuses it.
	capture := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req, _ = usage.RequestFrom(r.Context())
			next.ServeHTTP(w, r)
		})
	}
	h := usage.ReadBody(provider.FormatOpenAI, 1<<20)(capture(Budget(ledger, name, free, logger)(next)))
	if tn.ID != "" {
		h = asTenant(tn, h)
	}

	rec = httptest.NewRecorder()
	RequestID(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return rec, req, reached, res
}

// errorBody decodes the gateway's error body in rec.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	return body.Error
}

// TestBudgetAdmits: a request the ledger admits goes on, carrying the very
// reservation the ledger made. Reserve is asked once: for the tenant, against
// its budget, for pricing.Reserve's amount at the provider's price for the
// model, the cap and the choices, and at the time ReadBody received the
// request.
func TestBudgetAdmits(t *testing.T) {
	const body = `{"model":"gpt-4o","max_tokens":50,"n":2,"messages":[{"role":"user","content":"hi"}]}`
	ledger := &fakeLedger{spent: 1_000}
	rec, req, reached, res := serveBudget(t, ledger, "openai", false, discard, acmeWithBudget(50_000_000), body)

	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("reached = %v, status = %d; want the request let through", reached, rec.Code)
	}
	amount, ok := pricing.Reserve("openai", "gpt-4o", reserveTokens(body), 100, 2, req.Received)
	if !ok {
		t.Fatal("gpt-4o should be priced")
	}
	calls := ledger.recorded()
	if len(calls) != 1 {
		t.Fatalf("Reserve calls = %+v, want one", calls)
	}
	if c := calls[0]; c.tenantID != "tn_acme" || c.budget != 50_000_000 || c.amount != amount || !c.at.Equal(req.Received) {
		t.Errorf("Reserve(%q, budget %d, amount %d, at %v), want Reserve(%q, budget %d, amount %d, at %v)",
			c.tenantID, c.budget, c.amount, c.at, "tn_acme", 50_000_000, amount, req.Received)
	}
	want := budget.Reservation{Admitted: true, Spent: 1_000 + amount, Amount: amount}
	if res == nil || *res != want {
		t.Errorf("reservation on the context = %+v, want %+v", res, want)
	}
}

// TestBudgetRefuses: a request the ledger refuses gets 402 budget_exceeded,
// with the numbers behind it, a message in exact dollars that says what would
// help, no Retry-After, and one Info line. It goes no further.
func TestBudgetRefuses(t *testing.T) {
	const body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name                     string
		budget, spent, remaining int64
		// message is the 402's message, with AMOUNT for the reservation in
		// dollars and RESETS for resets_at.
		message string
	}{
		{"spent: it names the reset", 50_000_000, 49_996_000, 4_000,
			"this request could cost up to AMOUNT, more than the $0.004 left of the gateway's $50.00 monthly budget for this tenant; the budget resets at RESETS"},
		{"overspent: none left", 50_000_000, 50_000_100, 0,
			"this request could cost up to AMOUNT, more than the $0.00 left of the gateway's $50.00 monthly budget for this tenant; the budget resets at RESETS"},
		{"over the whole budget: a lower output cap", 100_000, 0, 100_000,
			"this request could cost up to AMOUNT, more than the gateway's whole monthly budget for this tenant, $0.10; set a lower output cap or send a shorter request"},
		{"budget 0: free providers only", 0, 0, 0,
			"the gateway's monthly budget for this tenant is $0.00, so it can use free providers only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			ledger := &fakeLedger{refuse: true, spent: tc.spent}
			rec, req, reached, _ := serveBudget(t, ledger, "openai", false, slog.New(slog.NewJSONHandler(&logs, nil)), acmeWithBudget(tc.budget), body)

			if reached {
				t.Error("a refused request went on to the next handler")
			}
			if rec.Code != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusPaymentRequired)
			}
			if got := rec.Header().Values("Retry-After"); got != nil {
				t.Errorf("Retry-After = %q, want none", got)
			}

			// Uncapped, so priced at gpt-4o's maximum output, 16,384.
			amount, _ := pricing.Reserve("openai", "gpt-4o", reserveTokens(body), 0, 1, req.Received)
			if calls := ledger.recorded(); len(calls) != 1 || calls[0].amount != amount {
				t.Errorf("Reserve calls = %+v, want one, for %d", calls, amount)
			}
			// The month the request arrived in, UTC, and the next one's start.
			y, m, _ := req.Received.UTC().Date()
			period := fmt.Sprintf("%04d-%02d", y, m)
			resetsAt := time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
			want := map[string]any{
				"type":             "budget_exceeded",
				"message":          strings.NewReplacer("AMOUNT", dollars(amount), "RESETS", resetsAt).Replace(tc.message),
				"budget_micros":    float64(tc.budget),
				"spent_micros":     float64(tc.spent),
				"remaining_micros": float64(tc.remaining),
				"reserve_micros":   float64(amount),
				"period":           period,
				"resets_at":        resetsAt,
			}
			if got := errorBody(t, rec); !reflect.DeepEqual(got, want) {
				t.Errorf("error = %v\nwant %v", got, want)
			}

			entry := logLine(t, &logs)
			if entry["level"] != "INFO" || entry["msg"] != "request over budget" || entry["request_id"] != rec.Header().Get(HeaderRequestID) ||
				entry["tenant_id"] != "tn_acme" || entry["provider"] != "openai" || entry["model"] != "gpt-4o" ||
				entry["reserve_micros"] != float64(amount) || entry["spent_micros"] != float64(tc.spent) || entry["budget_micros"] != float64(tc.budget) {
				t.Errorf("log line = %v, want INFO with the request ID, tenant, provider, model, reservation, spend and budget", entry)
			}
		})
	}
}

// TestBudgetUnknownModel: a model with no price, or no model at all, gets 400
// unknown_model naming the provider and the model, and a Warn for the
// operator. Nothing is reserved, and the request goes no further.
func TestBudgetUnknownModel(t *testing.T) {
	cases := []struct {
		name, body, model, message string
	}{
		{"unpriced", `{"model":"gpt-9000","messages":[]}`, "gpt-9000",
			`the gateway has no price for model "gpt-9000" on provider "openai", so it can't budget for the request`},
		{"no model", `{"messages":[]}`, "",
			`the request names no model, so the gateway can't price it on provider "openai"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			ledger := &fakeLedger{}
			rec, _, reached, _ := serveBudget(t, ledger, "openai", false, slog.New(slog.NewJSONHandler(&logs, nil)), acmeWithBudget(50_000_000), tc.body)

			if reached || rec.Code != http.StatusBadRequest {
				t.Fatalf("reached = %v, status = %d; want 400, refused", reached, rec.Code)
			}
			want := map[string]any{"type": "unknown_model", "message": tc.message, "provider": "openai", "model": tc.model}
			if got := errorBody(t, rec); !reflect.DeepEqual(got, want) {
				t.Errorf("error = %v\nwant %v", got, want)
			}
			if calls := ledger.recorded(); len(calls) != 0 {
				t.Errorf("Reserve calls = %+v, want none", calls)
			}
			entry := logLine(t, &logs)
			if entry["level"] != "WARN" || entry["request_id"] != rec.Header().Get(HeaderRequestID) || entry["tenant_id"] != "tn_acme" ||
				entry["provider"] != "openai" || entry["model"] != tc.model {
				t.Errorf("log line = %v, want WARN with the request ID, tenant, provider and model", entry)
			}
		})
	}
}

// TestBudgetFailsClosed: when Reserve fails, the request gets 503
// budget_unavailable and goes no further, with an Error log.
func TestBudgetFailsClosed(t *testing.T) {
	var logs bytes.Buffer
	ledger := &fakeLedger{err: errors.New("redis: connection refused")}
	rec, _, reached, _ := serveBudget(t, ledger, "openai", false, slog.New(slog.NewJSONHandler(&logs, nil)), acmeWithBudget(50_000_000),
		`{"model":"gpt-4o","messages":[]}`)

	if reached || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("reached = %v, status = %d; want 503, refused", reached, rec.Code)
	}
	want := map[string]any{"type": "budget_unavailable", "message": "the gateway can't check this tenant's budget right now; retry shortly"}
	if got := errorBody(t, rec); !reflect.DeepEqual(got, want) {
		t.Errorf("error = %v, want %v", got, want)
	}
	if calls := ledger.recorded(); len(calls) != 1 {
		t.Errorf("Reserve calls = %+v, want one", calls)
	}
	entry := logLine(t, &logs)
	if entry["level"] != "ERROR" || entry["request_id"] != rec.Header().Get(HeaderRequestID) || entry["tenant_id"] != "tn_acme" ||
		entry["provider"] != "openai" || entry["error"] != "redis: connection refused" {
		t.Errorf("log line = %v, want ERROR with the request ID, tenant, provider and error", entry)
	}
}

// TestBudgetWithoutTenantOrRequest: a request with no tenant, or no Request
// from ReadBody, means the routes are wired wrong. It is logged as an error
// and refused with 503, without asking the ledger: budgets guard money.
func TestBudgetWithoutTenantOrRequest(t *testing.T) {
	const body = `{"model":"gpt-4o","messages":[]}`
	cases := []struct {
		name  string
		serve func(t *testing.T, ledger Ledger, logger *slog.Logger) (rec *httptest.ResponseRecorder, reached bool)
	}{
		{"no tenant", func(t *testing.T, ledger Ledger, logger *slog.Logger) (*httptest.ResponseRecorder, bool) {
			rec, _, reached, _ := serveBudget(t, ledger, "openai", false, logger, tenant.Tenant{}, body)
			return rec, reached
		}},
		{"no ReadBody", func(_ *testing.T, ledger Ledger, logger *slog.Logger) (*httptest.ResponseRecorder, bool) {
			reached := false
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
			rec := httptest.NewRecorder()
			RequestID(asTenant(acmeWithBudget(50_000_000), Budget(ledger, "openai", false, logger)(next))).
				ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
			return rec, reached
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			ledger := &fakeLedger{}
			rec, reached := tc.serve(t, ledger, slog.New(slog.NewJSONHandler(&logs, nil)))

			if reached || rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("reached = %v, status = %d; want 503, refused", reached, rec.Code)
			}
			assertErrorType(t, rec, "budget_unavailable")
			if calls := ledger.recorded(); len(calls) != 0 {
				t.Errorf("Reserve calls = %+v, want none", calls)
			}
			if entry := logLine(t, &logs); entry["level"] != "ERROR" || entry["request_id"] != rec.Header().Get(HeaderRequestID) {
				t.Errorf("log line = %v, want ERROR with the request ID", entry)
			}
		})
	}
}

// TestBudgetFreeProvider: a free provider's Budget is next itself, so a
// request goes straight on, even with no tenant, while the ledger would refuse
// or fail it, and the ledger is never asked.
func TestBudgetFreeProvider(t *testing.T) {
	cases := []struct {
		name   string
		ledger *fakeLedger
	}{
		{"ledger refusing", &fakeLedger{refuse: true}},
		{"ledger failing", &fakeLedger{err: errors.New("redis: connection refused")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			rec, _, reached, res := serveBudget(t, tc.ledger, "ollama", true, slog.New(slog.NewJSONHandler(&logs, nil)), tenant.Tenant{},
				`{"model":"qwen3.5:9b","messages":[]}`)

			if !reached || rec.Code != http.StatusOK || res != nil {
				t.Errorf("reached = %v, status = %d, reservation %+v; want served with none", reached, rec.Code, res)
			}
			if calls := tc.ledger.recorded(); len(calls) != 0 || logs.Len() != 0 {
				t.Errorf("Reserve calls = %+v, logs %q; want neither", calls, logs.String())
			}
		})
	}
}

// TestBudgetWarnsOnce: a reservation the ledger reports as the first to take
// the month's spend to 80% of the budget is logged with one Warn, and no other
// is.
func TestBudgetWarnsOnce(t *testing.T) {
	const body = `{"model":"gpt-4o","messages":[]}`
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ledger := &fakeLedger{spent: 40_000_000, warn: true}

	first, req, _, _ := serveBudget(t, ledger, "openai", false, logger, acmeWithBudget(50_000_000), body)
	second, _, _, _ := serveBudget(t, ledger, "openai", false, logger, acmeWithBudget(50_000_000), body)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses %d and %d, want both admitted", first.Code, second.Code)
	}

	amount, _ := pricing.Reserve("openai", "gpt-4o", reserveTokens(body), 0, 1, req.Received)
	entry := logLine(t, &logs) // fails unless there is exactly one line
	if entry["level"] != "WARN" || entry["request_id"] != first.Header().Get(HeaderRequestID) || entry["tenant_id"] != "tn_acme" ||
		entry["spent_micros"] != float64(40_000_000+amount) || entry["budget_micros"] != float64(50_000_000) {
		t.Errorf("log line = %v, want one WARN for the first request, with its tenant, spend and budget", entry)
	}
}

// TestDollars: amounts in µ$ are written as exact dollars, with two decimals
// at least and as many as six, never rounded.
func TestDollars(t *testing.T) {
	cases := []struct {
		micros int64
		want   string
	}{
		{0, "$0.00"},
		{1, "$0.000001"},
		{123, "$0.000123"},
		{4_000, "$0.004"},
		{10_000, "$0.01"},
		{1_500_000, "$1.50"},
		{1_234_567, "$1.234567"},
		{50_000_000, "$50.00"},
		{tenant.MaxBudgetMicros, "$1000000000.00"},
	}
	for _, tc := range cases {
		if got := dollars(tc.micros); got != tc.want {
			t.Errorf("dollars(%d) = %q, want %q", tc.micros, got, tc.want)
		}
	}
}
