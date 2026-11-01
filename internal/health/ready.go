package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"
)

// readyTimeout is how long each of GET /ready's checks has to answer. They run
// at the same time, so the answer takes about this long at worst.
const readyTimeout = time.Second

// readyResponse is what GET /ready returns, as Response is for /health.
type readyResponse struct {
	// Status is "ready", or "not_ready" when any check is down.
	Status string `json:"status"`
	// Checks is each check's result, "ok" or "down", by name.
	Checks map[string]string `json:"checks"`
}

// ReadyHandler returns the handler for GET /ready. checks are the services
// the gateway can't serve without, by name, such as "postgres" and "redis".
// A check returns nil when its service answers.
//
// Where /health says the process is alive, this says it can serve, which is
// what a load balancer wants to know before it sends traffic. It answers 200
// when every check passes, and 503 when any fails or has not answered within a
// second. It never calls a provider: a provider outage can't make it fail.
//
// The handler waits for every check, so each must return when its context
// ends. A database/sql ping does; a go-redis command does only with
// ContextTimeoutEnabled, which cmd/gateway sets. Errors stay in the logs, each
// at Warn, as for /health: the endpoint is unauthenticated, so the body says
// only "ok" or "down".
func ReadyHandler(checks map[string]func(context.Context) error, logger *slog.Logger) http.Handler {
	// Go note: maps.Keys is an iterator over a map's keys, in no set order,
	// and slices.Sorted collects an iterator into a sorted slice. The checks
	// are sorted so that each has an index of its own, and so that the log
	// lines come in a fixed order.
	names := slices.Sorted(maps.Keys(checks))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()

		// Each goroutine writes only its own element of errs, so they need no
		// lock.
		errs := make([]error, len(names))
		var wg sync.WaitGroup
		for i, name := range names {
			wg.Go(func() { errs[i] = checks[name](ctx) })
		}
		wg.Wait()

		resp := readyResponse{Status: "ready", Checks: make(map[string]string, len(names))}
		code := http.StatusOK
		for i, name := range names {
			if errs[i] == nil {
				resp.Checks[name] = "ok"
				continue
			}
			resp.Status, code = "not_ready", http.StatusServiceUnavailable
			resp.Checks[name] = "down"
			logger.Warn("readiness check failed", "check", name, "error", errs[i])
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	})
}
