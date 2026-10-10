// Package metrics defines the gateway's Prometheus metrics and serves them at
// GET /metrics.
//
// Every metric is registered on the gateway's own registry, not Prometheus'
// global one: router() is built many times in tests, and registering a metric
// twice on one registry panics. Labels never carry a tenant or a model: clients
// choose model names, so the number of series would be theirs to grow.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/grishkadel/llm-gateway/internal/usage"
)

// durationBuckets span LLM calls: from a refused or tiny request well under a
// second to a non-streaming generation of several minutes.
var durationBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

// waitBuckets span a wait for a concurrency slot: from none at all to the
// default 30-second queue timeout, and past it for a longer one.
var waitBuckets = []float64{0.001, 0.01, 0.1, 0.5, 1, 2.5, 5, 10, 20, 30, 60}

// Metrics holds every metric the gateway exports.
type Metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	tokens   *prometheus.CounterVec
	wait     *prometheus.HistogramVec
}

// New returns the gateway's metrics, on a registry of their own.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Requests on each provider route, by the HTTP status the client received. provider is the route the client called, even when a fallback answered.",
		}, []string{"provider", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "Time to serve a request on each provider route, a streamed body included. provider is the route the client called, even when a fallback answered.",
			Buckets: durationBuckets,
		}, []string{"provider"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_tokens_total",
			Help: "Tokens the providers reported, by type. provider is the one that produced them, so after a fallback it is the fallback target.",
		}, []string{"provider", "type"}),
		wait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_concurrency_wait_seconds",
			Help:    "Time requests waited for a concurrency slot on a provider with max_concurrency, whether or not one came free.",
			Buckets: waitBuckets,
		}, []string{"provider"}),
	}
	m.registry.MustRegister(m.requests, m.duration, m.tokens, m.wait)
	return m
}

// Handler serves every metric in Prometheus' text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Requests returns middleware that counts and times each request on
// provider's route, by the status the client received.
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
				m.requests.WithLabelValues(provider, strconv.Itoa(rec.status())).Inc()
				duration.Observe(time.Since(start).Seconds())
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

// Tokens counts one response's tokens for provider, the provider that
// produced them.
func (m *Metrics) Tokens(provider string, u usage.Usage) {
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
			m.tokens.WithLabelValues(provider, t.typ).Add(float64(t.n))
		}
	}
}

// ConcurrencyWait returns the function that observes each wait for one of
// provider's concurrency slots.
func (m *Metrics) ConcurrencyWait(provider string) func(time.Duration) {
	wait := m.wait.WithLabelValues(provider)
	return func(d time.Duration) { wait.Observe(d.Seconds()) }
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
