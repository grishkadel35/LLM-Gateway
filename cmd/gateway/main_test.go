package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
	"github.com/grishkadel/llm-gateway/internal/metrics"
	"github.com/grishkadel/llm-gateway/internal/middleware"
	"github.com/grishkadel/llm-gateway/internal/mockprovider"
	"github.com/grishkadel/llm-gateway/internal/pricing"
	"github.com/grishkadel/llm-gateway/internal/provider"
	"github.com/grishkadel/llm-gateway/internal/ratelimit"
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
// knows one tenant, with the schema's default limits, and one key.
type fakeStore struct{}

func (fakeStore) Lookup(_ context.Context, key string) (tenant.Tenant, tenant.APIKey, error) {
	if key != testTenantKey {
		return tenant.Tenant{}, tenant.APIKey{}, tenant.ErrNotFound
	}
	return tenant.Tenant{ID: "tn_test", RateLimitTokensPerMin: 100_000, DefaultMaxTokens: 4096},
		tenant.APIKey{ID: 1, TenantID: "tn_test"}, nil
}

func (fakeStore) Create(_ context.Context, name string) (tenant.Tenant, tenant.NewKey, error) {
	return tenant.Tenant{ID: "tn_new", Name: name}, tenant.NewKey{APIKey: tenant.APIKey{ID: 2, TenantID: "tn_new"}}, nil
}

func (fakeStore) IssueKey(context.Context, string) (tenant.NewKey, error) {
	return tenant.NewKey{}, tenant.ErrNotFound
}

func (fakeStore) RevokeKey(context.Context, int64) error { return tenant.ErrNotFound }

func (fakeStore) TenantUsage(context.Context, string, time.Time, time.Time) (usage.Summary, error) {
	return usage.Summary{}, tenant.ErrNotFound
}

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

// newTestRouter wires five fake upstreams into a real config and builds the
// gateway's route table from it, exactly as main() would, over fakeStore and
// with no rate limiting.
func newTestRouter(t *testing.T) (http.Handler, map[string]*fakeUpstream) {
	t.Helper()
	return newTestRouterWith(t, fakeStore{}, nil)
}

func newTestRouterWith(t *testing.T, store tenantStore, limiter middleware.Limiter) (http.Handler, map[string]*fakeUpstream) {
	t.Helper()

	ups := map[string]*fakeUpstream{
		"openai":    newFakeUpstream(t),
		"anthropic": newFakeUpstream(t),
		"gemini":    newFakeUpstream(t),
		"groq":      newFakeUpstream(t),
		"ollama":    newFakeUpstream(t),
	}

	// Keys must be environment references; the values are what the upstreams
	// should receive.
	for name := range ups {
		t.Setenv("TEST_"+strings.ToUpper(name)+"_KEY", "sk-"+name)
	}

	// Groq's real API lives under a base path; keep that shape in the fake.
	// Ollama is keyless: auth none, and no key line.
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
  ollama:
    url: %s
    auth: none
    format: openai
`, ups["openai"].server.URL, ups["anthropic"].server.URL,
		ups["gemini"].server.URL, ups["groq"].server.URL, ups["ollama"].server.URL)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load() returned error: %v", err)
	}

	r, err := router(cfg, store, testAdminKey, nil, limiter, metrics.New(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
		// Ollama is keyless: the client's gateway key is stripped and nothing
		// replaces it.
		{"ollama", "/ollama/v1/chat/completions", "/v1/chat/completions", "Authorization", ""},
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
	// authenticates, and then none of the three copies may go upstream,
	// whether the provider has a key of its own (anthropic) or none (ollama).
	for _, p := range []struct{ name, path string }{
		{"anthropic", "/anthropic/v1/messages"},
		{"ollama", "/ollama/v1/chat/completions"},
	} {
		req := httptest.NewRequest(http.MethodPost, p.path, nil)
		req.Header.Set("Authorization", "Bearer "+testTenantKey)
		req.Header.Set("X-Api-Key", testTenantKey)
		req.Header.Set("X-Goog-Api-Key", testTenantKey)

		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want %d; body %s", p.name, rec.Code, http.StatusOK, rec.Body)
		}

		for _, h := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
			if got := ups[p.name].header.Get(h); strings.Contains(got, testTenantKey) {
				t.Errorf("%s: %s = %q leaked the client's credential upstream", p.name, h, got)
			}
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
	if len(body.Error.Providers) != len(ups) {
		t.Errorf("error.providers = %v, want all %d configured providers", body.Error.Providers, len(ups))
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
		// Keyless upstream, but the gateway key is still required.
		"/ollama/v1/chat/completions",
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
	r, ups := newTestRouterWith(t, stores{tenant.NewStore(conn), usage.NewReports(conn)}, nil)

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

// TestOpenRedisFailsFast: a missing, malformed or unreachable Redis stops
// startup with an error that says what to fix, and never quotes the URL,
// whose password would then land in the log.
func TestOpenRedisFailsFast(t *testing.T) {
	cases := []struct{ name, url, want string }{
		{"unset", "", "REDIS_URL is not set"},
		// url.Parse fails on this one, and its error quotes the whole URL.
		{"not a URL", "redis://:hunter2@127.0.0.1:port/0", "REDIS_URL is not a valid URL"},
		{"wrong scheme", "postgres://:hunter2@127.0.0.1:6379/0", "REDIS_URL is not a valid Redis URL"},
		{"unknown option", "redis://:hunter2@127.0.0.1:6379/0?retries=3", "REDIS_URL is not a valid Redis URL"},
		// Port 1 on loopback: nothing listens there, so the ping fails at once.
		{"unreachable", "redis://:hunter2@127.0.0.1:1/0", "connecting to Redis"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rdb, err := openRedis(tc.url)
			if err == nil {
				rdb.Close()
				t.Fatal("openRedis() returned no error")
			}
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "hunter2") {
				t.Errorf("openRedis() = %q, want an error containing %q, without the password", err, tc.want)
			}
		})
	}
}

// TestOpenRedis: the options redisOptions forces still connect to a real
// Redis.
func TestOpenRedis(t *testing.T) {
	u := os.Getenv("REDIS_URL")
	if u == "" {
		t.Skip("REDIS_URL not set; skipping Redis tests")
	}
	rdb, err := openRedis(u)
	if err != nil {
		t.Fatalf("openRedis() returned error: %v", err)
	}
	rdb.Close()
}

// TestRedisOptions: the client honours context deadlines and never retries a
// command, whatever the URL says. The rate limit scripts aren't idempotent,
// so a retry could charge twice.
func TestRedisOptions(t *testing.T) {
	for _, u := range []string{
		"redis://127.0.0.1:6379/0",
		// go-redis reads max_retries from the URL; it must not win.
		"redis://127.0.0.1:6379/0?max_retries=3",
	} {
		opts, err := redisOptions(u)
		if err != nil {
			t.Fatalf("redisOptions(%q) returned error: %v", u, err)
		}
		if opts.MaxRetries != -1 || !opts.ContextTimeoutEnabled {
			t.Errorf("redisOptions(%q): MaxRetries %d, ContextTimeoutEnabled %v; want -1, true",
				u, opts.MaxRetries, opts.ContextTimeoutEnabled)
		}
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
	for _, name := range []string{"openai", "groq", "ollama"} {
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

// fakeLimiter stands in for the Redis limiter: it answers every request the
// same way, and records what it was asked.
type fakeLimiter struct {
	allowed bool
	wait    time.Duration
	calls   []allowCall
}

// allowCall is one Allow a fakeLimiter received.
type allowCall struct {
	tenantID    string
	limit, cost int64
}

func (f *fakeLimiter) Allow(_ context.Context, tenantID string, limit, cost int64) (bool, time.Duration, error) {
	f.calls = append(f.calls, allowCall{tenantID, limit, cost})
	return f.allowed, f.wait, nil
}

// TestRateLimitThroughGateway: over the limit, the client gets the gateway's
// 429 and the provider is never called; under it, the provider receives the
// client's exact bytes. Either way the limiter is asked once, with the
// tenant's own limit and the request's estimate.
func TestRateLimitThroughGateway(t *testing.T) {
	// No output cap, so the estimate is the body's tokens (a token per 4
	// bytes) plus the tenant's default_max_tokens.
	const body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	want := allowCall{"tn_test", 100_000, int64(len(body)+3)/4 + 4096}

	send := func(t *testing.T, limiter *fakeLimiter) (*httptest.ResponseRecorder, map[string]*fakeUpstream) {
		t.Helper()
		r, ups := newTestRouterWith(t, fakeStore{}, limiter)
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testTenantKey)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if len(limiter.calls) != 1 || limiter.calls[0] != want {
			t.Errorf("Allow calls = %+v, want one: %+v", limiter.calls, want)
		}
		return rec, ups
	}

	t.Run("over the limit", func(t *testing.T) {
		rec, ups := send(t, &fakeLimiter{wait: 12 * time.Second})

		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "12" {
			t.Errorf("status %d, Retry-After %q; want 429, 12", rec.Code, rec.Header().Get("Retry-After"))
		}
		var got struct {
			Error struct {
				Type              string `json:"type"`
				RetryAfterSeconds int64  `json:"retry_after_seconds"`
				LimitTokensPerMin int64  `json:"limit_tokens_per_min"`
				EstimatedTokens   int64  `json:"estimated_tokens"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("body %s: %v", rec.Body, err)
		}
		if e := got.Error; e.Type != "rate_limited" || e.RetryAfterSeconds != 12 || e.LimitTokensPerMin != 100_000 || e.EstimatedTokens != want.cost {
			t.Errorf("error = %+v, want rate_limited, 12 s, limit 100000, estimate %d", e, want.cost)
		}
		for name, up := range ups {
			if up.path != "" {
				t.Errorf("a rate-limited request reached provider %q", name)
			}
		}
	})

	t.Run("under the limit", func(t *testing.T) {
		rec, ups := send(t, &fakeLimiter{allowed: true})

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body)
		}
		if ups["openai"].body != body {
			t.Errorf("upstream body = %s, want the client's exact bytes %s", ups["openai"].body, body)
		}
	})
}

// hungRedis returns the address of a server that accepts connections and
// never answers on them, like a Redis that has hung.
func hungRedis(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // the listener is closed
			}
			// Read whatever arrives, answer nothing, and hang up when the
			// client does.
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				conn.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// TestRateLimitFailsOpenWhenRedisIsDown: with Redis hung or refusing
// connections, a request is still admitted, with a Warn, in about Allow's
// 100 ms timeout. That takes a client from redisOptions: without
// ContextTimeoutEnabled, go-redis would wait out its own read timeout (5 s)
// on the hung one.
func TestRateLimitFailsOpenWhenRedisIsDown(t *testing.T) {
	cases := []struct{ name, addr string }{
		{"hung", hungRedis(t)},
		// Port 1 on loopback: nothing listens there, so connecting is refused.
		{"refusing connections", "127.0.0.1:1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := redisOptions("redis://" + tc.addr + "/0")
			if err != nil {
				t.Fatal(err)
			}
			rdb := redis.NewClient(opts)
			t.Cleanup(func() { rdb.Close() })

			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			admitted := false
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { admitted = true })
			// A provider route's order: Auth, ReadBody, then RateLimit.
			h := middleware.Auth(fakeStore{}, logger)(usage.ReadBody(provider.FormatOpenAI, 1<<20)(
				middleware.RateLimit(ratelimit.New(rdb), logger)(next)))

			start := time.Now()
			h.ServeHTTP(httptest.NewRecorder(), chatRequest("openai", "gpt-4o"))
			elapsed := time.Since(start)

			if !admitted {
				t.Error("the request was not admitted")
			}
			if elapsed > time.Second {
				t.Errorf("admitted after %v, want about 100 ms", elapsed)
			}
			var entry struct{ Level, Error string }
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry.Level != "WARN" || entry.Error == "" {
				t.Errorf("log = %q, want one WARN line with the error", logs.String())
			}
		})
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

	r, err := router(cfg, fakeStore{}, testAdminKey, onUsage, nil, metrics.New(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	store := stores{tenant.NewStore(conn), usage.NewReports(conn)}
	tn, key, err := store.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := usage.NewWriter(conn, logger)
	r, err := router(mockConfig(t), store, testAdminKey, recordUsage(writer, nil, logger), nil, metrics.New(), nil, logger)
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
	// The price in force today: Gemini 3.8 Flash's changes on 1 January
	// 2027, and pricing's own tests pin both values.
	wantCost, _ := pricing.Cost("gemini", usage.Usage{Model: "gemini-3.8-flash", Input: 15, CachedInput: 5, Output: 17}, "", time.Now())
	if input != 15 || cached != 5 || output != 17 || !cost.Valid || cost.Int64 != wantCost {
		t.Errorf("tokens %d/%d/%d, cost %v; want 15/5/17 and %d", input, cached, output, cost, wantCost)
	}

	if err := conn.QueryRow(`SELECT cost_micros FROM usage_logs WHERE request_id = $1`, groqID).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost.Valid {
		t.Errorf("unpriced model: cost = %d, want NULL", cost.Int64)
	}

	// The admin usage report reads the same rows back.
	req := httptest.NewRequest(http.MethodGet, "/admin/tenants/"+tn.ID+"/usage", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminKey)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage report: status = %d; body %s", rec.Code, rec.Body)
	}
	var report struct {
		Total usage.Totals `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Total.Requests != 2 || report.Total.CostMicros != wantCost || report.Total.UnpricedRequests != 1 {
		t.Errorf("report total = %+v, want 2 requests, %d micro-$, 1 unpriced", report.Total, wantCost)
	}
}

// TestEveryFormatLandsAPricedRow is the roadmap's Week 2 test matrix: every
// format, streamed or not, through the gateway to the mock, against
// Postgres, leaves one usage_logs row with the mock's tokens, the cost at
// today's price, and the X-Request-ID the client received.
func TestEveryFormatLandsAPricedRow(t *testing.T) {
	in, cached := int64(mockprovider.PromptTokens-mockprovider.CachedPromptTokens), int64(mockprovider.CachedPromptTokens)
	out := int64(mockprovider.OutputTokens)

	cases := []struct {
		name, provider, model, path, body string
		streamed                          bool
		output                            int64
	}{
		{"openai", "openai", "gpt-4o", "/openai/v1/chat/completions", `{"model":"gpt-4o","messages":[]}`, false, out},
		{"openai stream", "openai", "gpt-4o", "/openai/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`, true, out},
		{"groq", "groq", "openai/gpt-oss-120b", "/groq/v1/chat/completions", `{"model":"openai/gpt-oss-120b","messages":[]}`, false, out},
		{"groq stream", "groq", "openai/gpt-oss-120b", "/groq/v1/chat/completions", `{"model":"openai/gpt-oss-120b","stream":true,"messages":[]}`, true, out},
		{"anthropic", "anthropic", "claude-sonnet-4", "/anthropic/v1/messages", `{"model":"claude-sonnet-4","max_tokens":64,"messages":[]}`, false, out},
		{"anthropic stream", "anthropic", "claude-sonnet-4", "/anthropic/v1/messages", `{"model":"claude-sonnet-4","max_tokens":64,"stream":true,"messages":[]}`, true, out},
		// Gemini's output includes its thinking tokens.
		{"gemini", "gemini", "gemini-3.8-flash", "/gemini/v1beta/models/gemini-3.8-flash:generateContent", `{}`, false, out + mockprovider.ThoughtsTokens},
		{"gemini sse", "gemini", "gemini-3.8-flash", "/gemini/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse", `{}`, true, out + mockprovider.ThoughtsTokens},
		{"gemini json stream", "gemini", "gemini-3.8-flash", "/gemini/v1beta/models/gemini-3.8-flash:streamGenerateContent", `{}`, true, out + mockprovider.ThoughtsTokens},
	}

	conn := dbtest.New(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatalf("Migrate() returned error: %v", err)
	}
	store := stores{tenant.NewStore(conn), usage.NewReports(conn)}
	tn, key, err := store.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := usage.NewWriter(conn, logger)
	r, err := router(mockConfig(t), store, testAdminKey, recordUsage(writer, nil, logger), nil, metrics.New(), nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	ids := make([]string, len(cases))
	for i, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+key.Plaintext)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d; body %s", tc.name, rec.Code, rec.Body)
		}
		ids[i] = rec.Header().Get("X-Request-ID")
	}
	if err := writer.Close(ctx); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM usage_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(cases) {
		t.Errorf("usage_logs has %d rows, want one per request (%d)", n, len(cases))
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				tenantID, provider, model string
				keyID                     int64
				gotIn, gotCached, gotOut  int64
				cost                      sql.NullInt64
				streamed                  bool
			)
			err := conn.QueryRow(`
				SELECT tenant_id, api_key_id, provider, model, input_tokens, cached_input_tokens,
				       output_tokens, cost_micros, streamed
				FROM usage_logs WHERE request_id = $1`, ids[i],
			).Scan(&tenantID, &keyID, &provider, &model, &gotIn, &gotCached, &gotOut, &cost, &streamed)
			if err != nil {
				t.Fatalf("no row for the X-Request-ID the client got (%s): %v", ids[i], err)
			}

			if tenantID != tn.ID || keyID != key.ID || provider != tc.provider || model != tc.model || streamed != tc.streamed {
				t.Errorf("row = %s key %d %s/%s streamed=%v; want %s key %d %s/%s streamed=%v",
					tenantID, keyID, provider, model, streamed, tn.ID, key.ID, tc.provider, tc.model, tc.streamed)
			}
			if gotIn != in || gotCached != cached || gotOut != tc.output {
				t.Errorf("tokens = %d/%d/%d, want %d/%d/%d", gotIn, gotCached, gotOut, in, cached, tc.output)
			}

			wantCost, ok := pricing.Cost(tc.provider, usage.Usage{Model: tc.model, Input: in, CachedInput: cached, Output: tc.output}, "", time.Now())
			if !ok || wantCost <= 0 {
				t.Fatalf("%s/%s should be priced", tc.provider, tc.model)
			}
			if !cost.Valid || cost.Int64 != wantCost {
				t.Errorf("cost = %v, want %d", cost, wantCost)
			}
		})
	}
}

// TestRecordUsageCost: cost is recorded only when the usage is known. A
// response cut short, or too large to parse, has unknown usage, so its cost
// is NULL, not 0; a complete response with no tokens (an upstream error)
// really did cost nothing. A free provider's rows cost 0, complete or not,
// and every row keeps its tokens.
func TestRecordUsageCost(t *testing.T) {
	gpt4o := usage.Usage{Model: "gpt-4o", Input: 15, CachedInput: 5, Output: 10}
	cases := []struct {
		name     string
		provider string
		result   usage.Result
		wantNull bool
		want     int64
	}{
		{"complete, priced", "openai", usage.Result{Usage: gpt4o, Status: 200, Complete: true}, false, 144},
		{"complete, no tokens", "openai", usage.Result{Usage: usage.Usage{Model: "gpt-4o"}, Status: 429, Complete: true}, false, 0},
		{"complete, unpriced", "groq", usage.Result{Usage: usage.Usage{Model: "llama-3.3-70b-versatile", Input: 10}, Status: 200, Complete: true}, true, 0},
		{"cut off before usage arrived", "openai", usage.Result{Usage: usage.Usage{Model: "gpt-4o"}, Status: 200}, true, 0},
		{"cut off with partial usage", "anthropic", usage.Result{Usage: usage.Usage{Model: "claude-sonnet-4", Input: 15}, Status: 200}, true, 0},
		{"free, complete", "ollama", usage.Result{Usage: usage.Usage{Model: "qwen3.5:9b", Input: 15, Output: 10}, Status: 200, Complete: true}, false, 0},
		{"free, cut off", "ollama", usage.Result{Usage: usage.Usage{Model: "qwen3.5:9b", Input: 15}, Status: 200}, false, 0},
	}

	conn := dbtest.New(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	store := stores{tenant.NewStore(conn), usage.NewReports(conn)}
	_, key, err := store.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := usage.NewWriter(conn, logger)
	record := recordUsage(writer, map[string]config.ProviderConfig{"ollama": {Free: true}}, logger)

	// recordUsage reads the tenant and request ID from the context, so each
	// call runs inside the real middleware.
	ids := make([]string, len(cases))
	for i, tc := range cases {
		h := middleware.RequestID(middleware.Auth(store, logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			record(r.Context(), tc.provider, tc.result)
		})))
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Authorization", "Bearer "+key.Plaintext)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		ids[i] = rec.Header().Get("X-Request-ID")
	}
	if err := writer.Close(ctx); err != nil {
		t.Fatal(err)
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cost sql.NullInt64
			var input int64
			if err := conn.QueryRow(`SELECT cost_micros, input_tokens FROM usage_logs WHERE request_id = $1`, ids[i]).Scan(&cost, &input); err != nil {
				t.Fatal(err)
			}
			if input != tc.result.Usage.Input {
				t.Errorf("input_tokens = %d, want %d", input, tc.result.Usage.Input)
			}
			if tc.wantNull && cost.Valid {
				t.Errorf("cost = %d, want NULL", cost.Int64)
			}
			if !tc.wantNull && (!cost.Valid || cost.Int64 != tc.want) {
				t.Errorf("cost = %v, want %d", cost, tc.want)
			}
		})
	}
}
