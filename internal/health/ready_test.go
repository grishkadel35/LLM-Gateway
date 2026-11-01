package health

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	// Registers the "pgx" driver with database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/grishkadel/llm-gateway/internal/redistest"
)

// returning is a check that returns err: nil for a service that is up.
func returning(err error) func(context.Context) error {
	return func(context.Context) error { return err }
}

// blocking is a check on a hung service. It returns when its context ends, as
// a ping given a context does.
func blocking(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// slow is a check on a service that is up but takes d to answer. It gives up
// if its context ends first.
func slow(d time.Duration) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// serveReady serves one request to a ReadyHandler over checks. It returns the
// response and the JSON log lines the handler wrote.
func serveReady(t *testing.T, method string, checks map[string]func(context.Context) error) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()

	var logs bytes.Buffer
	rec := httptest.NewRecorder()
	ReadyHandler(checks, slog.New(slog.NewJSONHandler(&logs, nil))).ServeHTTP(rec, httptest.NewRequest(method, "/ready", nil))

	var lines []map[string]any
	dec := json.NewDecoder(&logs)
	for dec.More() {
		var line map[string]any
		if err := dec.Decode(&line); err != nil {
			t.Fatalf("log output is not JSON lines: %v", err)
		}
		lines = append(lines, line)
	}
	return rec, lines
}

// checkAnswer fails the test unless rec is JSON with this status code and
// exactly this body. The body is pinned literally, since monitoring tools
// match on it, and it has no room for an error's text: the endpoint is
// unauthenticated.
func checkAnswer(t *testing.T, rec *httptest.ResponseRecorder, code int, body string) {
	t.Helper()
	if rec.Code != code {
		t.Errorf("status = %d, want %d", rec.Code, code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	if got := strings.TrimSpace(rec.Body.String()); got != body {
		t.Errorf("body = %s, want %s", got, body)
	}
}

// checkTiming fails the test unless the handler answered once want was up,
// and within 100 ms of it. The tests give the second as time.Second rather
// than readyTimeout, so that changing the constant fails them.
func checkTiming(t *testing.T, elapsed, want time.Duration) {
	t.Helper()
	if elapsed < want || elapsed > want+100*time.Millisecond {
		t.Errorf("answered after %v, want about %v", elapsed, want)
	}
}

// TestReadyAllUp is the main contract: with every check passing, GET /ready
// answers 200 with a JSON body, and logs nothing.
func TestReadyAllUp(t *testing.T) {
	rec, lines := serveReady(t, http.MethodGet, map[string]func(context.Context) error{
		"postgres": returning(nil),
		"redis":    returning(nil),
	})

	checkAnswer(t, rec, http.StatusOK, `{"status":"ready","checks":{"postgres":"ok","redis":"ok"}}`)
	if len(lines) != 0 {
		t.Errorf("logged %v, want nothing", lines)
	}
}

// TestReadyOneDown: a failing check makes the answer 503, names it down and
// the other ok, and is logged at Warn with its name and error. The error
// stays in the log.
func TestReadyOneDown(t *testing.T) {
	cases := []struct {
		down string // the check that fails
		err  error  // what it returns
		want string // the body
	}{
		{"postgres", errors.New("dial tcp 10.0.0.5:5432: connect: connection refused"),
			`{"status":"not_ready","checks":{"postgres":"down","redis":"ok"}}`},
		{"redis", errors.New("dial tcp 10.0.0.6:6379: i/o timeout"),
			`{"status":"not_ready","checks":{"postgres":"ok","redis":"down"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.down, func(t *testing.T) {
			checks := map[string]func(context.Context) error{"postgres": returning(nil), "redis": returning(nil)}
			checks[tc.down] = returning(tc.err)

			rec, lines := serveReady(t, http.MethodGet, checks)

			checkAnswer(t, rec, http.StatusServiceUnavailable, tc.want)
			if len(lines) != 1 {
				t.Fatalf("logged %v, want one line", lines)
			}
			if l := lines[0]; l["level"] != "WARN" || l["check"] != tc.down || l["error"] != tc.err.Error() {
				t.Errorf("log line = %v, want WARN naming %s and %q", l, tc.down, tc.err)
			}
		})
	}
}

// TestReadyBlockedCheckIsDown: a check still waiting after a second is down,
// the other is reported ok, and the answer comes when the second is up.
func TestReadyBlockedCheckIsDown(t *testing.T) {
	start := time.Now()
	rec, lines := serveReady(t, http.MethodGet, map[string]func(context.Context) error{
		"postgres": returning(nil),
		"redis":    blocking,
	})
	elapsed := time.Since(start)

	checkAnswer(t, rec, http.StatusServiceUnavailable, `{"status":"not_ready","checks":{"postgres":"ok","redis":"down"}}`)
	checkTiming(t, elapsed, time.Second)
	if len(lines) != 1 || lines[0]["level"] != "WARN" || lines[0]["check"] != "redis" || lines[0]["error"] != context.DeadlineExceeded.Error() {
		t.Errorf("log lines = %v, want one WARN naming redis and %q", lines, context.DeadlineExceeded)
	}
}

// TestReadyTwoBlockedChecks: two blocked checks cost one second between
// them, not two.
func TestReadyTwoBlockedChecks(t *testing.T) {
	start := time.Now()
	rec, _ := serveReady(t, http.MethodGet, map[string]func(context.Context) error{
		"postgres": blocking,
		"redis":    blocking,
	})
	elapsed := time.Since(start)

	checkAnswer(t, rec, http.StatusServiceUnavailable, `{"status":"not_ready","checks":{"postgres":"down","redis":"down"}}`)
	checkTiming(t, elapsed, time.Second)
}

// TestReadyChecksRunTogether: the checks run at the same time, each with the
// whole of its 1 s. Two healthy checks that take 600 ms each are both ok, and
// the answer comes after 600 ms. Run one after the other, the later would be
// left with 400 ms and go down.
func TestReadyChecksRunTogether(t *testing.T) {
	start := time.Now()
	rec, lines := serveReady(t, http.MethodGet, map[string]func(context.Context) error{
		"postgres": slow(600 * time.Millisecond),
		"redis":    slow(600 * time.Millisecond),
	})
	elapsed := time.Since(start)

	checkAnswer(t, rec, http.StatusOK, `{"status":"ready","checks":{"postgres":"ok","redis":"ok"}}`)
	checkTiming(t, elapsed, 600*time.Millisecond)
	if len(lines) != 0 {
		t.Errorf("logged %v, want nothing", lines)
	}
}

// TestReadyRejectsNonGET: the endpoint is GET-only and says so in Allow, as
// /health does, and a request that isn't GET runs no check.
func TestReadyRejectsNonGET(t *testing.T) {
	checks := map[string]func(context.Context) error{
		"postgres": func(context.Context) error {
			t.Error("a check ran for a request that isn't GET")
			return nil
		},
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec, _ := serveReady(t, method, checks)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow = %q, want %q", got, http.MethodGet)
			}
		})
	}
}

// TestReadyWithRealServices: a live Postgres and Redis are ready, and a
// go-redis client pointed at a port nothing listens on is down, with the
// client's own error in the log and not in the body. It skips without
// DATABASE_URL and REDIS_URL.
func TestReadyWithRealServices(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rdb := redistest.New(t)

	// pingRedis is the check the gateway will pass for c.
	pingRedis := func(c *redis.Client) func(context.Context) error {
		return func(ctx context.Context) error { return c.Ping(ctx).Err() }
	}

	t.Run("both up", func(t *testing.T) {
		rec, lines := serveReady(t, http.MethodGet, map[string]func(context.Context) error{
			"postgres": db.PingContext,
			"redis":    pingRedis(rdb),
		})

		checkAnswer(t, rec, http.StatusOK, `{"status":"ready","checks":{"postgres":"ok","redis":"ok"}}`)
		if len(lines) != 0 {
			t.Errorf("logged %v, want nothing", lines)
		}
	})

	t.Run("redis unreachable", func(t *testing.T) {
		// Port 0 makes the system pick a free port. Once the listener is
		// closed, nothing is behind that address.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		// Set up as the gateway's own client is: it honours a context's
		// deadline, and doesn't retry a command.
		dead := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true, MaxRetries: -1})
		t.Cleanup(func() { dead.Close() })

		rec, lines := serveReady(t, http.MethodGet, map[string]func(context.Context) error{
			"postgres": db.PingContext,
			"redis":    pingRedis(dead),
		})

		checkAnswer(t, rec, http.StatusServiceUnavailable, `{"status":"not_ready","checks":{"postgres":"ok","redis":"down"}}`)
		if len(lines) != 1 {
			t.Fatalf("logged %v, want one line", lines)
		}
		logged, _ := lines[0]["error"].(string)
		if l := lines[0]; l["level"] != "WARN" || l["check"] != "redis" || !strings.Contains(logged, addr) {
			t.Errorf("log line = %v, want WARN naming redis and an error that mentions %s", l, addr)
		}
	})
}
