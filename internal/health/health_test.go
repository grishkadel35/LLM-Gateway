package health

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerReturnsOK is the main contract: GET /health answers 200 with
// {"status":"ok"} and a JSON content type.
func TestHandlerReturnsOK(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

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
			Handler().ServeHTTP(rec, httptest.NewRequest(method, "/health", nil))

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
	mux.Handle("/health", Handler())
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
