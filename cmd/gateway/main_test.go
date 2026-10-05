package main

import (
	"encoding/json"
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
)

// fakeUpstream records what a provider actually received.
type fakeUpstream struct {
	server *httptest.Server
	path   string
	header http.Header
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()

	f := &fakeUpstream{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.path = r.URL.Path
		f.header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	// Go note: t.Cleanup registers a function to run when the test ends —
	// like defer, but it works from inside a helper.
	t.Cleanup(f.server.Close)
	return f
}

// newTestRouter wires four fake upstreams into a real config and builds the
// gateway's route table from it, exactly as main() would.
func newTestRouter(t *testing.T) (http.Handler, map[string]*fakeUpstream) {
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
  anthropic:
    url: %s
    key: ${TEST_ANTHROPIC_KEY}
    auth: x-api-key
    headers:
      anthropic-version: "2023-06-01"
  gemini:
    url: %s
    key: ${TEST_GEMINI_KEY}
    auth: x-goog-api-key
  groq:
    url: %s/openai
    key: ${TEST_GROQ_KEY}
    auth: bearer
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

	r, err := router(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("router() returned error: %v", err)
	}
	return r, ups
}

// TestRoutingReachesTheRightProvider is the core of the multi-provider change:
// each prefix must land on its own upstream, with the prefix stripped and that
// provider's own credential applied.
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
			req.Header.Set("Authorization", "Bearer sk-CLIENT-LEAK")

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

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer sk-CLIENT-LEAK")
	req.Header.Set("X-Api-Key", "sk-CLIENT-LEAK")
	req.Header.Set("X-Goog-Api-Key", "sk-CLIENT-LEAK")

	r.ServeHTTP(httptest.NewRecorder(), req)

	for _, h := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
		if got := ups["anthropic"].header.Get(h); strings.Contains(got, "CLIENT-LEAK") {
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
		{"health", http.MethodGet, "/health", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Result().Header.Values("X-Request-ID"); len(got) != 1 || !strings.HasPrefix(got[0], "req_") {
				t.Errorf("X-Request-ID = %q, want one req_ ID", got)
			}
		})
	}
}
