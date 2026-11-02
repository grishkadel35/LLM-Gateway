package metrics

import (
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/grishkadel/llm-gateway/internal/middleware"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// checkCounter fails the test unless c's samples are exactly want: each
// series' label values in c's order, joined by commas, and its value.
func checkCounter(t *testing.T, name string, c *prometheus.CounterVec, want map[string]float64) {
	t.Helper()
	if got := testutil.CollectAndCount(c); got != len(want) {
		t.Errorf("%s: %d series, want %d: %v", name, got, len(want), want)
	}
	for labels, v := range want {
		if got := testutil.ToFloat64(c.WithLabelValues(strings.Split(labels, ",")...)); got != v {
			t.Errorf("%s{%s} = %v, want %v", name, labels, got, v)
		}
	}
}

// serveRecorded serves one request on route through Logging, which puts a
// Record on it, and m.Requests. The handler inside records what record
// says, as the layers inside would, and answers with status.
func serveRecorded(m *Metrics, route string, record middleware.Record, status int) {
	h := m.Requests(route)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := middleware.RecordFrom(r.Context())
		rec.TenantID, rec.Request, rec.Cache, rec.RateLimited, rec.BudgetRefusal =
			record.TenantID, record.Request, record.Cache, record.RateLimited, record.BudgetRefusal
		w.WriteHeader(status)
	}))
	middleware.Logging(slog.New(slog.DiscardHandler))(h).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
}

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
// the client received, and is timed. With no Record, it has tenant none and
// model other.
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

			checkCounter(t, "gateway_requests_total", m.requests, map[string]float64{"none,ollama,other," + tc.want: 1})
			if !strings.Contains(scrape(t, m), `gateway_request_duration_seconds_count{provider="ollama"} 1`+"\n") {
				t.Error("the request was not timed")
			}
		})
	}
}

// TestRequestsReadsTheRecord: once the route is done, each request counts
// under its Record's tenant and the table's name for the model it asked of
// the route's provider, or none and other. Its cache decision, the
// gateway's own 429 and a budget's refusal count too, each once, and
// nothing else does: a bad X-Gateway-Cache, a provider's own 429 and a
// client gone during the budget check (499) leave the Record empty.
func TestRequestsReadsTheRecord(t *testing.T) {
	asked := func(model string) *usage.Request { return &usage.Request{Model: model} }
	cases := []struct {
		name   string
		route  string
		record middleware.Record
		status int
		// Each counter's samples, as checkCounter takes them.
		requests, cache, rateLimits, budget map[string]float64
	}{
		{"nothing recorded", "openai", middleware.Record{}, http.StatusUnauthorized,
			map[string]float64{"none,openai,other,401": 1}, nil, nil, nil},
		{"a dated model", "openai", middleware.Record{TenantID: "t1", Request: asked("gpt-4o-2024-08-06"), Cache: "miss"}, http.StatusOK,
			map[string]float64{"t1,openai,gpt-4o,200": 1}, map[string]float64{"t1,openai,miss": 1}, nil, nil},
		{"a snapshot priced on its own", "openai", middleware.Record{TenantID: "t1", Request: asked("gpt-4o-2024-05-13"), Cache: "hit"}, http.StatusOK,
			map[string]float64{"t1,openai,gpt-4o-2024-05-13,200": 1}, map[string]float64{"t1,openai,hit": 1}, nil, nil},
		{"the gateway's 429", "openai", middleware.Record{TenantID: "t1", Request: asked("gpt-4o"), Cache: "bypass", RateLimited: true},
			http.StatusTooManyRequests, map[string]float64{"t1,openai,gpt-4o,429": 1}, map[string]float64{"t1,openai,bypass": 1},
			map[string]float64{"t1,openai": 1}, nil},
		{"a provider's 429", "openai", middleware.Record{TenantID: "t1", Request: asked("gpt-4o"), Cache: "bypass"},
			http.StatusTooManyRequests, map[string]float64{"t1,openai,gpt-4o,429": 1}, map[string]float64{"t1,openai,bypass": 1}, nil, nil},
		{"a bad X-Gateway-Cache", "openai", middleware.Record{TenantID: "t1", Request: asked("gpt-4o")}, http.StatusBadRequest,
			map[string]float64{"t1,openai,gpt-4o,400": 1}, nil, nil, nil},
		{"a failed lookup", "openai", middleware.Record{TenantID: "t1", Request: asked("gpt-4o"), Cache: "error"}, http.StatusOK,
			map[string]float64{"t1,openai,gpt-4o,200": 1}, map[string]float64{"t1,openai,error": 1}, nil, nil},
		{"an unknown model", "openai", middleware.Record{TenantID: "t1", Request: asked("gpt-9"), Cache: "bypass",
			BudgetRefusal: &middleware.BudgetRefusal{Provider: "openai", Reason: "unknown_model"}}, http.StatusBadRequest,
			map[string]float64{"t1,openai,other,400": 1}, map[string]float64{"t1,openai,bypass": 1}, nil,
			map[string]float64{"t1,openai,unknown_model": 1}},
		// The refusal names the fallback's provider, the rest the route.
		{"a fallback refused", "ollama", middleware.Record{TenantID: "t1", Request: asked("qwen3.5:9b"), Cache: "bypass",
			BudgetRefusal: &middleware.BudgetRefusal{Provider: "groq", Reason: "exceeded"}}, http.StatusPaymentRequired,
			map[string]float64{"t1,ollama,other,402": 1}, map[string]float64{"t1,ollama,bypass": 1}, nil,
			map[string]float64{"t1,groq,exceeded": 1}},
		{"a client gone during the budget check", "openai", middleware.Record{TenantID: "t1", Request: asked("gpt-4o"), Cache: "bypass"}, 499,
			map[string]float64{"t1,openai,gpt-4o,499": 1}, map[string]float64{"t1,openai,bypass": 1}, nil, nil},
		// Looked up in the route's provider's prices, not another's.
		{"priced by another provider", "groq", middleware.Record{TenantID: "t1", Request: asked("gpt-4o"), Cache: "bypass"}, http.StatusOK,
			map[string]float64{"t1,groq,other,200": 1}, map[string]float64{"t1,groq,bypass": 1}, nil, nil},
		{"no model named", "openai", middleware.Record{TenantID: "t1", Request: asked(""), Cache: "bypass"}, http.StatusOK,
			map[string]float64{"t1,openai,other,200": 1}, map[string]float64{"t1,openai,bypass": 1}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New()
			serveRecorded(m, tc.route, tc.record, tc.status)

			checkCounter(t, "gateway_requests_total", m.requests, tc.requests)
			checkCounter(t, "gateway_cache_requests_total", m.cache, tc.cache)
			checkCounter(t, "gateway_ratelimit_rejections_total", m.rateLimits, tc.rateLimits)
			checkCounter(t, "gateway_budget_rejections_total", m.budgetRefusals, tc.budget)
		})
	}
}

// TestDurationBucketsStartAt1ms: a cache hit takes about 1 ms, so the first
// bucket is 1 ms.
func TestDurationBucketsStartAt1ms(t *testing.T) {
	m := New()
	m.Requests("ollama")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	const first = `gateway_request_duration_seconds_bucket{provider="ollama",le="0.001"} `
	for line := range strings.Lines(scrape(t, m)) {
		if strings.HasPrefix(line, "gateway_request_duration_seconds_bucket{") {
			if !strings.HasPrefix(line, first) {
				t.Errorf("first bucket %q, want %q", strings.TrimSpace(line), first+"…")
			}
			return
		}
	}
	t.Error("no gateway_request_duration_seconds bucket on /metrics")
}

// TestHistogramsCarryNoTenantOrModel: a histogram has provider as its only
// label, whatever the request's Record says, since each label value
// multiplies its buckets.
func TestHistogramsCarryNoTenantOrModel(t *testing.T) {
	m := New()
	serveRecorded(m, "openai", middleware.Record{TenantID: "t1", Request: &usage.Request{Model: "gpt-4o"}, Cache: "miss"}, http.StatusOK)
	m.ConcurrencyWait("openai")(time.Millisecond)

	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string][]string{}
	for _, f := range families {
		if f.GetType().String() != "HISTOGRAM" {
			continue
		}
		for _, s := range f.GetMetric() {
			for _, l := range s.GetLabel() {
				labels[f.GetName()] = append(labels[f.GetName()], l.GetName())
			}
		}
	}
	want := map[string][]string{"gateway_request_duration_seconds": {"provider"}, "gateway_concurrency_wait_seconds": {"provider"}}
	if !maps.EqualFunc(labels, want, slices.Equal) {
		t.Errorf("histograms' labels = %v, want %v", labels, want)
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

// TestUsage: each type of token adds up across reports, by tenant, provider
// and the table's name for the model, the response's or else the requested
// one; a type with no tokens gets no series. A known cost above 0 adds up
// the same way, and an unknown cost or 0 adds none.
func TestUsage(t *testing.T) {
	m := New()
	cost, zero := int64(144), int64(0)
	m.Usage("t1", "openai", usage.Usage{Model: "gpt-4o-2024-08-06", Input: 10, CachedInput: 5, Output: 7}, "gpt-4o", &cost)
	// A response that names no model.
	m.Usage("t1", "openai", usage.Usage{Input: 1, Output: 1}, "gpt-4o-2024-08-06", &cost)
	// A cache hit: no tokens, and cost 0.
	m.Usage("t1", "openai", usage.Usage{Model: "gpt-4o"}, "gpt-4o", &zero)
	// A response cut short: its cost isn't known.
	m.Usage("t1", "openai", usage.Usage{Model: "gpt-4o", Output: 2}, "gpt-4o", nil)
	m.Usage("t1", "anthropic", usage.Usage{Model: "claude-sonnet-4-20250514", Input: 3, CacheWrite: 100, CacheWrite1h: 200}, "claude-sonnet-4", &cost)
	// A snapshot priced on its own keeps its name. A model the table
	// doesn't price, and a free provider's, are other.
	m.Usage("t2", "openai", usage.Usage{Model: "gpt-4o-2024-05-13", Output: 3}, "gpt-4o-2024-05-13", &cost)
	m.Usage("t2", "groq", usage.Usage{Model: "llama-3.3-70b-versatile", Output: 4}, "llama-3.3-70b-versatile", nil)
	m.Usage("t2", "ollama", usage.Usage{Model: "qwen3.5:9b", Input: 19, Output: 1}, "qwen3.5:9b", &zero)
	// A provider reporting more cached tokens than prompt tokens must not
	// panic the gateway.
	m.Usage("t1", "openai", usage.Usage{Model: "gpt-4o", Input: -3}, "gpt-4o", nil)

	checkCounter(t, "gateway_tokens_total", m.tokens, map[string]float64{
		"t1,openai,gpt-4o,input":                      11,
		"t1,openai,gpt-4o,cached_input":               5,
		"t1,openai,gpt-4o,output":                     10,
		"t1,anthropic,claude-sonnet-4,input":          3,
		"t1,anthropic,claude-sonnet-4,cache_write":    100,
		"t1,anthropic,claude-sonnet-4,cache_write_1h": 200,
		"t2,openai,gpt-4o-2024-05-13,output":          3,
		"t2,groq,other,output":                        4,
		"t2,ollama,other,input":                       19,
		"t2,ollama,other,output":                      1,
	})
	checkCounter(t, "gateway_cost_micros_total", m.cost, map[string]float64{
		"t1,openai,gpt-4o":             288,
		"t1,anthropic,claude-sonnet-4": 144,
		"t2,openai,gpt-4o-2024-05-13":  144,
	})
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
