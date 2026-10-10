package health

import (
	"context"
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

// TestHandlerReturnsOK is the main contract: GET /health answers 200 with
// {"status":"ok"} and a JSON content type.
func TestHandlerReturnsOK(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	// Decode rather than string-compare, so incidental whitespace doesn't make
	// the test brittle.
	var got Response
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q is not valid JSON: %v", body, err)
	}
	if got.Status != "ok" {
		t.Errorf("status field = %q, want %q", got.Status, "ok")
	}

	// Also pin the exact shape, since monitoring tools match on it literally.
	if want := `{"status":"ok"}`; strings.TrimSpace(string(body)) != want {
		t.Errorf("body = %q, want %q", strings.TrimSpace(string(body)), want)
	}
}

// TestHandlerRejectsNonGET checks that the endpoint is GET-only and advertises
// that fact in the Allow header, as RFC 9110 requires for a 405.
func TestHandlerRejectsNonGET(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		// Go note: t.Run gives each case its own name in the output. The loop
		// variable is captured safely per-iteration since Go 1.22.
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Handler(nil).ServeHTTP(rec, httptest.NewRequest(method, "/health", nil))

			res := rec.Result()
			defer res.Body.Close()

			if res.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", res.StatusCode, http.StatusMethodNotAllowed)
			}
			if got := res.Header.Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow = %q, want %q", got, http.MethodGet)
			}
		})
	}
}

// TestHealthDoesNotReachProxy documents the routing guarantee from main.go: with
// "/health" and "/" both registered, ServeMux picks the more specific pattern,
// so health checks never touch the upstream.
func TestHealthDoesNotReachProxy(t *testing.T) {
	proxyCalled := false

	mux := http.NewServeMux()
	mux.Handle("/health", Handler(nil))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalled = true
		w.WriteHeader(http.StatusBadGateway)
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if proxyCalled {
		t.Error("/health was routed to the catch-all proxy handler, want the health handler")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// testProvider is a provider at rawURL whose health path is /api/tags.
func testProvider(t *testing.T, rawURL string) provider.Provider {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return provider.Provider{Name: "ollama", URL: u, Auth: provider.AuthNone, HealthPath: "/api/tags"}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// getHealth returns GET /health's status and body.
func getHealth(t *testing.T, c *Checker) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler(c).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

// TestHandlerReportsCheckedProviders: /health lists the checked providers
// after "status", from unknown to the latest result, and still answers 200
// with the provider down. A provider without a health path isn't listed, and
// with none checked the body stays exactly {"status":"ok"}.
func TestHandlerReportsCheckedProviders(t *testing.T) {
	// Port 1 on loopback: nothing listens there, so the check fails at once.
	ollama := testProvider(t, "http://127.0.0.1:1")
	groq := provider.Provider{Name: "groq", URL: ollama.URL}

	if _, body := getHealth(t, NewChecker([]provider.Provider{groq}, nil, discardLogger())); body != `{"status":"ok"}` {
		t.Errorf("no provider checked: body = %s, want {\"status\":\"ok\"}", body)
	}

	c := NewChecker([]provider.Provider{ollama, groq}, nil, discardLogger())
	if _, body := getHealth(t, c); body != `{"status":"ok","providers":{"ollama":{"status":"unknown"}}}` {
		t.Errorf("before the first check: body = %s", body)
	}

	c.check(context.Background(), ollama)
	code, body := getHealth(t, c)
	if code != http.StatusOK {
		t.Errorf("status = %d, want %d: a provider outage must not fail liveness", code, http.StatusOK)
	}
	if !strings.HasPrefix(body, `{"status":"ok","providers":{"ollama":{"status":"down","checked_at":"`) {
		t.Errorf("after a failed check: body = %s", body)
	}
	// The endpoint is unauthenticated: the reason stays in the logs.
	if strings.Contains(body, "refused") {
		t.Errorf("body %s carries the check's error", body)
	}
}

// TestChecker: the first check runs as soon as Run starts. A 2xx from the
// health path is up, anything else down; each result reaches the callback;
// a keyed provider's check carries its key; Run returns when its context
// ends.
func TestChecker(t *testing.T) {
	refused := httptest.NewServer(http.NotFoundHandler())
	refusedURL := refused.URL
	refused.Close()

	cases := []struct {
		name   string
		status int // what the health path answers; 0 if nothing listens
		key    string
		wantUp bool
	}{
		{"200", http.StatusOK, "", true},
		{"500", http.StatusInternalServerError, "", false},
		{"connection refused", 0, "", false},
		{"keyed provider", http.StatusOK, "sk-provider", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testProvider(t, refusedURL)
			if tc.status != 0 {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path != "/api/tags":
						w.WriteHeader(http.StatusNotFound)
					case tc.key != "" && r.Header.Get("Authorization") != "Bearer "+tc.key:
						w.WriteHeader(http.StatusUnauthorized)
					default:
						w.WriteHeader(tc.status)
					}
				}))
				t.Cleanup(srv.Close)
				p = testProvider(t, srv.URL)
			}
			if tc.key != "" {
				p.Auth, p.Key = provider.AuthBearer, tc.key
			}

			reports := make(chan bool, 1)
			c := NewChecker([]provider.Provider{p}, func(name string, up bool) { reports <- up }, discardLogger())

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stopped := make(chan struct{})
			go func() {
				c.Run(ctx)
				close(stopped)
			}()

			select {
			case up := <-reports:
				if up != tc.wantUp {
					t.Errorf("callback got up = %v, want %v", up, tc.wantUp)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the first check never ran")
			}
			want := "down"
			if tc.wantUp {
				want = "up"
			}
			if got := c.Statuses()["ollama"]; got.Status != want || got.CheckedAt.IsZero() {
				t.Errorf("status = %+v, want %s with a time", got, want)
			}

			cancel()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("Run kept going after its context ended")
			}
		})
	}
}
