package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/health"
	"github.com/grishkadel/llm-gateway/internal/metrics"
)

// The ports run() opens: /metrics on a port of its own behind the admin key,
// /ready on the main one, and go-redis's log lines in the gateway's log.

// errorType returns the error.type of an apierror response, or "" for a body
// that isn't one.
func errorType(rec *httptest.ResponseRecorder) string {
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Type
}

// TestMetricsIsNotOnTheMainPort: the main router answers GET /metrics with the
// catch-all's 404, whether or not the request carries the admin key.
func TestMetricsIsNotOnTheMainPort(t *testing.T) {
	r := routerFor(t, fmt.Sprintf(ollamaConfig, "http://127.0.0.1:1"), nil, nil)

	for _, authorization := range []string{"", "Bearer " + testAdminKey} {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		rec := serve(r, req)
		if rec.Code != http.StatusNotFound || errorType(rec) != "unknown_provider" {
			t.Errorf("with Authorization %q: status %d, error %q; want 404 unknown_provider",
				authorization, rec.Code, errorType(rec))
		}
	}
}

// TestMetricsPortNeedsTheAdminKey: the handler run() serves on the metrics
// port refuses a request with no key, or with a tenant's, and gives the admin
// key the metrics.
func TestMetricsPortNeedsTheAdminKey(t *testing.T) {
	m := metrics.New()
	m.ProviderUp("ollama", true)
	h := metricsHandler(m, testAdminKey)

	cases := []struct {
		name, authorization string
		status              int
	}{
		{"no key", "", http.StatusUnauthorized},
		{"a tenant key", "Bearer " + testTenantKey, http.StatusUnauthorized},
		{"the admin key", "Bearer " + testAdminKey, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tc.authorization != "" {
				req.Header.Set("Authorization", tc.authorization)
			}
			rec := serve(h, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.status, rec.Body)
			}
			hasMetrics := strings.Contains(rec.Body.String(), `gateway_provider_up{provider="ollama"} 1`+"\n")
			switch {
			case tc.status == http.StatusOK && !hasMetrics:
				t.Errorf("the response lacks the metrics:\n%s", rec.Body)
			case tc.status != http.StatusOK && (hasMetrics || errorType(rec) != "invalid_admin_key"):
				t.Errorf("error %q, metrics shown: %v; want invalid_admin_key and none; body %s", errorType(rec), hasMetrics, rec.Body)
			}
		})
	}
}

// TestMetricsPortServesOnlyMetrics: with the admin key, the metrics port gives
// the metrics at /metrics alone, and any other path a JSON 404 without them.
// The key is checked before the path, as under /admin/, so without it every
// path gets 401.
func TestMetricsPortServesOnlyMetrics(t *testing.T) {
	m := metrics.New()
	m.ProviderUp("ollama", true)
	h := metricsHandler(m, testAdminKey)
	admin := "Bearer " + testAdminKey

	cases := []struct {
		name, path, authorization string
		status                    int
		errorType                 string
	}{
		{"metrics", "/metrics", admin, http.StatusOK, ""},
		{"metrics with a query", "/metrics?debug=1", admin, http.StatusOK, ""},
		{"the root", "/", admin, http.StatusNotFound, "not_found"},
		{"a trailing slash", "/metrics/", admin, http.StatusNotFound, "not_found"},
		{"an admin path", "/admin/tenants", admin, http.StatusNotFound, "not_found"},
		{"another path without the key", "/", "", http.StatusUnauthorized, "invalid_admin_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.authorization != "" {
				req.Header.Set("Authorization", tc.authorization)
			}
			rec := serve(h, req)
			if rec.Code != tc.status {
				t.Fatalf("GET %s: status = %d, want %d; body %s", tc.path, rec.Code, tc.status, rec.Body)
			}
			hasMetrics := strings.Contains(rec.Body.String(), `gateway_provider_up{provider="ollama"} 1`+"\n")
			if hasMetrics != (tc.status == http.StatusOK) {
				t.Errorf("GET %s: metrics shown: %v; body %s", tc.path, hasMetrics, rec.Body)
			}
			if got := errorType(rec); got != tc.errorType {
				t.Errorf("GET %s: error %q, want %q", tc.path, got, tc.errorType)
			}
		})
	}
}

// TestReadyThroughTheRouter: GET /ready is the readiness handler given to
// router: 200 while every check passes, 503 naming the check that fails. With
// no handler, the catch-all answers.
func TestReadyThroughTheRouter(t *testing.T) {
	checks := func(redisErr error) http.Handler {
		return health.ReadyHandler(map[string]func(context.Context) error{
			"postgres": func(context.Context) error { return nil },
			"redis":    func(context.Context) error { return redisErr },
		}, discardLogger)
	}
	cfg := loadConfig(t, fmt.Sprintf(ollamaConfig, "http://127.0.0.1:1"))

	cases := []struct {
		name   string
		ready  http.Handler
		status int
		checks map[string]string
	}{
		{"checks that pass", checks(nil), http.StatusOK, map[string]string{"postgres": "ok", "redis": "ok"}},
		{"a failing redis", checks(errors.New("connection refused")), http.StatusServiceUnavailable, map[string]string{"postgres": "ok", "redis": "down"}},
		{"no handler", nil, http.StatusNotFound, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := router(cfg, fakeStore{}, testAdminKey, nil, nil, nil, nil, metrics.New(), nil, tc.ready, discardLogger)
			if err != nil {
				t.Fatalf("router() returned error: %v", err)
			}
			rec := serve(r, httptest.NewRequest(http.MethodGet, "/ready", nil))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.status, rec.Body)
			}
			if tc.checks == nil {
				if got := errorType(rec); got != "unknown_provider" {
					t.Errorf("error %q, want unknown_provider", got)
				}
				return
			}
			var body struct {
				Status string
				Checks map[string]string
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q is not JSON: %v", rec.Body, err)
			}
			if len(body.Checks) != len(tc.checks) || body.Checks["postgres"] != tc.checks["postgres"] || body.Checks["redis"] != tc.checks["redis"] {
				t.Errorf("checks = %v, want %v", body.Checks, tc.checks)
			}
		})
	}
}

// runLogged runs run() on cfg with the database DATABASE_URL names, and
// returns its error and its JSON log. Without DATABASE_URL the test is
// skipped. run() only pings the database, and the tests here make it fail
// before it serves anything.
func runLogged(t *testing.T, cfg *config.Config) (string, error) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres tests")
	}
	t.Setenv("GATEWAY_ADMIN_KEY", testAdminKey)
	// run() points go-redis's logger, which is process-wide, at the log it is
	// given. Leave it quiet for the tests after this one.
	t.Cleanup(func() { redis.SetLogger(redisLogger{slog.New(slog.DiscardHandler)}) })

	var logs lockedBuffer
	err := run(cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
	return logs.String(), err
}

// TestRunFailsWhenTheMetricsPortIsTaken: with another listener on the metrics
// port, run() returns an error that names its address, and has not said it is
// listening: it opens both ports before it serves on either, and lets go of
// the main port when the metrics port fails.
func TestRunFailsWhenTheMetricsPortIsTaken(t *testing.T) {
	if os.Getenv("REDIS_URL") == "" {
		t.Skip("REDIS_URL not set; skipping Redis tests")
	}

	// The metrics port is this listener's for as long as the test runs. The
	// main port is one that was free a moment ago.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free.Close()

	cfg := loadConfig(t, fmt.Sprintf(ollamaConfig, "http://127.0.0.1:1"))
	cfg.Port = free.Addr().(*net.TCPAddr).Port
	cfg.MetricsPort = taken.Addr().(*net.TCPAddr).Port

	logs, err := runLogged(t, cfg)
	if err == nil || !strings.Contains(err.Error(), cfg.MetricsAddr()) {
		t.Fatalf("run() returned %v, want an error naming %s", err, cfg.MetricsAddr())
	}
	if strings.Contains(logs, "gateway listening") {
		t.Errorf("run() said it was listening:\n%s", logs)
	}
	// run() opened the main port first, and must not have kept it.
	again, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		t.Errorf("the main port is still held after run() failed: %v", err)
	} else {
		again.Close()
	}
}

// TestRunLogsGoRedisAsJSON: go-redis's own log lines go through run()'s logger
// from the first connection on. With nothing listening at REDIS_URL, run()
// fails at that connection, and the log holds go-redis's line about the dials
// that failed, as JSON, at WARN, from go-redis.
func TestRunLogsGoRedisAsJSON(t *testing.T) {
	// Port 1 on loopback: nothing listens there.
	t.Setenv("REDIS_URL", "redis://127.0.0.1:1/0")

	logs, err := runLogged(t, loadConfig(t, fmt.Sprintf(ollamaConfig, "http://127.0.0.1:1")))
	if err == nil || !strings.Contains(err.Error(), "connecting to Redis") {
		t.Fatalf("run() returned %v, want an error from the connection to Redis", err)
	}

	found := false
	for line := range strings.Lines(logs) {
		var entry struct{ Level, Msg, Component string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if entry.Component == "go-redis" && entry.Level == "WARN" && strings.Contains(entry.Msg, "failed to dial") {
			found = true
		}
	}
	if !found {
		t.Errorf("the log has no WARN line from go-redis about the failed dials:\n%s", logs)
	}
}
