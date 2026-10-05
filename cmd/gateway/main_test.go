package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
	"github.com/grishkadel/llm-gateway/internal/mockprovider"
	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// testTenantKey is the one gateway key fakeStore accepts; testAdminKey is the
// admin key newTestRouter configures.
const (
	testTenantKey = "gw_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testAdminKey  = "admin-test-key-0123456789abcdef0123456789"
)

// fakeStore stands in for tenant.Store so routing tests need no Postgres. It
// knows one tenant with one key.
type fakeStore struct{}

func (fakeStore) Lookup(_ context.Context, key string) (tenant.Tenant, tenant.APIKey, error) {
	if key != testTenantKey {
		return tenant.Tenant{}, tenant.APIKey{}, tenant.ErrNotFound
	}
	return tenant.Tenant{ID: "tn_test"}, tenant.APIKey{ID: 1, TenantID: "tn_test"}, nil
}

func (fakeStore) Create(_ context.Context, name string) (tenant.Tenant, tenant.NewKey, error) {
	return tenant.Tenant{ID: "tn_new", Name: name}, tenant.NewKey{APIKey: tenant.APIKey{ID: 2, TenantID: "tn_new"}}, nil
}

func (fakeStore) IssueKey(context.Context, string) (tenant.NewKey, error) {
	return tenant.NewKey{}, tenant.ErrNotFound
}

func (fakeStore) RevokeKey(context.Context, int64) error { return tenant.ErrNotFound }

// fakeUpstream records what a provider actually received.
type fakeUpstream struct {
	server        *httptest.Server
	path          string
	header        http.Header
	body          string
	contentLength int64
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()

	f := &fakeUpstream{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.path = r.URL.Path
		f.header = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		f.body = string(b)
		f.contentLength = r.ContentLength
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	// Go note: t.Cleanup registers a function to run when the test ends —
	// like defer, but it works from inside a helper.
	t.Cleanup(f.server.Close)
	return f
}

// newTestRouter wires four fake upstreams into a real config and builds the
// gateway's route table from it, exactly as main() would, over fakeStore.
func newTestRouter(t *testing.T) (http.Handler, map[string]*fakeUpstream) {
	t.Helper()
	return newTestRouterWith(t, fakeStore{})
}

func newTestRouterWith(t *testing.T, store tenantStore) (http.Handler, map[string]*fakeUpstream) {
	t.Helper()

	ups := map[string]*fakeUpstream{
		"openai":    newFakeUpstream(t),
		"anthropic": newFakeUpstream(t),
		"gemini":    newFakeUpstream(t),
		"groq":      newFakeUpstream(t),
	}

	// Keys must be environment references; the values are what the upstreams
	// should receive.
	for name := range ups {
		t.Setenv("TEST_"+strings.ToUpper(name)+"_KEY", "sk-"+name)
	}

	// Groq's real API lives under a base path; keep that shape in the fake.
	yaml := fmt.Sprintf(`
providers:
  openai:
    url: %s
    key: ${TEST_OPENAI_KEY}
    auth: bearer
    format: openai
  anthropic:
    url: %s
    key: ${TEST_ANTHROPIC_KEY}
    auth: x-api-key
    format: anthropic
    headers:
      anthropic-version: "2023-06-01"
  gemini:
    url: %s
    key: ${TEST_GEMINI_KEY}
    auth: x-goog-api-key
    format: gemini
  groq:
    url: %s/openai
    key: ${TEST_GROQ_KEY}
    auth: bearer
    format: openai
`, ups["openai"].server.URL, ups["anthropic"].server.URL,
		ups["gemini"].server.URL, ups["groq"].server.URL)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load() returned error: %v", err)
	}

	r, err := router(cfg, store, testAdminKey, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("router() returned error: %v", err)
	}
	return r, ups
}

// TestRoutingReachesTheRightProvider is the core of the multi-provider change:
// each prefix must land on its own upstream, with the prefix stripped and that
// provider's own credential applied in place of the client's gateway key.
func TestRoutingReachesTheRightProvider(t *testing.T) {
	cases := []struct {
		provider   string
		request    string
		wantPath   string
		wantHeader string
		wantValue  string
	}{
		{"openai", "/openai/v1/chat/completions", "/v1/chat/completions", "Authorization", "Bearer sk-openai"},
		{"anthropic", "/anthropic/v1/messages", "/v1/messages", "X-Api-Key", "sk-anthropic"},
		{"gemini", "/gemini/v1beta/models/x:generateContent", "/v1beta/models/x:generateContent", "X-Goog-Api-Key", "sk-gemini"},
		// Groq keeps its base path: /groq/v1/... arrives as /openai/v1/...
		{"groq", "/groq/v1/chat/completions", "/openai/v1/chat/completions", "Authorization", "Bearer sk-groq"},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			r, ups := newTestRouter(t)

			req := httptest.NewRequest(http.MethodPost, tc.request, strings.NewReader(`{"model":"x"}`))
			req.Header.Set("Authorization", "Bearer "+testTenantKey)

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			up := ups[tc.provider]
			if up.path != tc.wantPath {
				t.Errorf("upstream path = %q, want %q", up.path, tc.wantPath)
			}
			if got := up.header.Get(tc.wantHeader); got != tc.wantValue {
				t.Errorf("%s = %q, want %q", tc.wantHeader, got, tc.wantValue)
			}

			// No other provider should have been contacted.
			for name, other := range ups {
				if name != tc.provider && other.path != "" {
					t.Errorf("request also reached provider %q at %q", name, other.path)
				}
			}
		})
	}
}

// TestClientCredentialNeverReachesUpstream is the security property, checked at
// the router level rather than only in the provider unit tests.
func TestClientCredentialNeverReachesUpstream(t *testing.T) {
	r, ups := newTestRouter(t)

	// The gateway key in every credential header the gateway reads: it
	// authenticates, and then none of the three copies may go upstream.
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+testTenantKey)
	req.Header.Set("X-Api-Key", testTenantKey)
	req.Header.Set("X-Goog-Api-Key", testTenantKey)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body)
	}

	for _, h := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
		if got := ups["anthropic"].header.Get(h); strings.Contains(got, testTenantKey) {
			t.Errorf("%s = %q leaked the client's credential upstream", h, got)
		}
	}
	if got := ups["anthropic"].header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want %q", got, "2023-06-01")
	}
}

// TestUnknownProviderReturns404 checks that a typo'd prefix is rejected rather
// than silently forwarded, and that the error names the real providers.
func TestUnknownProviderReturns404(t *testing.T) {
	r, ups := newTestRouter(t)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/openai-typo/v1/chat/completions", nil))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusNotFound)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}

	var body struct {
		Error struct {
			Type      string   `json:"type"`
			Providers []string `json:"providers"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body.Error.Type != "unknown_provider" {
		t.Errorf("error.type = %q, want %q", body.Error.Type, "unknown_provider")
	}
	if len(body.Error.Providers) != 4 {
		t.Errorf("error.providers = %v, want all four configured providers", body.Error.Providers)
	}

	// Critically: no upstream was contacted. A typo must not spend money.
	for name, up := range ups {
		if up.path != "" {
			t.Errorf("unknown prefix still reached provider %q", name)
		}
	}
}

// TestHealthBypassesProviders confirms /health is still served locally now that
// the catch-all route exists.
func TestHealthBypassesProviders(t *testing.T) {
	r, ups := newTestRouter(t)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", got, `{"status":"ok"}`)
	}
	for name, up := range ups {
		if up.path != "" {
			t.Errorf("/health reached provider %q", name)
		}
	}
}

// TestEveryResponseCarriesRequestID checks the wiring: the ID is on routed,
// rejected and failed requests alike, since those are the ones clients quote.
func TestEveryResponseCarriesRequestID(t *testing.T) {
	r, ups := newTestRouter(t)
	// A provider that is down, for the 502 path.
	ups["gemini"].server.Close()

	cases := []struct {
		name, method, path string
		wantStatus         int
	}{
		{"proxied", http.MethodPost, "/openai/v1/chat/completions", http.StatusOK},
		{"unknown provider", http.MethodPost, "/nope/v1/chat/completions", http.StatusNotFound},
		{"upstream down", http.MethodPost, "/gemini/v1beta/models/x:generateContent", http.StatusBadGateway},
		{"rejected by auth", http.MethodPost, "/admin/tenants", http.StatusUnauthorized},
		{"health", http.MethodGet, "/health", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+testTenantKey)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Result().Header.Values("X-Request-ID"); len(got) != 1 || !strings.HasPrefix(got[0], "req_") {
				t.Errorf("X-Request-ID = %q, want one req_ ID", got)
			}
		})
	}
}

// TestProviderRoutesRequireTenantKey: without a valid gateway key, no
// provider is contacted, so nobody can spend the gateway's provider keys.
func TestProviderRoutesRequireTenantKey(t *testing.T) {
	cases := []struct{ name, header, value string }{
		{"no key", "", ""},
		{"unknown gateway key", "Authorization", "Bearer gw_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"},
		{"provider key", "Authorization", "Bearer sk-proj-abc"},
		{"admin key", "Authorization", "Bearer " + testAdminKey},
	}
	paths := []string{
		"/openai/v1/chat/completions",
		"/anthropic/v1/messages",
		"/gemini/v1beta/models/x:generateContent",
		"/groq/v1/chat/completions",
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ups := newTestRouter(t)
			for _, path := range paths {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"x"}`))
				if tc.header != "" {
					req.Header.Set(tc.header, tc.value)
				}
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)

				if rec.Code != http.StatusUnauthorized {
					t.Errorf("%s: status = %d, want %d", path, rec.Code, http.StatusUnauthorized)
				}
			}
			for name, up := range ups {
				if up.path != "" {
					t.Errorf("rejected request reached provider %q", name)
				}
			}
		})
	}
}

// TestAdminRoutesRequireAdminKey: the admin API answers only the admin key.
// Unauthenticated requests get 401 whatever the path or method, so they
// learn nothing about which admin routes exist.
func TestAdminRoutesRequireAdminKey(t *testing.T) {
	r, _ := newTestRouter(t)

	rejected := []struct{ name, method, path, authorization string }{
		{"no key", http.MethodPost, "/admin/tenants", ""},
		{"tenant key", http.MethodPost, "/admin/tenants", "Bearer " + testTenantKey},
		{"wrong method, no key", http.MethodGet, "/admin/tenants", ""},
		{"unknown path, no key", http.MethodGet, "/admin/nope", ""},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"name":"acme"}`))
			if tc.authorization != "" {
				req.Header.Set("Authorization", tc.authorization)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}

	t.Run("admin key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(`{"name":"acme"}`))
		req.Header.Set("Authorization", "Bearer "+testAdminKey)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want %d; body %s", rec.Code, http.StatusCreated, rec.Body)
		}
	})
}

// TestHealthNeedsNoKey: liveness probes carry no credentials.
func TestHealthNeedsNoKey(t *testing.T) {
	r, _ := newTestRouter(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestKeyLifecycleThroughGateway runs the whole flow against Postgres: the
// admin API creates a tenant, its key works on a provider route, the admin
// API revokes it, and the same key is then refused.
func TestKeyLifecycleThroughGateway(t *testing.T) {
	conn := dbtest.New(t)
	if _, err := db.Migrate(context.Background(), conn); err != nil {
		t.Fatalf("Migrate() returned error: %v", err)
	}
	r, ups := newTestRouterWith(t, tenant.NewStore(conn))

	call := func(method, path, authorization, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", authorization)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := call(http.MethodPost, "/admin/tenants", "Bearer "+testAdminKey, `{"name":"acme"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create tenant: status = %d; body %s", rec.Code, rec.Body)
	}
	var created struct {
		Key struct {
			ID        int64  `json:"id"`
			Plaintext string `json:"plaintext"`
		} `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	if rec := call(http.MethodPost, "/openai/v1/chat/completions", "Bearer "+created.Key.Plaintext, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("new key: status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body)
	}

	if rec := call(http.MethodDelete, "/admin/keys/"+strconv.FormatInt(created.Key.ID, 10), "Bearer "+testAdminKey, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	ups["openai"].path = ""
	if rec := call(http.MethodPost, "/openai/v1/chat/completions", "Bearer "+created.Key.Plaintext, `{}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked key: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if ups["openai"].path != "" {
		t.Error("revoked key's request reached the provider")
	}
}

func TestValidateAdminKey(t *testing.T) {
	cases := []struct {
		name, key string
		ok        bool
	}{
		{"good", testAdminKey, true},
		{"unset", "", false},
		{"too short", "admin-0123456789abcdef", false},
		// A tenant key as admin key would make that tenant an admin.
		{"tenant key", testTenantKey, false},
		// From an env file with a trailing newline: it could never be
		// presented exactly, so admin would be locked out with no error.
		{"trailing newline", testAdminKey + "\n", false},
		{"leading space", " " + testAdminKey, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAdminKey(tc.key)
			if (err == nil) != tc.ok {
				t.Errorf("validateAdminKey() = %v, want ok = %v", err, tc.ok)
			}
		})
	}
}

// TestOpenDBFailsFast: a missing or unreachable database stops startup with
// an error, rather than surfacing as 503s on the first requests.
func TestOpenDBFailsFast(t *testing.T) {
	if _, err := openDB(""); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("openDB(\"\") = %v, want an error naming DATABASE_URL", err)
	}
	// Port 1 on loopback: nothing listens there, so the ping fails at once.
	if conn, err := openDB("postgres://gateway:gateway@127.0.0.1:1/gateway?sslmode=disable&connect_timeout=2"); err == nil {
		conn.Close()
		t.Error("openDB() on an unreachable database returned no error")
	}
}

// TestUnmeteredEndpointsAreRefused: tenant requests reach only endpoints the
// gateway can meter, and the check sees the path after the provider prefix,
// so Groq's base path doesn't confuse it.
func TestUnmeteredEndpointsAreRefused(t *testing.T) {
	cases := []struct {
		path       string
		wantStatus int
	}{
		{"/groq/v1/chat/completions", http.StatusOK},
		{"/openai/v1/embeddings", http.StatusForbidden},
		{"/groq/v1/responses", http.StatusForbidden},
		{"/anthropic/v1/messages/count_tokens", http.StatusForbidden},
		{"/gemini/v1beta/models/x:countTokens", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			r, ups := newTestRouter(t)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+testTenantKey)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantStatus == http.StatusForbidden {
				for name, up := range ups {
					if up.path != "" {
						t.Errorf("unmetered request reached provider %q", name)
					}
				}
			}
		})
	}

	// Auth comes first: without a key the answer is 401, so unauthenticated
	// callers can't map which endpoints are metered.
	r, _ := newTestRouter(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/openai/v1/embeddings", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated unmetered request: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestStreamingOpenAIRequestAsksForUsage checks the body rewrite end to end:
// the provider receives include_usage, with a Content-Length that matches
// the rewritten body.
func TestStreamingOpenAIRequestAsksForUsage(t *testing.T) {
	for _, name := range []string{"openai", "groq"} {
		t.Run(name, func(t *testing.T) {
			r, ups := newTestRouter(t)
			req := httptest.NewRequest(http.MethodPost, "/"+name+"/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[]}`))
			req.Header.Set("Authorization", "Bearer "+testTenantKey)
			r.ServeHTTP(httptest.NewRecorder(), req)

			up := ups[name]
			if !strings.Contains(up.body, `"stream_options":{"include_usage":true}`) {
				t.Errorf("upstream body = %s, want stream_options.include_usage true", up.body)
			}
			if up.contentLength != int64(len(up.body)) {
				t.Errorf("upstream Content-Length = %d, body is %d bytes", up.contentLength, len(up.body))
			}
		})
	}
}

// TestOversizedBodyIsRefused: the body limit applies before anything is
// forwarded.
func TestOversizedBodyIsRefused(t *testing.T) {
	r, ups := newTestRouter(t)
	big := `{"model":"gpt-4o","messages":[{"role":"user","content":"` + strings.Repeat("a", config.DefaultMaxRequestBytes) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer "+testTenantKey)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if ups["openai"].path != "" {
		t.Error("oversized request reached the provider")
	}
}

// meteredCall is one usage callback the gateway made.
type meteredCall struct {
	provider string
	result   usage.Result
}

// mockConfig returns a config with every format pointed at the real mock
// provider.
func mockConfig(t *testing.T) *config.Config {
	t.Helper()

	mock := httptest.NewServer(mockprovider.Handler())
	t.Cleanup(mock.Close)
	t.Setenv("TEST_MOCK_KEY", "mock")

	yaml := fmt.Sprintf(`
providers:
  openai:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: bearer
    format: openai
  groq:
    url: %[1]s/openai
    key: ${TEST_MOCK_KEY}
    auth: bearer
    format: openai
  anthropic:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: x-api-key
    format: anthropic
    headers:
      anthropic-version: "2023-06-01"
  gemini:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: x-goog-api-key
    format: gemini
`, mock.URL)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load() returned error: %v", err)
	}
	return cfg
}

// newMockRouter builds the gateway in front of the real mock provider and
// records each usage callback.
func newMockRouter(t *testing.T) (http.Handler, func() []meteredCall) {
	t.Helper()

	cfg := mockConfig(t)
	var mu sync.Mutex
	var calls []meteredCall
	onUsage := func(_ context.Context, provider string, r usage.Result) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, meteredCall{provider, r})
	}

	r, err := router(cfg, fakeStore{}, testAdminKey, onUsage, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("router() returned error: %v", err)
	}
	return r, func() []meteredCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]meteredCall(nil), calls...)
	}
}

// meterThroughMock sends one request through the gateway to the mock and
// returns the single usage callback it produced.
func meterThroughMock(t *testing.T, path, body string) meteredCall {
	t.Helper()

	r, calls := newMockRouter(t)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testTenantKey)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body)
	}

	got := calls()
	if len(got) != 1 {
		t.Fatalf("usage callback ran %d times, want once", len(got))
	}
	return got[0]
}

// TestOpenAIFormatUsageThroughMock: OpenAI and Groq, streamed or not, report
// the mock's exact numbers, in the gateway's convention (input excludes the
// cached tokens).
func TestOpenAIFormatUsageThroughMock(t *testing.T) {
	want := usage.Usage{
		Model:       "gpt-4o",
		Input:       mockprovider.PromptTokens - mockprovider.CachedPromptTokens,
		CachedInput: mockprovider.CachedPromptTokens,
		Output:      mockprovider.OutputTokens,
	}
	for _, name := range []string{"openai", "groq"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s stream=%v", name, stream), func(t *testing.T) {
				got := meterThroughMock(t, "/"+name+"/v1/chat/completions",
					fmt.Sprintf(`{"model":"gpt-4o","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream))

				if got.provider != name {
					t.Errorf("provider = %q, want %q", got.provider, name)
				}
				if got.result.Usage != want {
					t.Errorf("usage = %+v, want %+v", got.result.Usage, want)
				}
				if got.result.Streamed != stream || !got.result.Complete || got.result.Status != http.StatusOK {
					t.Errorf("result = %+v, want streamed %v, complete, status 200", got.result, stream)
				}
			})
		}
	}
}

func TestAnthropicUsageThroughMock(t *testing.T) {
	want := usage.Usage{
		Model:       "claude-sonnet-4",
		Input:       mockprovider.PromptTokens - mockprovider.CachedPromptTokens,
		CachedInput: mockprovider.CachedPromptTokens,
		Output:      mockprovider.OutputTokens,
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			got := meterThroughMock(t, "/anthropic/v1/messages",
				fmt.Sprintf(`{"model":"claude-sonnet-4","max_tokens":64,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream))

			if got.result.Usage != want {
				t.Errorf("usage = %+v, want %+v", got.result.Usage, want)
			}
			if got.result.Streamed != stream || !got.result.Complete {
				t.Errorf("result = %+v, want streamed %v, complete", got.result, stream)
			}
		})
	}
}

// TestGeminiUsageThroughMock covers all three Gemini response shapes: plain
// JSON, SSE, and the default JSON-array stream. Output includes thinking.
func TestGeminiUsageThroughMock(t *testing.T) {
	want := usage.Usage{
		Model:       "gemini-3.8-flash",
		Input:       mockprovider.PromptTokens - mockprovider.CachedPromptTokens,
		CachedInput: mockprovider.CachedPromptTokens,
		Output:      mockprovider.OutputTokens + mockprovider.ThoughtsTokens,
	}
	cases := []struct {
		name, path string
		streamed   bool
	}{
		{"generateContent", "/gemini/v1beta/models/gemini-3.8-flash:generateContent", false},
		{"sse stream", "/gemini/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse", true},
		{"json array stream", "/gemini/v1beta/models/gemini-3.8-flash:streamGenerateContent", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := meterThroughMock(t, tc.path, `{"contents":[{"parts":[{"text":"hi"}]}]}`)

			if got.result.Usage != want {
				t.Errorf("usage = %+v, want %+v", got.result.Usage, want)
			}
			if got.result.Streamed != tc.streamed || !got.result.Complete {
				t.Errorf("result = %+v, want streamed %v, complete", got.result, tc.streamed)
			}
		})
	}
}

// TestUsageRowsThroughGateway is the Week 2 checkpoint in one test: requests
// through the gateway, against Postgres, each leave exactly one usage_logs
// row carrying the X-Request-ID the client got, the tenant and key, the
// tokens and the cost.
func TestUsageRowsThroughGateway(t *testing.T) {
	conn := dbtest.New(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatalf("Migrate() returned error: %v", err)
	}
	store := tenant.NewStore(conn)
	tn, key, err := store.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := usage.NewWriter(conn, logger)
	r, err := router(mockConfig(t), store, testAdminKey, recordUsage(writer, logger), logger)
	if err != nil {
		t.Fatal(err)
	}

	send := func(path, body string) string {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key.Plaintext)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d; body %s", path, rec.Code, rec.Body)
		}
		return rec.Header().Get("X-Request-ID")
	}
	geminiID := send("/gemini/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse", `{}`)
	// Groq's Llama has no public price.
	groqID := send("/groq/v1/chat/completions", `{"model":"llama-3.3-70b-versatile","messages":[]}`)

	if err := writer.Close(ctx); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM usage_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("usage_logs has %d rows, want 2", n)
	}

	var (
		tenantID, provider, model, endpoint string
		keyID                               int64
		input, cached, output, status       int
		cost                                sql.NullInt64
		streamed                            bool
	)
	err = conn.QueryRow(`
		SELECT tenant_id, api_key_id, provider, model, endpoint, status,
		       input_tokens, cached_input_tokens, output_tokens, cost_micros, streamed
		FROM usage_logs WHERE request_id = $1`, geminiID,
	).Scan(&tenantID, &keyID, &provider, &model, &endpoint, &status, &input, &cached, &output, &cost, &streamed)
	if err != nil {
		t.Fatalf("no row with the request ID the client got (%s): %v", geminiID, err)
	}
	if tenantID != tn.ID || keyID != key.ID || provider != "gemini" || model != "gemini-3.8-flash" ||
		endpoint != "/v1beta/models/gemini-3.8-flash:streamGenerateContent" || status != 200 || !streamed {
		t.Errorf("row = %s %d %s %s %s %d streamed=%v", tenantID, keyID, provider, model, endpoint, status, streamed)
	}
	// 15 + 5 cached + 17 out at $0.75 / $0.075 / $3.75: 75.375 → 75.
	if input != 15 || cached != 5 || output != 17 || !cost.Valid || cost.Int64 != 75 {
		t.Errorf("tokens %d/%d/%d, cost %v; want 15/5/17 and 75", input, cached, output, cost)
	}

	if err := conn.QueryRow(`SELECT cost_micros FROM usage_logs WHERE request_id = $1`, groqID).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost.Valid {
		t.Errorf("unpriced model: cost = %d, want NULL", cost.Int64)
	}
}
