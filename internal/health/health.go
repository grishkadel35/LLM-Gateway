// Package health exposes the gateway's liveness endpoint.
package health

import (
	"encoding/json"
	"net/http"
)

// Response is what GET /health returns. Giving it a named type (instead of a
// map) means the JSON shape is checked by the compiler and shows up in tests.
type Response struct {
	Status string `json:"status"`
}

// Handler returns the handler for GET /health.
//
// It deliberately does not touch the upstream: this endpoint answers "is the
// gateway process alive and serving?", which is what a load balancer or
// container orchestrator wants to know. Upstream reachability is a separate
// concern and would make health checks fail for the wrong reason.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			// Go note: constants like http.MethodGet and http.StatusOK exist so
			// typos become compile errors instead of runtime surprises.
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Response{Status: "ok"})
	})
}
