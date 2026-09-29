// Command gateway starts the LLM API gateway.
//
// Go note: `package main` plus a func main() makes this an executable. Every
// other package in this repo is a library. The directory layout convention is
// cmd/<binary-name>/main.go, so one repo can ship several binaries later.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/health"
	"github.com/grishkadel/llm-gateway/internal/middleware"
	"github.com/grishkadel/llm-gateway/internal/proxy"
)

func main() {
	// slog is the standard library's structured logger (Go 1.21+). The JSON
	// handler emits one object per line, which is what log aggregators want.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	configPath := os.Getenv("GATEWAY_CONFIG")
	if configPath == "" {
		configPath = "config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if err := run(cfg, logger); err != nil {
		logger.Error("server stopped with error", "error", err)
		os.Exit(1)
	}
}

// router builds the gateway's route table: one reverse proxy per configured
// provider, plus /health and a catch-all that rejects unknown prefixes.
//
// It's separated from run() so tests can exercise routing without starting a
// real server or handling signals.
func router(cfg *config.Config, logger *slog.Logger) (http.Handler, error) {
	providers, err := cfg.BuildProviders()
	if err != nil {
		return nil, err
	}

	// http.ServeMux is the standard library router. The most specific pattern
	// wins, so "/health" beats "/openai/" beats the catch-all "/".
	mux := http.NewServeMux()
	mux.Handle("/health", health.Handler())

	for _, p := range providers {
		// A trailing slash makes this a subtree pattern: "/openai/" matches
		// "/openai/v1/chat/completions". StripPrefix then removes "/openai"
		// before the proxy's Rewrite joins what's left onto the provider's
		// base URL — so routing and path rewriting need no custom code.
		prefix := "/" + p.Name
		mux.Handle(prefix+"/", http.StripPrefix(prefix, proxy.New(p, logger)))
	}

	// Anything that names no provider is rejected. There is deliberately no
	// default upstream: a typo in a base URL must never silently send traffic
	// — and spend — to the wrong provider.
	mux.Handle("/", unknownProvider(cfg.ProviderNames()))

	return mux, nil
}

// unknownProvider returns the handler for paths that match no configured
// provider. It lists what is configured, because the usual cause is a
// misspelled base URL.
func unknownProvider(names []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"type":      "unknown_provider",
				"message":   "no provider matches this path; point your client's base URL at /<provider>",
				"providers": names,
			},
		})
	})
}

// run wires everything together and blocks until the process is asked to stop.
//
// Go note: keeping the real work in run() instead of main() means it returns an
// error normally. os.Exit skips deferred functions, so we want exactly one place
// (main) that calls it.
func run(cfg *config.Config, logger *slog.Logger) error {
	mux, err := router(cfg, logger)
	if err != nil {
		return err
	}

	handler := middleware.Logging(logger)(mux)

	srv := &http.Server{
		Addr:    cfg.Addr(),
		Handler: handler,
		// Guards against a slow client holding a connection open by dribbling
		// out request headers.
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout is intentionally left at zero (no limit): streaming
		// completions can take minutes, and a write deadline would truncate
		// them mid-stream. The upstream timeout in config.yaml is what protects
		// us from a provider that never responds.
		IdleTimeout: 120 * time.Second,
	}

	// Go note: a channel is a typed pipe used to pass values between
	// goroutines. Here it carries the server's exit error out of the goroutine
	// we start it in. Buffered with size 1 so the send never blocks, even if
	// nobody is reading yet.
	serverErr := make(chan error, 1)

	// `go f()` runs f concurrently. ListenAndServe blocks forever, so it goes on
	// its own goroutine while main waits for a shutdown signal.
	go func() {
		logger.Info("gateway listening",
			"addr", srv.Addr,
			"providers", cfg.ProviderNames(),
		)
		// The gateway now holds real provider API keys but has no tenant
		// authentication yet (that arrives with middleware/auth.go). Until
		// then, anything that can reach this address can spend those keys.
		logger.Warn("no tenant authentication configured: any client that can reach this address can spend the configured provider keys",
			"addr", srv.Addr,
		)
		serverErr <- srv.ListenAndServe()
	}()

	// signal.NotifyContext gives us a context that is cancelled on Ctrl-C or on
	// the TERM signal Docker/Kubernetes send.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Go note: `defer` schedules a call to run when the surrounding function
	// returns, no matter which return path is taken. It's Go's version of a
	// `finally` block, and it's written next to the thing it cleans up.
	defer stop()

	// select waits on several channel operations at once and proceeds with
	// whichever is ready first.
	select {
	case err := <-serverErr:
		// ErrServerClosed is the expected error after a graceful Shutdown, so
		// it isn't a real failure.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil

	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// Shutdown stops accepting new connections and waits for in-flight
		// requests to finish, up to the deadline above.
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}

		logger.Info("gateway stopped")
		return nil
	}
}
