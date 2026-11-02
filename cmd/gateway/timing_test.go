package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/mockprovider"
)

// timing returns, from text, a scrape of /metrics, the sum in seconds of
// each provider's observations of the histogram name, and how many there
// were.
func timing(t *testing.T, text, name string) (sums, counts map[string]float64) {
	t.Helper()
	return samples(t, text, name+"_sum", "provider"), samples(t, text, name+"_count", "provider")
}

// TestTimingThroughGateway: an upstream that waitCounts 200 ms before its headers
// has a TTFB of 200 to 250 ms, and the gateway's overhead before the handoff
// is well under 50 ms. Each is observed once, under the route.
func TestTimingThroughGateway(t *testing.T) {
	chat := ollamaFixture(t, "chat.json")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chat)
	}))
	t.Cleanup(upstream.Close)
	r := routerFor(t, fmt.Sprintf(ollamaConfig, upstream.URL), nil, nil)

	if rec := serve(r, chatRequest("ollama", "qwen3.5:9b")); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body)
	}

	got := scrapeMetrics(t, r.metrics)
	ttfb, ttfbCounts := timing(t, got, "gateway_upstream_ttfb_seconds")
	overhead, overheadCounts := timing(t, got, "gateway_overhead_seconds")
	checkSamples(t, "gateway_upstream_ttfb_seconds_count", ttfbCounts, map[string]float64{"ollama": 1})
	checkSamples(t, "gateway_overhead_seconds_count", overheadCounts, map[string]float64{"ollama": 1})
	t.Logf("TTFB %.1f ms, overhead %.2f ms", ttfb["ollama"]*1000, overhead["ollama"]*1000)
	if s := ttfb["ollama"]; s < 0.2 || s > 0.25 {
		t.Errorf("TTFB = %v s, want 0.2 to 0.25", s)
	}
	if s := overhead["ollama"]; s >= 0.05 {
		t.Errorf("overhead = %v s, want well under 0.05", s)
	}
}

// TestTimingLeavesOutTheConcurrencyWait: with max_concurrency 1, a second
// request waitCounts about 300 ms for the slot the first holds. The wait is in
// gateway_concurrency_wait_seconds, and in neither the second's TTFB nor its
// overhead.
func TestTimingLeavesOutTheConcurrencyWait(t *testing.T) {
	ollama := newFakeOllama(t, ollamaKnobs{hold: true})
	r := routerFor(t, fmt.Sprintf(ollamaConfig+"    max_concurrency: 1\n", ollama.server.URL), nil, nil)

	first := serveAsync(r, chatRequest("ollama", "qwen3.5:9b"))
	receive(t, ollama.arrivals, "the first request at Ollama")
	second := serveAsync(r, chatRequest("ollama", "qwen3.5:9b"))
	time.Sleep(300 * time.Millisecond)
	ollama.release()
	for _, done := range []<-chan *httptest.ResponseRecorder{first, second} {
		if rec := receive(t, done, "a response"); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body)
		}
	}

	got := scrapeMetrics(t, r.metrics)
	wait, waitCounts := timing(t, got, "gateway_concurrency_wait_seconds")
	ttfb, ttfbCounts := timing(t, got, "gateway_upstream_ttfb_seconds")
	overhead, overheadCounts := timing(t, got, "gateway_overhead_seconds")
	checkSamples(t, "gateway_concurrency_wait_seconds_count", waitCounts, map[string]float64{"ollama": 2})
	checkSamples(t, "gateway_upstream_ttfb_seconds_count", ttfbCounts, map[string]float64{"ollama": 2})
	checkSamples(t, "gateway_overhead_seconds_count", overheadCounts, map[string]float64{"ollama": 2})
	t.Logf("both requests: wait %.1f ms, TTFB %.1f ms, overhead %.2f ms", wait["ollama"]*1000, ttfb["ollama"]*1000, overhead["ollama"]*1000)
	// The first request waited for no slot, and the second about 300 ms.
	if s := wait["ollama"]; s < 0.25 || s > 0.35 {
		t.Errorf("wait = %v s, want about 0.3", s)
	}
	// The first's TTFB is the 300 ms hold, and the second's a few ms. With
	// the wait in it, the second's would be 300 ms too.
	if s := ttfb["ollama"]; s < 0.3 || s > 0.35 {
		t.Errorf("TTFB = %v s, want 0.3 to 0.35", s)
	}
	// With the wait in it, the second's overhead would be 300 ms.
	if s := overhead["ollama"]; s >= 0.05 {
		t.Errorf("overhead = %v s, want well under 0.05", s)
	}
}

// fallbackTimingConfig is a config file with one route, ollama, to the URL
// it is formatted with first, falling back for qwen3.5:9b to groq, the mock
// at the second.
const fallbackTimingConfig = `
providers:
  ollama:
    url: %s
    auth: none
    format: openai
    fallback:
      enabled: true
      provider: groq
      models:
        "qwen3.5:9b": qwen/qwen3.8-27b
  groq:
    url: %s/openai
    key: ${TEST_MOCK_KEY}
    auth: bearer
    format: openai
`

// TestTimingAcrossAFallback: a request whose first attempt fails is one
// request to the overhead, observed once under the route. Each attempt that
// got a response has a TTFB under its own provider: after a 5xx both have
// one, and after a failed dial only the fallback has.
func TestTimingAcrossAFallback(t *testing.T) {
	failing := newFakeOllama(t, ollamaKnobs{status: http.StatusInternalServerError})
	mock := httptest.NewServer(mockprovider.Handler())
	t.Cleanup(mock.Close)
	t.Setenv("TEST_MOCK_KEY", "mock")

	for _, tc := range []struct {
		name, ollama string
		ttfbCounts   map[string]float64
	}{
		{"a 5xx", failing.server.URL, map[string]float64{"ollama": 1, "groq": 1}},
		{"a failed dial", "http://127.0.0.1:1", map[string]float64{"ollama": 0, "groq": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := routerFor(t, fmt.Sprintf(fallbackTimingConfig, tc.ollama, mock.URL), nil, nil)

			rec := serve(r, chatRequest("ollama", "qwen3.5:9b"))
			if rec.Code != http.StatusOK || rec.Header().Get("X-Gateway-Fallback") != "true" {
				t.Fatalf("status %d, X-Gateway-Fallback %q; want 200 from the fallback; body %s",
					rec.Code, rec.Header().Get("X-Gateway-Fallback"), rec.Body)
			}

			got := scrapeMetrics(t, r.metrics)
			_, overheadCounts := timing(t, got, "gateway_overhead_seconds")
			_, ttfbCounts := timing(t, got, "gateway_upstream_ttfb_seconds")
			checkSamples(t, "gateway_overhead_seconds_count", overheadCounts, map[string]float64{"ollama": 1})
			checkSamples(t, "gateway_upstream_ttfb_seconds_count", ttfbCounts, tc.ttfbCounts)
		})
	}
}

// TestTTFBOfAStream: a stream's TTFB is the time to its headers, not to its
// end. The upstream sends its headers and first event at once, and the rest
// 300 ms later.
func TestTTFBOfAStream(t *testing.T) {
	stream := ollamaFixture(t, "chat_stream.sse")
	// The end of the first event.
	split := bytes.Index(stream, []byte("\n\n")) + 2
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream[:split])
		_ = http.NewResponseController(w).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write(stream[split:])
	}))
	t.Cleanup(upstream.Close)
	r := routerFor(t, fmt.Sprintf(ollamaConfig, upstream.URL), nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/ollama/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3.5:9b","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer "+testTenantKey)
	if rec := serve(r, req); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), stream) {
		t.Fatalf("status %d, body %q; want 200 and the whole stream", rec.Code, rec.Body)
	}

	got := scrapeMetrics(t, r.metrics)
	ttfb, ttfbCounts := timing(t, got, "gateway_upstream_ttfb_seconds")
	duration, _ := timing(t, got, "gateway_request_duration_seconds")
	checkSamples(t, "gateway_upstream_ttfb_seconds_count", ttfbCounts, map[string]float64{"ollama": 1})
	t.Logf("TTFB %.1f ms, the whole stream %.1f ms", ttfb["ollama"]*1000, duration["ollama"]*1000)
	if s := duration["ollama"]; s < 0.3 {
		t.Errorf("the request took %v s, want 0.3 or more: the stream runs 300 ms past its headers", s)
	}
	if s := ttfb["ollama"]; s >= 0.1 {
		t.Errorf("TTFB = %v s, want under 0.1: the time to the stream's headers, not to its end", s)
	}
}

// TestTimingSkipsCacheHitsAndRefusals: a cache hit and the gateway's own 429
// never reach a provider, so neither is timed. The miss before them is.
func TestTimingSkipsCacheHitsAndRefusals(t *testing.T) {
	upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
	limiter := &fakeLimiter{allowed: true}
	r := cachedRouter(t, upstream, &fakeCache{}, limiter, discardLogger)

	for _, want := range []string{"miss", "hit"} {
		if rec := serve(r, openaiChat(zeroTemperature)); rec.Code != http.StatusOK || verdict(rec) != want {
			t.Fatalf("status %d, X-Gateway-Cache %q; want 200, %s", rec.Code, verdict(rec), want)
		}
	}
	limiter.mu.Lock()
	limiter.allowed, limiter.wait = false, time.Second
	limiter.mu.Unlock()
	// Another body, as a hit is served even while the limiter refuses.
	if rec := serve(r, openaiChat(`{"model":"gpt-4o","temperature":0,"messages":[{"role":"user","content":"bye"}]}`)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d requests reached the provider, want the miss alone", n)
	}

	got := scrapeMetrics(t, r.metrics)
	_, overheadCounts := timing(t, got, "gateway_overhead_seconds")
	_, ttfbCounts := timing(t, got, "gateway_upstream_ttfb_seconds")
	checkSamples(t, "gateway_overhead_seconds_count", overheadCounts, map[string]float64{"openai": 1})
	checkSamples(t, "gateway_upstream_ttfb_seconds_count", ttfbCounts, map[string]float64{"openai": 1, "gemini": 0})
}
