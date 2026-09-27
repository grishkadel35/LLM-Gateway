package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// discardLogger returns a logger that throws its output away, so test runs stay
// readable.
//
// Go note: io.Discard is the standard library's /dev/null. A test helper that
// takes *testing.T and calls t.Helper() makes failures point at the caller's
// line rather than at this function.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mustHost pulls the host:port out of a URL, failing the test if it won't parse.
func mustHost(t *testing.T, rawURL string) string {
	t.Helper()

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", rawURL, err)
	}
	return u.Host
}

// testProvider builds a bearer-auth provider pointed at upstreamURL, the way
// the config package would.
func testProvider(t *testing.T, upstreamURL string) provider.Provider {
	t.Helper()

	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", upstreamURL, err)
	}

	return provider.Provider{
		Name:    "testprovider",
		URL:     u,
		Timeout: 5 * time.Second,
		Key:     "sk-gateway-key",
		Auth:    provider.AuthBearer,
	}
}

// newTestProxy builds a proxy pointed at upstreamURL.
func newTestProxy(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()

	return New(testProvider(t, upstreamURL), discardLogger())
}

// TestProxyForwardsRequestAndResponse is the core end-to-end test: stand up a
// fake upstream, point the gateway at it, and check that what the client gets
// back is exactly what the upstream sent.
func TestProxyForwardsRequestAndResponse(t *testing.T) {
	const wantBody = `{"id":"chatcmpl-123","object":"chat.completion"}`

	// These are filled in by the upstream handler and read after the request.
	var (
		gotPath   string
		gotMethod string
		gotAuth   string
		gotBody   string
		gotHost   string
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		// r.Host is the Host header as it arrived on the wire.
		gotHost = r.Host

		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_abc")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, wantBody)
	}))
	// Go note: defer runs when the test function returns, whichever way it
	// returns — including a t.Fatalf further down. That's why cleanup goes right
	// next to the thing being created instead of at the bottom.
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Authorization", "Bearer sk-test-123")
	req.Header.Set("Content-Type", "application/json")

	// httptest.NewRecorder is an in-memory ResponseWriter: no socket needed.
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}

	gotResponseBody, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	if string(gotResponseBody) != wantBody {
		t.Errorf("body = %q, want %q", gotResponseBody, wantBody)
	}

	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	// Upstream response headers must survive the round trip untouched.
	if got := res.Header.Get("X-Request-Id"); got != "req_abc" {
		t.Errorf("X-Request-Id = %q, want %q", got, "req_abc")
	}

	// Now assert on what the upstream actually received.
	if gotMethod != http.MethodPost {
		t.Errorf("upstream method = %q, want %q", gotMethod, http.MethodPost)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want %q", gotPath, "/v1/chat/completions")
	}
	if gotBody != `{"model":"gpt-4"}` {
		t.Errorf("upstream body = %q, want %q", gotBody, `{"model":"gpt-4"}`)
	}
	// The gateway holds the provider credentials now. The client's own key is
	// stripped and replaced with the gateway's — it must never reach upstream.
	if gotAuth == "Bearer sk-test-123" {
		t.Error("upstream received the client's Authorization header; it must be stripped")
	}
	if gotAuth != "Bearer sk-gateway-key" {
		t.Errorf("upstream Authorization = %q, want the gateway's own key %q", gotAuth, "Bearer sk-gateway-key")
	}
	// The Host header must be rewritten to the upstream, not left as the
	// gateway's own host, or providers will route or TLS-verify incorrectly.
	// httptest.NewRequest defaults the incoming Host to "example.com", so this
	// only passes if the Director actually replaced it.
	wantHost := mustHost(t, upstream.URL)
	if gotHost != wantHost {
		t.Errorf("upstream Host = %q, want %q", gotHost, wantHost)
	}
}

// TestProxySetsForwardedHeaders checks the X-Forwarded-* family, including the
// subtle one: ReverseProxy sets X-Forwarded-For itself when a Director is used,
// so it must appear exactly once.
func TestProxySetsForwardedHeaders(t *testing.T) {
	var got http.Header

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "gateway.local"
	req.RemoteAddr = "203.0.113.7:54321"

	p.ServeHTTP(httptest.NewRecorder(), req)

	if xff := got.Get("X-Forwarded-For"); xff != "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q, want %q (set once, by ReverseProxy)", xff, "203.0.113.7")
	}
	if got := got.Get("X-Forwarded-Host"); got != "gateway.local" {
		t.Errorf("X-Forwarded-Host = %q, want %q", got, "gateway.local")
	}
	if got := got.Get("X-Forwarded-Proto"); got != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want %q", got, "http")
	}
}

// TestProxyPreservesUpstreamBasePath covers an upstream URL that carries a path
// prefix. This is the Groq case: its OpenAI-compatible API lives at
// https://api.groq.com/openai, so /groq/v1/models must arrive upstream as
// /openai/v1/models.
func TestProxyPreservesUpstreamBasePath(t *testing.T) {
	var gotPath string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/openai")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	p.ServeHTTP(httptest.NewRecorder(), req)

	if gotPath != "/openai/v1/models" {
		t.Errorf("upstream path = %q, want %q", gotPath, "/openai/v1/models")
	}
}

// TestProxyForwardsUpstreamErrorStatus confirms we relay a provider's own error
// response rather than replacing it. A 429 from OpenAI must arrive as a 429.
func TestProxyForwardsUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "20")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_exceeded"}}`)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusTooManyRequests)
	}
	if got := res.Header.Get("Retry-After"); got != "20" {
		t.Errorf("Retry-After = %q, want %q", got, "20")
	}
}

// TestProxyErrorHandlerOnUnreachableUpstream exercises the ErrorHandler by
// pointing at a server that has already been shut down.
func TestProxyErrorHandlerOnUnreachableUpstream(t *testing.T) {
	// Start a server just to get a port nothing is listening on, then close it.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	p := newTestProxy(t, deadURL)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusBadGateway)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	if len(body) == 0 {
		t.Error("error response body is empty, want a JSON error object")
	}
}

// TestProxyAppliesProviderAuth runs each provider shape end-to-end through the
// proxy and asserts the upstream sees the right credential header — and never
// the client's.
//
// Go note: this is a "table-driven test", the idiomatic Go pattern for testing
// many inputs. t.Run creates a named subtest per case, so a failure names the
// case that broke.
func TestProxyAppliesProviderAuth(t *testing.T) {
	cases := []struct {
		name       string
		auth       provider.AuthStyle
		headers    map[string]string
		wantHeader string
		wantValue  string
	}{
		{
			name:       "openai",
			auth:       provider.AuthBearer,
			wantHeader: "Authorization",
			wantValue:  "Bearer sk-gateway-key",
		},
		{
			name:       "anthropic",
			auth:       provider.AuthAPIKey,
			headers:    map[string]string{"anthropic-version": "2023-06-01"},
			wantHeader: "X-Api-Key",
			wantValue:  "sk-gateway-key",
		},
		{
			name:       "gemini",
			auth:       provider.AuthGoogleKey,
			wantHeader: "X-Goog-Api-Key",
			wantValue:  "sk-gateway-key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got http.Header

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()

			p := testProvider(t, upstream.URL)
			p.Name = tc.name
			p.Auth = tc.auth
			p.Headers = tc.headers

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			// The client sends its own key, which must not survive.
			req.Header.Set("Authorization", "Bearer sk-CLIENT-LEAK")

			New(p, discardLogger()).ServeHTTP(httptest.NewRecorder(), req)

			if v := got.Get(tc.wantHeader); v != tc.wantValue {
				t.Errorf("%s = %q, want %q", tc.wantHeader, v, tc.wantValue)
			}
			for _, h := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
				if got.Get(h) == "Bearer sk-CLIENT-LEAK" || got.Get(h) == "sk-CLIENT-LEAK" {
					t.Errorf("%s leaked the client's credential to the upstream", h)
				}
			}
			for name, want := range tc.headers {
				if v := got.Get(name); v != want {
					t.Errorf("%s = %q, want %q", name, v, want)
				}
			}
		})
	}
}

// TestProxyLogsStreamingFromContentType checks the "streaming" field of the
// upstream-response log line. It must follow the Content-Type, not the
// Content-Length: providers such as Groq send ordinary JSON replies with
// chunked encoding (Content-Length -1), and those aren't streams.
func TestProxyLogsStreamingFromContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		chunked     bool
		want        bool
	}{
		{"json with length", "application/json", false, false},
		{"chunked json", "application/json", true, false},
		{"sse", "text/event-stream", true, true},
		{"sse with charset", "text/event-stream; charset=utf-8", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				_, _ = io.WriteString(w, "{}")
				if tt.chunked {
					// Flushing before the handler returns means the length
					// isn't known yet, so the server switches to chunked.
					w.(http.Flusher).Flush()
				}
			}))
			defer upstream.Close()

			var logs strings.Builder
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			p := New(testProvider(t, upstream.URL), logger)

			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

			var line struct {
				Streaming bool `json:"streaming"`
			}
			if err := json.Unmarshal([]byte(logs.String()), &line); err != nil {
				t.Fatalf("parsing log line %q: %v", logs.String(), err)
			}
			if line.Streaming != tt.want {
				t.Errorf("streaming = %v, want %v (log: %s)", line.Streaming, tt.want, logs.String())
			}
		})
	}
}
