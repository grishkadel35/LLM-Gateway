package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/grishkadel/llm-gateway/internal/usage"
)

// scrape returns what GET /metrics serves.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: status = %d", rec.Code)
	}
	return rec.Body.String()
}

// TestRequestsRecordsTheStatusSent: each request counts once, under the status
// the client received, and is timed.
func TestRequestsRecordsTheStatusSent(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"explicit status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, "401"},
		{"body without WriteHeader", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) }, "200"},
		{"nothing written", func(w http.ResponseWriter, r *http.Request) {}, "200"},
		// An informational response isn't the status the client ends up with.
		{"1xx first", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusServiceUnavailable)
		}, "503"},
		// How ReverseProxy ends a stream whose client went away.
		{"aborted mid-stream", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			panic(http.ErrAbortHandler)
		}, "200"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New()
			h := m.Requests("ollama")(tc.handler)
			func() {
				// net/http recovers the abort panic; so does the test.
				defer func() { _ = recover() }()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
			}()

			if got := testutil.ToFloat64(m.requests.WithLabelValues("ollama", tc.want)); got != 1 {
				t.Errorf("gateway_requests_total{status=%q} = %v, want 1", tc.want, got)
			}
			if got := testutil.CollectAndCount(m.requests); got != 1 {
				t.Errorf("%d request series, want 1", got)
			}
			if !strings.Contains(scrape(t, m), `gateway_request_duration_seconds_count{provider="ollama"} 1`+"\n") {
				t.Error("the request was not timed")
			}
		})
	}
}

// TestRequestsKeepsFlush: a streamed reply must still flush through the
// recorder, or each event would wait for the end of the stream.
func TestRequestsKeepsFlush(t *testing.T) {
	var flushErr error
	h := New().Requests("ollama")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if flushErr != nil || !rec.Flushed {
		t.Errorf("Flush() = %v, flushed = %v; want it to reach the underlying writer", flushErr, rec.Flushed)
	}
}

// TestTokens: each type adds up across responses, and a type with no tokens
// gets no series.
func TestTokens(t *testing.T) {
	m := New()
	m.Tokens("groq", usage.Usage{Input: 10, CachedInput: 5, Output: 7})
	m.Tokens("groq", usage.Usage{Input: 1, Output: 1})
	// A provider reporting more cached tokens than prompt tokens must not
	// panic the gateway.
	m.Tokens("groq", usage.Usage{Input: -3})

	for typ, want := range map[string]float64{"input": 11, "cached_input": 5, "output": 8} {
		if got := testutil.ToFloat64(m.tokens.WithLabelValues("groq", typ)); got != want {
			t.Errorf("gateway_tokens_total{type=%q} = %v, want %v", typ, got, want)
		}
	}
	if got := testutil.CollectAndCount(m.tokens); got != 3 {
		t.Errorf("%d token series, want 3: a zero must not create one", got)
	}
}

// TestProviderUp: the gauge follows the latest health check.
func TestProviderUp(t *testing.T) {
	m := New()
	for _, up := range []bool{true, false} {
		m.ProviderUp("ollama", up)
		want := 0.0
		if up {
			want = 1
		}
		if got := testutil.ToFloat64(m.up.WithLabelValues("ollama")); got != want {
			t.Errorf("after up = %v: gateway_provider_up = %v, want %v", up, got, want)
		}
	}
}

// TestFallback: each fallback counts once, by route, target and reason.
func TestFallback(t *testing.T) {
	m := New()
	m.Fallback("ollama", "groq", "timeout")
	if got := testutil.ToFloat64(m.fallbacks.WithLabelValues("ollama", "groq", "timeout")); got != 1 {
		t.Errorf("gateway_fallbacks_total = %v, want 1", got)
	}
}
