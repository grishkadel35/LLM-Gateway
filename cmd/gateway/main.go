// Command gateway starts the LLM API gateway.
//
// Go note: `package main` plus a func main() makes this an executable. Every
// other package in this repo is a library. The directory layout convention is
// cmd/<binary-name>/main.go, so one repo can ship several binaries later.
package main

import (
	"context"
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

// run wires everything together and blocks until the process is asked to stop.
//
// Go note: keeping the real work in run() instead of main() means it returns an
// error normally. os.Exit skips deferred functions, so we want exactly one place
// (main) that calls it.
func run(cfg *config.Config, logger *slog.Logger) error {
	rp, err := proxy.New(cfg.Upstream.URL, cfg.Upstream.Timeout(), logger)
	if err != nil {
		return err
	}

	// http.ServeMux is the standard library router. Longer patterns win, so
	// "/health" is matched before the catch-all "/" — health checks never reach
	// the proxy.
	mux := http.NewServeMux()
	mux.Handle("/health", health.Handler())
	mux.Handle("/", rp)

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
			"upstream", cfg.Upstream.URL,
			"timeout_seconds", cfg.Upstream.TimeoutSeconds,
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
