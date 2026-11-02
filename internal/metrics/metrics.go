// Package metrics defines the gateway's Prometheus metrics and serves them at
// GET /metrics.
//
// Every metric is registered on the gateway's own registry, not Prometheus'
// global one: router() is built many times in tests, and registering a metric
// twice on one registry panics.
//
// A provider label is a provider's name in config.yaml, and each metric's
// Help says which one: the route the client called, or the provider that
// served an attempt, which after a fallback is the fallback's.
//
// The counters of requests, cache decisions, refusals, tokens and cost also
// carry the tenant, as do the budget gauges, and the counters of requests,
// tokens and cost the model.
// Histograms never carry either: each label value there multiplies about 15
// buckets. Neither label lets a client grow the number of series:
//   - tenant is the tenant's ID, or none for a request refused before a
//     tenant was known (a 401, or 503 auth_unavailable). Tenants are created
//     through the admin API, so their number is the operator's.
//   - model is the price table's name for the model (pricing.Name), or other.
//     Clients choose model names, but only the table's become label values:
//     an unpriced name is other, and so is every model of a provider the
//     table has no prices for, such as a free Ollama, and a request that
//     names none.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/grishkadel/llm-gateway/internal/middleware"
	"github.com/grishkadel/llm-gateway/internal/pricing"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// durationBuckets span requests: from a cache hit, which takes about 1 ms,
// and a refused request to a non-streaming generation of several minutes.
var durationBuckets = []float64{0.001, 0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

// waitBuckets span a wait for a concurrency slot: from none at all to the
// default 30-second queue timeout, and past it for a longer one.
var waitBuckets = []float64{0.001, 0.01, 0.1, 0.5, 1, 2.5, 5, 10, 20, 30, 60}

// ttfbBuckets span the time to a provider's response headers: from a stream's,
// which come as it starts, to a non-streaming generation's, which come once
// it is done, up to the default 600-second provider timeout.
var ttfbBuckets = []float64{0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

// overheadBuckets span the gateway's own work on a request before a provider
// sees it: from a fraction of a millisecond to a budget rebuild, which can
// take up to 2 s.
var overheadBuckets = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}

// Metrics holds every metric the gateway exports.
type Metrics struct {
	registry       *prometheus.Registry
	requests       *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	overhead       *prometheus.HistogramVec
	ttfb           *prometheus.HistogramVec
	tokens         *prometheus.CounterVec
	cost           *prometheus.CounterVec
	cache          *prometheus.CounterVec
	rateLimits     *prometheus.CounterVec
	budgetRefusals *prometheus.CounterVec
	budgetSpent    *prometheus.GaugeVec
	budget         *prometheus.GaugeVec
	wait           *prometheus.HistogramVec
	up             *prometheus.GaugeVec
	fallbacks      *prometheus.CounterVec
}

// New returns the gateway's metrics, on a registry of their own.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Requests on each provider route, by the HTTP status the client received. provider is the route the client called, even when a fallback answered. tenant is the tenant's ID, or none before one was known (a 401, or 503 auth_unavailable); the operator creates tenants. model is the price table's name for the model the client asked of the route's provider, or other, so clients can't add series.",
		}, []string{"tenant", "provider", "model", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "Time to serve a request on each provider route, a streamed body included. provider is the route the client called, even when a fallback answered. No tenant or model label, since each value would multiply the buckets.",
			Buckets: durationBuckets,
		}, []string{"provider"}),
		overhead: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_overhead_seconds",
			Help:    "The gateway's own work on a request before a provider sees it (auth, the body read, the cache lookup, the rate-limit and budget checks): from its arrival on a provider route to its first attempt's handoff to a provider's transport, before any wait for a concurrency slot. Observed once per request handed off, so not for a cache hit or a request refused before then. provider is the route the client called, even when a fallback answered.",
			Buckets: overheadBuckets,
		}, []string{"provider"}),
		ttfb: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_upstream_ttfb_seconds",
			Help:    "Time from each attempt's handoff to its provider's transport, after any wait for a concurrency slot, to the provider's response headers: a stream's come as it starts, a non-streaming response's once it is generated. Only attempts that got a response, of any status, are observed, not a failed dial, a timeout or a client gone. provider is the provider the attempt went to, so after a fallback each attempt counts under its own.",
			Buckets: ttfbBuckets,
		}, []string{"provider"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_tokens_total",
			Help: "Tokens the providers reported, by type. provider is the one that produced them, so after a fallback it is the fallback target. tenant is the tenant's ID; the operator creates tenants. model is the price table's name for the model that priced them, or other, so clients can't add series.",
		}, []string{"tenant", "provider", "model", "type"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_cost_micros_total",
			Help: "What the providers' responses cost in micro-dollars, as their usage rows have it; a response whose cost isn't known adds nothing. provider is the one that produced them, so after a fallback it is the fallback target. tenant is the tenant's ID; the operator creates tenants. model is the price table's name for the model that priced them, so clients can't add series.",
		}, []string{"tenant", "provider", "model"}),
		cache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_cache_requests_total",
			Help: "Requests the response cache looked at, by its decision: hit, miss, bypass, or error for a lookup that failed. provider is the route the client called. tenant is the tenant's ID; the operator creates tenants.",
		}, []string{"tenant", "provider", "result"}),
		rateLimits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_ratelimit_rejections_total",
			Help: "Requests the gateway refused with its own 429 rate_limited; a provider's 429 isn't one. provider is the route the client called. tenant is the tenant's ID; the operator creates tenants.",
		}, []string{"tenant", "provider"}),
		budgetRefusals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_budget_rejections_total",
			Help: "Attempts a tenant's budget refused, by reason: exceeded (402), unknown_model (400) or unavailable (503). provider is the attempt's, so after a fallback it is the fallback target. tenant is the tenant's ID; the operator creates tenants.",
		}, []string{"tenant", "provider", "reason"}),
		budgetSpent: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_budget_spent_micros",
			Help: "Each tenant's spend for the month in micro-dollars, reservations in flight included, as the budget ledger reported it to the tenant's latest request to a paid provider: after its reservation, admitted or refused, or its settlement. A tenant shows up once it sends one after the gateway starts, and one idle since the month began shows last month's spend until its next. tenant is the tenant's ID; the operator creates tenants.",
		}, []string{"tenant"}),
		budget: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_budget_micros",
			Help: "Each tenant's monthly budget in micro-dollars, as the tenant's latest request to a paid provider found it, set with gateway_budget_spent_micros. A new budget shows from the tenant's next such request. tenant is the tenant's ID; the operator creates tenants.",
		}, []string{"tenant"}),
		wait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_concurrency_wait_seconds",
			Help:    "Time requests waited for a concurrency slot on a provider with max_concurrency, whether or not one came free.",
			Buckets: waitBuckets,
		}, []string{"provider"}),
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_provider_up",
			Help: "1 if the provider answered its latest health check with a 2xx, 0 if not. Only providers with a health_path are checked.",
		}, []string{"provider"}),
		fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_fallbacks_total",
			Help: "Requests handed from a failed provider to its fallback, by why it failed: unreachable, timeout or server_error.",
		}, []string{"from", "to", "reason"}),
	}
	m.registry.MustRegister(m.requests, m.duration, m.overhead, m.ttfb, m.tokens, m.cost, m.cache, m.rateLimits, m.budgetRefusals, m.budgetSpent, m.budget, m.wait, m.up, m.fallbacks)
	return m
}

// Handler serves every metric in Prometheus' text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Requests returns middleware that counts and times each request on
// provider's route, by the status the client received.
//
// It goes inside middleware.Logging, and reads the request's Record once the
// route is done: the tenant, the model the client asked for, the cache's
// decision, the gateway's own 429, a budget's refusal, and the tenant's spend
// and budget for the budget gauges. Without a Record, a request counts under
// tenant none and model other, and nothing else.
func (m *Metrics) Requests(provider string) func(http.Handler) http.Handler {
	duration := m.duration.WithLabelValues(provider)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}

			// In a defer, so the request counts even when next panics:
			// ReverseProxy aborts with panic(http.ErrAbortHandler) when a
			// client disconnects mid-stream.
			defer func() {
				m.request(provider, rec.status(), middleware.RecordFrom(r.Context()))
				duration.Observe(time.Since(start).Seconds())
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

// request counts a request on provider's route that ended with status, by
// what record, its Record, says of it: nil for none. It sets the tenant's
// budget gauges to the spend and budget on record, if there are any.
func (m *Metrics) request(provider string, status int, record *middleware.Record) {
	if record == nil {
		record = &middleware.Record{}
	}
	tenant := tenantLabel(record.TenantID)
	asked := ""
	if record.Request != nil {
		asked = record.Request.Model
	}
	m.requests.WithLabelValues(tenant, provider, modelLabel(provider, asked), strconv.Itoa(status)).Inc()
	// The Record leaves these empty for a request refused for a bad
	// X-Gateway-Cache, a provider's own 429, and a client gone during the
	// budget check (499).
	if record.Cache != "" {
		m.cache.WithLabelValues(tenant, provider, record.Cache).Inc()
	}
	if record.RateLimited {
		m.rateLimits.WithLabelValues(tenant, provider).Inc()
	}
	if b := record.BudgetRefusal; b != nil {
		m.budgetRefusals.WithLabelValues(tenant, b.Provider, b.Reason).Inc()
	}
	// Only a paid provider's attempt records a spend, and only when the
	// ledger reported one.
	if s := record.BudgetSpend; s != nil {
		m.budgetSpent.WithLabelValues(tenant).Set(float64(s.SpentMicros))
		m.budget.WithLabelValues(tenant).Set(float64(s.BudgetMicros))
	}
}

// Usage counts one usage report from provider, the provider that produced
// it, for the tenant with ID tenantID: u's tokens by type, and costMicros,
// what it cost, when that is known and above 0.
//
// The model is the price table's name for u's model, else for requested, the
// model the request asked provider for: the names pricing.Cost tries, so it
// names the rate that priced costMicros.
func (m *Metrics) Usage(tenantID, provider string, u usage.Usage, requested string, costMicros *int64) {
	tenant, model := tenantLabel(tenantID), modelLabel(provider, u.Model, requested)
	for _, t := range []struct {
		typ string
		n   int64
	}{
		{"input", u.Input},
		{"cached_input", u.CachedInput},
		{"cache_write", u.CacheWrite},
		{"cache_write_1h", u.CacheWrite1h},
		{"output", u.Output},
	} {
		// Zeros are skipped, so a type a provider never reports gets no
		// series. > rather than != because Add panics on a negative number,
		// and Input is the provider's own subtraction.
		if t.n > 0 {
			m.tokens.WithLabelValues(tenant, provider, model, t.typ).Add(float64(t.n))
		}
	}
	// A cache hit and a free provider's response cost 0, and add no series.
	if costMicros != nil && *costMicros > 0 {
		m.cost.WithLabelValues(tenant, provider, model).Add(float64(*costMicros))
	}
}

// tenantLabel is the tenant label for the tenant with ID id, which is ""
// before a tenant was known.
func tenantLabel(id string) string {
	if id == "" {
		return "none"
	}
	return id
}

// modelLabel is the model label for a model of provider's, named by the
// first of models the price table names it by, or other.
func modelLabel(provider string, models ...string) string {
	if name, ok := pricing.Name(provider, models...); ok {
		return name
	}
	return "other"
}

// ConcurrencyWait returns the function that observes each wait for one of
// provider's concurrency slots.
func (m *Metrics) ConcurrencyWait(provider string) func(time.Duration) {
	wait := m.wait.WithLabelValues(provider)
	return func(d time.Duration) { wait.Observe(d.Seconds()) }
}

// Overhead wraps next, the transport of a provider's proxy, to time the
// gateway's own work on each request: from its Record's Start to its first
// attempt's handoff to a provider's transport, this one or another's, under
// the route on the Record. A fallback's attempt isn't timed again, and a
// request without a Record isn't timed at all.
//
// It goes outside any concurrency limiter, so the wait for a slot isn't
// timed. A request handed to the limiter is timed even if it gets no slot.
func (m *Metrics) Overhead(next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// The proxy's outbound request carries the inbound one's context, and
		// with it the Record.
		if rec := middleware.RecordFrom(req.Context()); rec != nil && !rec.HandedOff {
			// No lock: ReverseProxy calls RoundTrip on the goroutine serving
			// the request, so the Record is still touched from that one alone.
			rec.HandedOff = true
			m.overhead.WithLabelValues(rec.Provider).Observe(time.Since(rec.Start).Seconds())
		}
		return next.RoundTrip(req)
	})
}

// UpstreamTTFB wraps next, the transport of provider's proxy, to time each
// attempt that gets a response, of any status: from its handoff to next to
// the response headers. An attempt that gets none (a failed dial, a timeout,
// a client gone) isn't timed.
//
// It goes inside any concurrency limiter, so the wait for a slot isn't timed.
func (m *Metrics) UpstreamTTFB(provider string, next http.RoundTripper) http.RoundTripper {
	ttfb := m.ttfb.WithLabelValues(provider)
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		start := time.Now()
		resp, err := next.RoundTrip(req)
		if err == nil {
			ttfb.Observe(time.Since(start).Seconds())
		}
		return resp, err
	})
}

// roundTripFunc adapts a function to http.RoundTripper.
//
// Go note: http.RoundTripper is the interface behind each outbound request.
// Its one method, RoundTrip, sends a request and returns as soon as the
// response headers are in, leaving the body to be read after. A wrapper holds
// the next RoundTripper and does its work around next.RoundTrip, as handler
// middleware does around next.ServeHTTP, so timing around the call times the
// wait for the headers, for a stream too. net/http has no adapter like
// http.HandlerFunc for RoundTrippers, so this is one: a function type whose
// RoundTrip method calls the function itself (see middleware.Logging's note
// on http.HandlerFunc).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ProviderUp records the result of provider's latest health check.
func (m *Metrics) ProviderUp(provider string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	m.up.WithLabelValues(provider).Set(v)
}

// Fallback counts a request handed from provider from to its fallback to,
// for reason.
func (m *Metrics) Fallback(from, to, reason string) {
	m.fallbacks.WithLabelValues(from, to, reason).Inc()
}

// UsageWriter registers the usage writer's two metrics, which read the writer
// as each scrape happens: queued, how many rows wait in its queue, and
// dropped, how many rows it has dropped, which must only ever go up. They are
// functions, not the writer, so the writer needs no metrics and the metrics no
// writer. Call it once: registering a metric twice panics.
//
// Go note: prometheus.NewGaugeFunc and NewCounterFunc make metrics that store
// nothing. The registry calls the function every time it is scraped, so the
// number is the writer's own, with no second copy to keep in step, as a
// Gauge's Set or a Counter's Add would need. The function runs on the
// scrape's goroutine while the writer keeps working, so it must be safe to
// call at the same time: len of a channel and an atomic Load both are. A
// counter that goes down reads to Prometheus as a restart, which is why
// dropped must not.
func (m *Metrics) UsageWriter(queued func() int, dropped func() int64) {
	m.registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "gateway_usage_writer_queue_depth",
			Help: "Usage rows waiting in the usage writer's queue for their batch insert, read when scraped. The queue is bounded, and a row that finds it full is dropped (see gateway_usage_writer_dropped_total). A batch being inserted has left the queue and isn't counted.",
		}, func() float64 { return float64(queued()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "gateway_usage_writer_dropped_total",
			Help: "Usage rows the usage writer dropped because its queue was full, or because it had closed, read when scraped. A dropped row never reaches usage_logs. A row Postgres rejects is logged, not counted here.",
		}, func() float64 { return float64(dropped()) }),
	)
}

// statusRecorder remembers the status a handler sent.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	// A 1xx is informational; the final status follows it.
	if r.code == 0 && code >= 200 {
		r.code = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	// Writing without calling WriteHeader first implies a 200.
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// status reports the status sent: 200 for a handler that wrote nothing, as
// net/http then sends that for it.
func (r *statusRecorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

// Unwrap lets http.NewResponseController reach the original writer's Flush,
// as in middleware's responseRecorder: ReverseProxy flushes each streamed event
// through it, and without Unwrap a stream would be buffered.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
