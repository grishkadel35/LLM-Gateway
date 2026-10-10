package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/metrics"
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
}

func newFakeOllama(t *testing.T) *fakeOllama {
	t.Helper()

	chat := ollamaFixture(t, "chat.json")
	f := &fakeOllama{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Like Ollama: a Content-Length (net/http adds it for a body this
		// small) and no request ID.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chat)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
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
	r, err := router(cfg, fakeStore{}, testAdminKey, onUsage, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	ollama := newFakeOllama(t)
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
