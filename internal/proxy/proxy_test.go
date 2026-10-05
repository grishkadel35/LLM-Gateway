package proxy

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/provider"
	"github.com/grishkadel/llm-gateway/internal/usage"
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

	return New(testProvider(t, upstreamURL), discardLogger(), nil)
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

// TestProxyDoesNotForwardClientIdentity checks no X-Forwarded-* header reaches
// the provider, neither one the gateway would add nor one the client sent.
// Providers are third parties with no use for our clients' addresses.
func TestProxyDoesNotForwardClientIdentity(t *testing.T) {
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
	req.Header.Set("X-Forwarded-For", "198.51.100.1")

	p.ServeHTTP(httptest.NewRecorder(), req)

	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		if v := got.Get(h); v != "" {
			t.Errorf("%s = %q, want it absent", h, v)
		}
	}
}

// TestProxyIgnoresConnectionHeaderNamingCredentials guards against a client
// listing the gateway's own headers as hop-by-hop. "Connection: X-Api-Key"
// tells a proxy to drop X-Api-Key; if that happened after the gateway set the
// provider key, the upstream would receive no credential at all.
func TestProxyIgnoresConnectionHeaderNamingCredentials(t *testing.T) {
	var got http.Header

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := testProvider(t, upstream.URL)
	p.Auth = provider.AuthAPIKey
	p.Headers = map[string]string{"anthropic-version": "2023-06-01"}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Connection", "X-Api-Key, Anthropic-Version")

	New(p, discardLogger(), nil).ServeHTTP(httptest.NewRecorder(), req)

	if v := got.Get("X-Api-Key"); v != "sk-gateway-key" {
		t.Errorf("X-Api-Key = %q, want %q", v, "sk-gateway-key")
	}
	if v := got.Get("Anthropic-Version"); v != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want %q", v, "2023-06-01")
	}
}

// TestProxyStripsQueryCredential checks a key sent as ?key= (Gemini's URL
// form) never reaches the upstream, while the rest of the query does.
func TestProxyStripsQueryCredential(t *testing.T) {
	var gotQuery string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := testProvider(t, upstream.URL)
	p.Auth = provider.AuthGoogleKey

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/x:streamGenerateContent?key=gw_CLIENT-LEAK&alt=sse", nil)
	New(p, discardLogger(), nil).ServeHTTP(httptest.NewRecorder(), req)

	if gotQuery != "alt=sse" {
		t.Errorf("upstream query = %q, want %q", gotQuery, "alt=sse")
	}
}

// TestProxyDecompressesUpstreamResponse checks the body leaving the gateway is
// plain bytes even when the client asked for gzip. Usage metering reads the
// body, and can't parse a compressed one.
func TestProxyDecompressesUpstreamResponse(t *testing.T) {
	const wantBody = `{"usage":{"prompt_tokens":20}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = io.WriteString(w, wantBody)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, wantBody)
		_ = gz.Close()
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	rec := httptest.NewRecorder()
	newTestProxy(t, upstream.URL).ServeHTTP(rec, req)

	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want none", ce)
	}
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
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

// TestProxyTimeoutReturns504 checks an upstream that never starts replying
// produces a gateway timeout, not the 502 used for unreachable upstreams.
func TestProxyTimeoutReturns504(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer upstream.Close()
	// Unblock the handler before Close, which waits for it to return.
	defer close(release)

	p := testProvider(t, upstream.URL)
	p.Timeout = 50 * time.Millisecond

	rec := httptest.NewRecorder()
	New(p, discardLogger(), nil).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusGatewayTimeout)
	}
	if !strings.Contains(rec.Body.String(), "upstream_timeout") {
		t.Errorf("body = %q, want an upstream_timeout error", rec.Body.String())
	}
}

// TestProxyClientCancelIsNotAnUpstreamError checks a client that gives up
// before the upstream replies is recorded as 499 and not logged as an error:
// the provider did nothing wrong.
func TestProxyClientCancelIsNotAnUpstreamError(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	var logs strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)

	rec := httptest.NewRecorder()
	New(testProvider(t, upstream.URL), logger, nil).ServeHTTP(rec, req)

	if rec.Code != StatusClientClosedRequest {
		t.Errorf("status = %d, want %d", rec.Code, StatusClientClosedRequest)
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("client cancel was logged as an error: %s", logs.String())
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

			New(p, discardLogger(), nil).ServeHTTP(httptest.NewRecorder(), req)

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
			p := New(testProvider(t, upstream.URL), logger, nil)

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

// TestMeteredStreamIsNotBuffered: with usage metering on, each event must
// still reach the client as the provider sends it. The upstream here holds
// the stream open until the client has received the first event; if metering
// buffered the body, the test would time out.
func TestMeteredStreamIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := testProvider(t, upstream.URL)
	p.Format = provider.FormatOpenAI
	results := make(chan usage.Result, 1)
	gateway := httptest.NewServer(New(p, discardLogger(), func(_ context.Context, r usage.Result) { results <- r }))
	defer gateway.Close()

	res, err := http.Post(gateway.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		unblock()
		t.Fatal(err)
	}
	defer res.Body.Close()
	// Deferred last so it runs first: on a failure the upstream must be
	// released before the servers close, or closing them waits forever.
	defer unblock()

	first := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := res.Body.Read(buf)
		first <- string(buf[:n])
	}()
	select {
	case got := <-first:
		if !strings.Contains(got, `"hi"`) {
			t.Errorf("first read = %q, want the first event", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first event never arrived: the metered stream is buffered")
	}

	unblock()
	_, _ = io.ReadAll(res.Body)
	select {
	case r := <-results:
		if r.Input != 3 || r.Output != 1 || !r.Streamed {
			t.Errorf("result = %+v, want input 3, output 1, streamed", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("usage callback never ran")
	}
}
