package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/grishkadel/llm-gateway/internal/mockprovider"
)

// TestMalformedJSONOnAPaidRoute: a body that isn't JSON names no model the
// gateway can read, even when the body, or on Gemini the URL, does name one. A
// paid route can't price it, so it answers 400 unknown_model, reserves
// nothing, and never calls the provider. The rate limiter, which charged the
// request before the budget saw it, gets its whole charge back.
func TestMalformedJSONOnAPaidRoute(t *testing.T) {
	cases := []struct{ name, provider, path, body string }{
		{"openai, cut off", "openai", "/openai/v1/chat/completions",
			`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}`},
		{"openai, not JSON", "openai", "/openai/v1/chat/completions",
			`this is not json`},
		{"openai, text after the object", "openai", "/openai/v1/chat/completions",
			`{"model":"gpt-4o","messages":[]} and then some`},
		{"anthropic, cut off", "anthropic", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"hi"}`},
		{"gemini, cut off, the model in the URL", "gemini", "/gemini/v1beta/models/gemini-3.8-flash:generateContent",
			`{"contents":[{"parts":[{"text":"hi"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
			ledger, limiter := &fakeLedger{}, &fakeLimiter{allowed: true}
			r := budgetRouter(t, mockConfigAt(t, upstream), budgetedStore{micros: fiftyDollars}, ledger, limiter, nil)

			rec := serve(r, tenantPost(tc.path, tc.body))

			var got struct {
				Error struct{ Type, Provider string } `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body %s: %v", rec.Body, err)
			}
			if e := got.Error; rec.Code != http.StatusBadRequest || e.Type != "unknown_model" || e.Provider != tc.provider {
				t.Errorf("status %d, error %+v; want 400 unknown_model from %s", rec.Code, e, tc.provider)
			}
			if reserves := ledger.recorded(); len(reserves) != 0 || calls.Load() != 0 {
				t.Errorf("Reserve calls %+v, provider calls %d; want neither", reserves, calls.Load())
			}
			allows, adjusts := limiter.recorded()
			if len(allows) != 1 || len(adjusts) != 1 || adjusts[0] != (adjustCall{allows[0].tenantID, allows[0].limit, allows[0].cost, nil}) {
				t.Errorf("Allow calls %+v, Adjust calls %+v; want one charge, and all of it back", allows, adjusts)
			}
		})
	}
}

// TestMalformedJSONOnAFreeRoute: a free route is never budgeted, so a body that
// isn't JSON reaches a keyless provider (Ollama) as the client sent it, byte
// for byte, with budgets on. The provider's answer, its refusal, goes back to
// the client unchanged, and the request's usage row costs 0. Without
// DATABASE_URL the test is skipped.
func TestMalformedJSONOnAFreeRoute(t *testing.T) {
	const answer = `{"error":{"message":"invalid character 'h' in literal true (expecting 'r')","type":"invalid_request_error","param":null,"code":null}}`
	cases := []struct{ name, body string }{
		{"cut off", `{"model":"qwen3.5:9b","messages":[{"role":"user","content":"hi"}`},
		{"not JSON", `this is not json`},
		{"text after the object", `{"model":"qwen3.5:9b","messages":[]} and then some`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			upstream, calls := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				received <- string(b)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, answer)
			})
			ledger := &fakeLedger{}
			cfg := loadConfig(t, fmt.Sprintf(ollamaConfig+"    free: true\n", upstream))
			r, conn, writer, key := rowsGateway(t, cfg, ledger)

			rec := serve(r, postAs(key, "/ollama/v1/chat/completions", tc.body))

			if calls.Load() != 1 {
				t.Fatalf("provider calls = %d, want 1", calls.Load())
			}
			if got := <-received; got != tc.body {
				t.Errorf("Ollama received %q, want the client's exact bytes %q", got, tc.body)
			}
			if rec.Code != http.StatusBadRequest || rec.Body.String() != answer || rec.Header().Get("Content-Type") != "application/json" {
				t.Errorf("status %d, Content-Type %q, body %s; want Ollama's 400 unchanged",
					rec.Code, rec.Header().Get("Content-Type"), rec.Body)
			}
			if reserves := ledger.recorded(); len(reserves) != 0 {
				t.Errorf("Reserve calls = %+v, want none: a free route is never budgeted", reserves)
			}

			if err := writer.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if cost := rowCost(t, conn, rec.Header().Get("X-Request-ID")); !cost.Valid || cost.Int64 != 0 {
				t.Errorf("row's cost = %v, want 0", cost)
			}
		})
	}
}
