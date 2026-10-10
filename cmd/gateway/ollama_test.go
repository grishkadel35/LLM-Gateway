package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/metrics"
	"github.com/grishkadel/llm-gateway/internal/middleware"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// ollamaFixture reads one of the replies captured from Ollama 0.40.2.
func ollamaFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "usage", "testdata", "ollama", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeOllama answers like Ollama's OpenAI-compatible API, with the replies
// captured from the real thing.
type fakeOllama struct {
	server *httptest.Server
	// arrivals receives a value as each chat request arrives.
	arrivals chan struct{}
	// release lets held chat requests finish; see ollamaKnobs.hold.
	release func()
}

// ollamaKnobs set how a fakeOllama behaves. They are fixed before it starts
// serving, so its handlers read them without locks.
type ollamaKnobs struct {
	// hold keeps each chat request waiting until release is called.
	hold bool
	// status, when not 0, answers each chat request with it and an error.
	status int
}

func newFakeOllama(t *testing.T, knobs ollamaKnobs) *fakeOllama {
	t.Helper()

	chat := ollamaFixture(t, "chat.json")
	held := make(chan struct{})
	var once sync.Once
	f := &fakeOllama{
		arrivals: make(chan struct{}, 16),
		release:  func() { once.Do(func() { close(held) }) },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.arrivals <- struct{}{}
		if knobs.hold {
			<-held
		}
		// Like Ollama: a Content-Length (net/http adds it for a body this
		// small) and no request ID.
		w.Header().Set("Content-Type", "application/json")
		if knobs.status != 0 {
			w.WriteHeader(knobs.status)
			_, _ = io.WriteString(w, `{"error":{"message":"model runner failed","type":"api_error","param":null,"code":null}}`)
			return
		}
		_, _ = w.Write(chat)
	})
	f.server = httptest.NewServer(mux)
	// Cleanups run last-registered first, so held requests are let go before
	// Close, which waits for them.
	t.Cleanup(f.server.Close)
	t.Cleanup(f.release)
	return f
}

// ollamaConfig is a config file with one provider, ollama, at the URL it is
// formatted with.
const ollamaConfig = `
providers:
  ollama:
    url: %s
    auth: none
    format: openai
`

// routerFor builds the gateway from a config file's text, over fakeStore.
func routerFor(t *testing.T, yaml string, onUsage usageFunc) http.Handler {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load() returned error: %v", err)
	}
	r, err := router(cfg, fakeStore{}, testAdminKey, onUsage, metrics.New(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("router() returned error: %v", err)
	}
	return r
}

// chatRequest is a tenant's non-streaming chat request on route.
func chatRequest(route, model string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/"+route+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer "+testTenantKey)
	return req
}

// serveAsync serves req on r in the background. The channel delivers the
// response once it is done.
func serveAsync(r http.Handler, req *http.Request) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		done <- rec
	}()
	return done
}

// receive waits for a value from ch, and fails the test if none arrives in
// 5 seconds.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// scrapeMetrics returns GET /metrics, which needs no key.
func scrapeMetrics(t *testing.T, r http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: status = %d, want %d", rec.Code, http.StatusOK)
	}
	return rec.Body.String()
}

// TestMetricsThroughGateway: requests through the gateway show on /metrics,
// counted by the status the client got, refused ones included, with the
// tokens the provider reported.
func TestMetricsThroughGateway(t *testing.T) {
	ollama := newFakeOllama(t, ollamaKnobs{})
	r := routerFor(t, fmt.Sprintf(ollamaConfig, ollama.server.URL), nil)

	r.ServeHTTP(httptest.NewRecorder(), chatRequest("ollama", "qwen3.5:9b"))
	refused := chatRequest("ollama", "qwen3.5:9b")
	refused.Header.Set("Authorization", "Bearer gw_not-a-key")
	r.ServeHTTP(httptest.NewRecorder(), refused)

	got := scrapeMetrics(t, r)
	for _, want := range []string{
		`gateway_requests_total{provider="ollama",status="200"} 1`,
		`gateway_requests_total{provider="ollama",status="401"} 1`,
		`gateway_request_duration_seconds_count{provider="ollama"} 2`,
		// chat.json's usage: 19 prompt tokens, none cached, 1 completion token.
		`gateway_tokens_total{provider="ollama",type="input"} 19`,
		`gateway_tokens_total{provider="ollama",type="output"} 1`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	if strings.Contains(got, `type="cached_input"`) {
		t.Error("a token type with no tokens got a series")
	}
}

// TestConcurrencyCapQueuesRequests: with max_concurrency 1, a second request
// reaches Ollama only once the first has finished, and both succeed.
func TestConcurrencyCapQueuesRequests(t *testing.T) {
	ollama := newFakeOllama(t, ollamaKnobs{hold: true})
	r := routerFor(t, fmt.Sprintf(ollamaConfig+"    max_concurrency: 1\n", ollama.server.URL), nil)

	first := serveAsync(r, chatRequest("ollama", "qwen3.5:9b"))
	receive(t, ollama.arrivals, "the first request at Ollama")
	second := serveAsync(r, chatRequest("ollama", "qwen3.5:9b"))

	select {
	case <-ollama.arrivals:
		t.Fatal("the second request reached Ollama while the first held the only slot")
	case <-time.After(100 * time.Millisecond):
	}

	ollama.release()
	for _, done := range []<-chan *httptest.ResponseRecorder{first, second} {
		if rec := receive(t, done, "a response"); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body)
		}
	}
	receive(t, ollama.arrivals, "the second request at Ollama")
}

// TestConcurrencyCapTimesOut: a request that gets no slot within
// queue_timeout is answered 503 provider_busy with Retry-After: 1 and never
// reaches Ollama, and every wait, granted or not, is observed.
func TestConcurrencyCapTimesOut(t *testing.T) {
	ollama := newFakeOllama(t, ollamaKnobs{hold: true})
	r := routerFor(t, fmt.Sprintf(ollamaConfig+"    max_concurrency: 1\n    queue_timeout: 1\n", ollama.server.URL), nil)

	first := serveAsync(r, chatRequest("ollama", "qwen3.5:9b"))
	receive(t, ollama.arrivals, "the first request at Ollama")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, chatRequest("ollama", "qwen3.5:9b"))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("status %d, Retry-After %q; want 503, 1", rec.Code, rec.Header().Get("Retry-After"))
	}
	var body struct {
		Error struct{ Type, Provider string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if body.Error.Type != "provider_busy" || body.Error.Provider != "ollama" {
		t.Errorf("error = %+v, want provider_busy from ollama", body.Error)
	}

	ollama.release()
	if rec := receive(t, first, "the first response"); rec.Code != http.StatusOK {
		t.Errorf("first request: status = %d, want %d", rec.Code, http.StatusOK)
	}
	select {
	case <-ollama.arrivals:
		t.Error("the refused request reached Ollama")
	default:
	}

	got := scrapeMetrics(t, r)
	for _, want := range []string{
		`gateway_concurrency_wait_seconds_count{provider="ollama"} 2`,
		`gateway_requests_total{provider="ollama",status="503"} 1`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}

// TestFallbackThroughGateway: with Ollama answering 500, a request for a
// mapped model is served by groq through the whole gateway. The client gets
// groq's answer and the headers saying so; groq gets the mapped model under
// its own key; each attempt leaves its usage under the one request ID; and
// the fallback is counted.
func TestFallbackThroughGateway(t *testing.T) {
	ollama := newFakeOllama(t, ollamaKnobs{status: http.StatusInternalServerError})
	groq := newFakeUpstream(t)
	t.Setenv("TEST_GROQ_KEY", "sk-groq")

	var calls []meteredCall
	var ids []string
	r := routerFor(t, fmt.Sprintf(`
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
    key: ${TEST_GROQ_KEY}
    auth: bearer
    format: openai
`, ollama.server.URL, groq.server.URL), func(ctx context.Context, provider string, r usage.Result) {
		calls = append(calls, meteredCall{provider, r})
		ids = append(ids, middleware.RequestIDFrom(ctx))
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, chatRequest("ollama", "qwen3.5:9b"))

	if rec.Code != http.StatusOK || rec.Header().Get("X-Gateway-Provider") != "groq" || rec.Header().Get("X-Gateway-Fallback") != "true" {
		t.Errorf("status %d, X-Gateway-Provider %q, X-Gateway-Fallback %q; want 200 from groq, true",
			rec.Code, rec.Header().Get("X-Gateway-Provider"), rec.Header().Get("X-Gateway-Fallback"))
	}
	if groq.path != "/openai/v1/chat/completions" || groq.header.Get("Authorization") != "Bearer sk-groq" ||
		!strings.Contains(groq.body, `"model":"qwen/qwen3.8-27b"`) {
		t.Errorf("groq got %s with Authorization %q and body %s; want the mapped model under its own key",
			groq.path, groq.header.Get("Authorization"), groq.body)
	}
	if len(calls) != 2 || calls[0].provider != "ollama" || calls[0].result.Status != http.StatusInternalServerError ||
		calls[1].provider != "groq" || calls[1].result.Status != http.StatusOK {
		t.Errorf("usage callbacks = %+v, want ollama's 500, then groq's 200", calls)
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Errorf("request IDs = %q, want one ID for both attempts", ids)
	}

	got := scrapeMetrics(t, r)
	for _, want := range []string{
		`gateway_fallbacks_total{from="ollama",reason="server_error",to="groq"} 1`,
		// The route the client called, with the status it got.
		`gateway_requests_total{provider="ollama",status="200"} 1`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}
