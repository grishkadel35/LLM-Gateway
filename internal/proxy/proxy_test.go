package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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

// newTestProxy builds a proxy pointed at upstreamURL, failing the test if the
// constructor errors.
func newTestProxy(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()

	p, err := New(upstreamURL, 5*time.Second, discardLogger())
	if err != nil {
		t.Fatalf("New(%q) returned error: %v", upstreamURL, err)
	}
	return p
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
	// The whole point of a transparent gateway: the caller's credentials reach
	// the provider unchanged.
	if gotAuth != "Bearer sk-test-123" {
		t.Errorf("upstream Authorization = %q, want %q", gotAuth, "Bearer sk-test-123")
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
// prefix, e.g. https://host/openai/v1 in front of an Azure-style deployment.
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

// TestNewRejectsBadUpstreamURL checks the constructor's validation.
//
// Go note: this is a "table-driven test", the idiomatic Go pattern for testing
// many inputs. t.Run creates a named subtest per case, so a failure names the
// case that broke.
func TestNewRejectsBadUpstreamURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"no scheme", "api.openai.com"},
		{"scheme only", "https://"},
		{"control character", "https://exa\x7fmple.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.url, time.Second, discardLogger()); err == nil {
				t.Errorf("New(%q) = nil error, want an error", tc.url)
			}
		})
	}
}
