// Package health exposes the gateway's liveness endpoint, and checks in the
// background the providers that ask for it.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// Response is what GET /health returns. Giving it a named type (instead of a
// map) means the JSON shape is checked by the compiler and shows up in tests.
type Response struct {
	Status string `json:"status"`
	// Providers is each checked provider's latest result. It is left out
	// when no provider is checked.
	Providers map[string]ProviderStatus `json:"providers,omitempty"`
}

// ProviderStatus is a provider's latest check.
type ProviderStatus struct {
	// Status is "up", "down", or "unknown" until the first check ends.
	Status string `json:"status"`
	// CheckedAt is when the latest check ended; left out while unknown.
	CheckedAt time.Time `json:"checked_at,omitzero"`
}

// Handler returns the handler for GET /health. c is the background Checker,
// or nil when no provider is checked.
//
// It deliberately does not touch the upstream: this endpoint answers "is the
// gateway process alive and serving?", which is what a load balancer or
// container orchestrator wants to know. It always answers 200, so a provider
// outage can't take the gateway out of rotation, and what it says about the
// providers is the Checker's last result, never a call of its own. Errors stay
// in the logs: the endpoint is unauthenticated.
func Handler(c *Checker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			// Go note: constants like http.MethodGet and http.StatusOK exist so
			// typos become compile errors instead of runtime surprises.
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		resp := Response{Status: "ok"}
		if c != nil {
			resp.Providers = c.Statuses()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// A provider is checked every checkInterval, and each check gets
// checkTimeout to answer.
const (
	checkInterval = 15 * time.Second
	checkTimeout  = 5 * time.Second
)

// Checker checks every provider that has a health path, in the background,
// and keeps the latest result of each.
type Checker struct {
	providers []provider.Provider
	report    func(provider string, up bool)
	logger    *slog.Logger

	// mu guards results: Run writes them on its own goroutine while /health
	// reads them on request goroutines.
	mu      sync.Mutex
	results map[string]ProviderStatus
}

// NewChecker returns a Checker for those providers that have a HealthPath.
// report, if not nil, receives the result of every check.
func NewChecker(providers []provider.Provider, report func(provider string, up bool), logger *slog.Logger) *Checker {
	c := &Checker{report: report, logger: logger, results: map[string]ProviderStatus{}}
	for _, p := range providers {
		if p.HealthPath != "" {
			c.providers = append(c.providers, p)
			c.results[p.Name] = ProviderStatus{Status: "unknown"}
		}
	}
	return c
}

// Run checks every provider at once, and then every 15 seconds until ctx
// ends. It blocks, so it is started on its own goroutine.
func (c *Checker) Run(ctx context.Context) {
	if len(c.providers) == 0 {
		return
	}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		// One after another: a few providers at checkTimeout each fit well
		// within checkInterval.
		for _, p := range c.providers {
			c.check(ctx, p)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Statuses returns the latest result for each checked provider.
func (c *Checker) Statuses() map[string]ProviderStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.results)
}

// check checks p once, records the result, and logs a change of state.
func (c *Checker) check(ctx context.Context, p provider.Provider) {
	err := probe(ctx, p)
	if ctx.Err() != nil {
		// The gateway is shutting down: the check was cut short, it didn't
		// fail.
		return
	}

	up, status := err == nil, "down"
	if up {
		status = "up"
	}

	c.mu.Lock()
	previous := c.results[p.Name].Status
	c.results[p.Name] = ProviderStatus{Status: status, CheckedAt: time.Now().UTC()}
	c.mu.Unlock()

	if c.report != nil {
		c.report(p.Name, up)
	}
	switch {
	case status == previous:
	case up:
		c.logger.Info("provider is up", "provider", p.Name)
	default:
		c.logger.Warn("provider is down", "provider", p.Name, "error", err)
	}
}

// probe GETs p's health path, with p's own credentials so a provider that
// needs a key can be checked too. It returns why p isn't up, or nil when it
// answered 2xx.
func probe(ctx context.Context, p provider.Provider) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL.JoinPath(p.HealthPath).String(), nil)
	if err != nil {
		return err
	}
	p.Apply(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
