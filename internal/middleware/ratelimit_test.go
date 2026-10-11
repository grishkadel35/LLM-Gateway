package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/provider"
	"github.com/grishkadel/llm-gateway/internal/ratelimit"
	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// fakeLimiter answers every Allow the same way, and records each one.
type fakeLimiter struct {
	allowed bool
	wait    time.Duration
	err     error

	calls []allowCall
}

// allowCall is one Allow a fakeLimiter received.
type allowCall struct {
	tenantID    string
	limit, cost int64
	// timeout is how long the call had left when it arrived, or 0 if its
	// context had no deadline.
	timeout time.Duration
}

func (f *fakeLimiter) Allow(ctx context.Context, tenantID string, limit, cost int64) (bool, time.Duration, error) {
	var timeout time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	f.calls = append(f.calls, allowCall{tenantID, limit, cost, timeout})
	return f.allowed, f.wait, f.err
}

// acme has the schema's default limits.
var acme = tenant.Tenant{ID: "tn_acme", RateLimitTokensPerMin: 100_000, DefaultMaxTokens: 4096}

// serveRateLimit sends body as tn's OpenAI-format request through RequestID,
// ReadBody and RateLimit, in front of a handler that records whether it was
// reached and the charge it saw. A zero tn sends it with no tenant at all.
func serveRateLimit(t *testing.T, limiter Limiter, logger *slog.Logger, tn tenant.Tenant, body string) (rec *httptest.ResponseRecorder, reached bool, c *charge) {
	t.Helper()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		c = chargeFrom(r.Context())
	})
	h := usage.ReadBody(provider.FormatOpenAI, 1<<20)(RateLimit(limiter, logger)(next))
	if tn.ID != "" {
		h = asTenant(tn, h)
	}

	rec = httptest.NewRecorder()
	RequestID(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return rec, reached, c
}

// asTenant puts tn on each request's context, as Auth does for a valid key.
func asTenant(tn tenant.Tenant, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, principal{tenant: tn})))
	})
}

// logLine decodes the single line logs holds.
func logLine(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log output %q is not one JSON line: %v", logs, err)
	}
	return entry
}

// textTokens is the input estimate for a text-only body: a token per 4
// bytes, rounded up.
func textTokens(body string) int64 { return int64(len(body)+3) / 4 }

// TestRateLimitRefusesOverLimit: a request the bucket can't cover gets the
// gateway's 429, with Retry-After in whole seconds (rounded up, at least 1)
// and the numbers behind it, and goes no further.
func TestRateLimitRefusesOverLimit(t *testing.T) {
	cases := []struct {
		name string
		wait time.Duration
		want int64
	}{
		{"whole seconds", 12 * time.Second, 12},
		{"a part second rounds up", 1500 * time.Millisecond, 2},
		{"never below 1", 0, 1},
	}
	const body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	wantCost := textTokens(body) + 4096

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			limiter := &fakeLimiter{wait: tc.wait}
			rec, reached, _ := serveRateLimit(t, limiter, slog.New(slog.NewJSONHandler(&logs, nil)), acme, body)

			if reached {
				t.Error("a refused request went on to the next handler")
			}
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
			}
			if got := rec.Header().Get("Retry-After"); got != strconv.FormatInt(tc.want, 10) {
				t.Errorf("Retry-After = %q, want %d", got, tc.want)
			}

			var got struct {
				Error struct {
					Type              string `json:"type"`
					Message           string `json:"message"`
					RetryAfterSeconds int64  `json:"retry_after_seconds"`
					LimitTokensPerMin int64  `json:"limit_tokens_per_min"`
					EstimatedTokens   int64  `json:"estimated_tokens"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body %s: %v", rec.Body, err)
			}
			e := got.Error
			if e.Type != "rate_limited" || !strings.Contains(e.Message, "gateway") ||
				e.RetryAfterSeconds != tc.want || e.LimitTokensPerMin != 100_000 || e.EstimatedTokens != wantCost {
				t.Errorf("error = %+v, want rate_limited from the gateway, retry after %d s, limit 100000, estimate %d", e, tc.want, wantCost)
			}

			entry := logLine(t, &logs)
			if entry["level"] != "INFO" || entry["request_id"] != rec.Header().Get(HeaderRequestID) || entry["tenant_id"] != "tn_acme" ||
				entry["estimated_tokens"] != float64(wantCost) || entry["retry_after_seconds"] != float64(tc.want) {
				t.Errorf("log line = %v, want INFO with the request ID, tenant, estimate and wait", entry)
			}
		})
	}
}

// TestRateLimitAdmitsUnderLimit: an admitted request goes on carrying its
// charge, the very one Allow was asked for, and Allow gets at most
// allowTimeout.
func TestRateLimitAdmitsUnderLimit(t *testing.T) {
	const body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	limiter := &fakeLimiter{allowed: true}
	rec, reached, c := serveRateLimit(t, limiter, slog.New(slog.NewTextHandler(io.Discard, nil)), acme, body)

	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("reached = %v, status = %d; want the request let through", reached, rec.Code)
	}
	if len(limiter.calls) != 1 {
		t.Fatalf("Allow called %d times, want once", len(limiter.calls))
	}
	call := limiter.calls[0]
	want := charge{tenantID: "tn_acme", limit: 100_000, cost: textTokens(body) + 4096}
	if call.tenantID != want.tenantID || call.limit != want.limit || call.cost != want.cost {
		t.Errorf("Allow(%q, limit %d, cost %d), want Allow(%q, limit %d, cost %d)",
			call.tenantID, call.limit, call.cost, want.tenantID, want.limit, want.cost)
	}
	if call.timeout <= 0 || call.timeout > allowTimeout {
		t.Errorf("Allow had %v left, want a deadline within %v", call.timeout, allowTimeout)
	}
	if c == nil || *c != want {
		t.Errorf("charge on the context = %+v, want %+v", c, want)
	}
}

// TestRateLimitCost: the charge is the input estimate plus the client's
// output cap, or default_max_tokens for each choice when it set none. The
// output part never exceeds the tenant's limit, nothing wraps negative, and
// the total stops at ratelimit.MaxCost, where Allow clamps it.
func TestRateLimitCost(t *testing.T) {
	// A limit no request reaches, so only MaxCost bounds the charge.
	vast := tenant.Tenant{ID: "tn_vast", RateLimitTokensPerMin: 1<<63 - 1, DefaultMaxTokens: 4096}
	cases := []struct {
		name   string
		tenant tenant.Tenant
		body   string
		// output is the charge's output part: the request costs
		// textTokens(body) + output, up to ratelimit.MaxCost.
		output int64
	}{
		{"client's cap", acme, `{"model":"m","max_tokens":500,"messages":[]}`, 500},
		{"no cap: default_max_tokens", acme, `{"model":"m","messages":[]}`, 4096},
		{"no cap, n 3: default for each choice", acme, `{"model":"m","n":3,"messages":[]}`, 3 * 4096},
		{"cap of 2^31: clamped at the limit", acme, `{"model":"m","max_tokens":2147483648,"messages":[]}`, 100_000},
		{"cap of 2^63-1, n 2: clamped, not wrapped", acme, `{"model":"m","max_tokens":9223372036854775807,"n":2,"messages":[]}`, 100_000},
		{"no cap, n 2^63-1: default × n saturates", acme, `{"model":"m","n":9223372036854775807,"messages":[]}`, 100_000},
		{"cap past MaxCost: the total stops at MaxCost", vast, `{"model":"m","max_tokens":9223372036854775807,"messages":[]}`, ratelimit.MaxCost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := min(textTokens(tc.body)+tc.output, ratelimit.MaxCost)

			limiter := &fakeLimiter{allowed: true}
			_, _, c := serveRateLimit(t, limiter, slog.New(slog.NewTextHandler(io.Discard, nil)), tc.tenant, tc.body)

			if len(limiter.calls) != 1 || limiter.calls[0].cost != want {
				t.Fatalf("Allow calls = %+v, want one costing %d", limiter.calls, want)
			}
			if c == nil || c.cost != want {
				t.Errorf("charge on the context = %+v, want cost %d", c, want)
			}
		})
	}
}

// TestRateLimitFailsOpen: when Allow fails (Redis down or slow), the request
// goes ahead with a Warn, and its charge is still on the context, since the
// script may have run.
func TestRateLimitFailsOpen(t *testing.T) {
	const body = `{"model":"gpt-4o","messages":[]}`
	var logs bytes.Buffer
	limiter := &fakeLimiter{err: context.DeadlineExceeded}
	rec, reached, c := serveRateLimit(t, limiter, slog.New(slog.NewJSONHandler(&logs, nil)), acme, body)

	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("reached = %v, status = %d; want the request let through", reached, rec.Code)
	}
	want := charge{tenantID: "tn_acme", limit: 100_000, cost: textTokens(body) + 4096}
	if c == nil || *c != want {
		t.Errorf("charge on the context = %+v, want %+v", c, want)
	}
	entry := logLine(t, &logs)
	if entry["level"] != "WARN" || entry["request_id"] != rec.Header().Get(HeaderRequestID) || entry["tenant_id"] != "tn_acme" ||
		entry["error"] != context.DeadlineExceeded.Error() {
		t.Errorf("log line = %v, want WARN with the request ID, tenant and error", entry)
	}
}

// TestRateLimitWithoutTenant: a request with no tenant on its context means
// the routes are wired wrong. It is logged as an error and served
// unlimited, without asking the limiter.
func TestRateLimitWithoutTenant(t *testing.T) {
	var logs bytes.Buffer
	limiter := &fakeLimiter{}
	_, reached, c := serveRateLimit(t, limiter, slog.New(slog.NewJSONHandler(&logs, nil)), tenant.Tenant{}, `{}`)

	if !reached || c != nil || len(limiter.calls) != 0 {
		t.Errorf("reached = %v, charge = %+v, Allow calls = %d; want served with no charge and no Allow", reached, c, len(limiter.calls))
	}
	if entry := logLine(t, &logs); entry["level"] != "ERROR" {
		t.Errorf("log line = %v, want ERROR", entry)
	}
}
