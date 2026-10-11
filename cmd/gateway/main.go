// Command gateway starts the LLM API gateway.
//
// Go note: `package main` plus a func main() makes this an executable. Every
// other package in this repo is a library. The directory layout convention is
// cmd/<binary-name>/main.go, so one repo can ship several binaries later.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// Registers the "pgx" driver with database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/grishkadel/llm-gateway/internal/admin"
	"github.com/grishkadel/llm-gateway/internal/apierror"
	"github.com/grishkadel/llm-gateway/internal/concurrency"
	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/health"
	"github.com/grishkadel/llm-gateway/internal/metrics"
	"github.com/grishkadel/llm-gateway/internal/middleware"
	"github.com/grishkadel/llm-gateway/internal/pricing"
	"github.com/grishkadel/llm-gateway/internal/proxy"
	"github.com/grishkadel/llm-gateway/internal/ratelimit"
	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
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
		logger.Error("gateway exited with error", "error", err)
		os.Exit(1)
	}
}

// usageFunc receives each provider response's usage, with the name of the
// provider that served it.
type usageFunc func(ctx context.Context, provider string, r usage.Result)

// tenantStore is what the routes need from storage: key lookup for tenant
// auth, and tenant, key and usage reporting for the admin API. stores
// satisfies it; routing tests substitute a fake.
type tenantStore interface {
	middleware.KeyLookup
	admin.Store
}

// stores combines the two Postgres-backed stores into one tenantStore.
type stores struct {
	*tenant.Store
	*usage.Reports
}

// router builds the gateway's handler: one reverse proxy per configured
// provider behind tenant auth and rate limiting, the admin API behind the
// admin key, /health, /metrics, and a catch-all that rejects unknown
// prefixes, all behind the request ID and logging middleware. A nil limiter
// means no rate limiting.
//
// It's separated from run() so tests can exercise routing without starting a
// real server or handling signals.
func router(cfg *config.Config, store tenantStore, adminKey string, onUsage usageFunc, limiter middleware.Limiter, m *metrics.Metrics, checker *health.Checker, logger *slog.Logger) (http.Handler, error) {
	providers, err := cfg.BuildProviders()
	if err != nil {
		return nil, err
	}

	// http.ServeMux is the standard library router. The most specific pattern
	// wins, so "/health" beats "/openai/" beats the catch-all "/".
	mux := http.NewServeMux()
	mux.Handle("/health", health.Handler(checker))
	// Unauthenticated for now; roadmap Week 6 decides between admin auth and a
	// separate port.
	mux.Handle("/metrics", m.Handler())

	// One subtree behind AdminAuth, not a pattern per admin route: with
	// per-route patterns, ServeMux would answer an unauthenticated request for
	// the wrong method with 405 and an Allow header, before any auth ran.
	mux.Handle("/admin/", middleware.AdminAuth(adminKey)(admin.Handler(store, logger)))

	tenantAuth := middleware.Auth(store, logger)

	// Every provider's proxy first, so a route can hand a failed request to
	// its fallback's.
	proxies := make(map[string]http.Handler, len(providers))
	for _, p := range providers {
		name := p.Name
		meter := func(ctx context.Context, r usage.Result) {
			m.Tokens(name, r.Usage)
			if onUsage != nil {
				onUsage(ctx, name, r)
			}
		}
		rp := proxy.New(p, logger, meter)
		// The limiter wraps the transport, so a slot is held only while the
		// upstream is working on the request.
		if p.MaxConcurrency > 0 {
			rp.Transport = concurrency.Limit(rp.Transport, p.MaxConcurrency, p.QueueTimeout, m.ConcurrencyWait(p.Name))
		}
		proxies[p.Name] = rp
	}

	for _, p := range providers {
		// A trailing slash makes this a subtree pattern: "/openai/" matches
		// "/openai/v1/chat/completions". StripPrefix then removes "/openai"
		// before the proxy's Rewrite joins what's left onto the provider's
		// base URL — so routing and path rewriting need no custom code.
		//
		// Auth runs first: a request without a valid gateway key never reaches
		// the proxy, so it can't spend the provider's key.
		prefix := "/" + p.Name
		// RequireMetered then refuses endpoints the gateway can't read usage
		// from, and ReadBody takes ownership of the body. Both run after
		// StripPrefix, so they see the provider-relative path.
		h := proxies[p.Name]
		// A fallback retries on the target's plain proxy: the request has
		// already passed auth and ReadBody, which serve the target as well.
		if fb := cfg.Providers[p.Name].Fallback; fb.Enabled {
			h = proxy.Fallback(p.Name, h, fb.Provider, proxies[fb.Provider], fb.Models, m.Fallback, logger)
		}
		// Rate limiting needs ReadBody's estimate, so it goes inside ReadBody.
		// It wraps the fallback, so a request is charged once, whichever
		// providers try it.
		if limiter != nil {
			h = middleware.RateLimit(limiter, logger)(h)
		}
		body := usage.ReadBody(p.Format, cfg.MaxRequestBytes)(h)
		metered := usage.RequireMetered(p.Format)(body)
		// The request metrics go outermost, so requests the gateway refuses
		// itself (401, 403, 413) are counted too.
		mux.Handle(prefix+"/", m.Requests(p.Name)(tenantAuth(http.StripPrefix(prefix, metered))))
	}

	// Anything that names no provider is rejected. There is deliberately no
	// default upstream: a typo in a base URL must never silently send traffic
	// — and spend — to the wrong provider.
	mux.Handle("/", unknownProvider(cfg.ProviderNames()))

	// RequestID goes outermost, so the log line and every response, rejected
	// or failed ones included, carry the ID.
	return middleware.RequestID(middleware.Logging(logger)(mux)), nil
}

// unknownProvider returns the handler for paths that match no configured
// provider. It lists what is configured, because the usual cause is a
// misspelled base URL.
func unknownProvider(names []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apierror.Write(w, http.StatusNotFound, "unknown_provider",
			"no provider matches this path; point your client's base URL at /<provider>",
			map[string]any{"providers": names})
	})
}

// run wires everything together and blocks until the process is asked to stop.
//
// Go note: keeping the real work in run() instead of main() means it returns an
// error normally. os.Exit skips deferred functions, so we want exactly one place
// (main) that calls it.
func run(cfg *config.Config, logger *slog.Logger) error {
	adminKey := os.Getenv("GATEWAY_ADMIN_KEY")
	if err := validateAdminKey(adminKey); err != nil {
		return fmt.Errorf("GATEWAY_ADMIN_KEY %w", err)
	}

	conn, err := openDB(os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer conn.Close()

	rdb, err := openRedis(os.Getenv("REDIS_URL"))
	if err != nil {
		return err
	}
	defer rdb.Close()

	writer := usage.NewWriter(conn, logger)
	// Deferred after conn.Close, so it runs first: by the time run returns,
	// Shutdown has drained every request, so every usage row is queued.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := writer.Close(ctx); err != nil {
			logger.Error("usage writer did not drain", "error", err)
		}
		if n := writer.Dropped(); n > 0 {
			logger.Error("usage rows were dropped", "count", n)
		}
	}()

	m := metrics.New()
	providers, err := cfg.BuildProviders()
	if err != nil {
		return err
	}
	checker := health.NewChecker(providers, m.ProviderUp, logger)

	handler, err := router(cfg, stores{tenant.NewStore(conn), usage.NewReports(conn)}, adminKey, recordUsage(writer, cfg.Providers, logger), ratelimit.New(rdb), m, checker, logger)
	if err != nil {
		return err
	}

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
		serverErr <- srv.ListenAndServe()
	}()

	// signal.NotifyContext gives us a context that is cancelled on Ctrl-C or on
	// the TERM signal Docker/Kubernetes send.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Go note: `defer` schedules a call to run when the surrounding function
	// returns, no matter which return path is taken. It's Go's version of a
	// `finally` block, and it's written next to the thing it cleans up.
	defer stop()

	// The provider checks run until ctx ends: at a shutdown signal, or when
	// run returns because the server failed.
	go checker.Run(ctx)

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

// minAdminKeyLen is the shortest admin key accepted: 32 characters, so a
// random key carries at least 128 bits even in hex.
const minAdminKeyLen = 32

// validateAdminKey rejects admin keys that are missing, short, or would
// never match. The returned error reads after the variable name.
func validateAdminKey(key string) error {
	switch {
	case key == "":
		return errors.New("is not set; the admin API needs it (generate one with: openssl rand -hex 32)")
	case strings.TrimSpace(key) != key:
		// Usually a trailing newline from an env file. A client would never
		// present it, so admin would be locked out with no error anywhere.
		return errors.New("has leading or trailing whitespace")
	case strings.HasPrefix(key, tenant.KeyPrefix):
		// That tenant's key would then pass the admin check too.
		return fmt.Errorf("must not be a tenant key (%s...)", tenant.KeyPrefix)
	case len(key) < minAdminKeyLen:
		return fmt.Errorf("must be at least %d characters, got %d", minAdminKeyLen, len(key))
	}
	return nil
}

// openDB connects to Postgres and pings it, so a missing or unreachable
// database stops startup instead of turning every request into a 503.
func openDB(dsn string) (*sql.DB, error) {
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is not set, e.g. postgres://gateway:gateway@127.0.0.1:5432/gateway?sslmode=disable")
	}

	// sql.Open only validates its arguments; the ping is what connects.
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening the database: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connecting to the database: %w", err)
	}
	return conn, nil
}

// openRedis connects to Redis and pings it, so a missing or unreachable
// Redis stops startup instead of leaving every request unlimited.
func openRedis(redisURL string) (*redis.Client, error) {
	opts, err := redisOptions(redisURL)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("connecting to Redis: %w", err)
	}
	return rdb, nil
}

// redisOptions parses REDIS_URL into the client options the rate limiter
// needs, whatever the URL asks for:
//   - ContextTimeoutEnabled, without which go-redis ignores context deadlines
//     on the socket and waits out its own read timeout, so a hung Redis
//     would hang every request.
//   - MaxRetries -1, no retries: the rate limit scripts aren't idempotent,
//     and a retry after a lost reply would charge or refund twice.
//
// Its errors never quote the URL, which can carry a password.
func redisOptions(redisURL string) (*redis.Options, error) {
	if redisURL == "" {
		return nil, errors.New("REDIS_URL is not set, e.g. redis://127.0.0.1:6379/0")
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		// url.Parse's errors quote the URL, password and all; go-redis's own
		// name only the part that's wrong.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, errors.New("REDIS_URL is not a valid URL, e.g. redis://127.0.0.1:6379/0")
		}
		return nil, fmt.Errorf("REDIS_URL is not a valid Redis URL (%w), e.g. redis://127.0.0.1:6379/0", err)
	}
	opts.ContextTimeoutEnabled = true
	opts.MaxRetries = -1
	return opts, nil
}

// recordUsage returns a usageFunc that turns each provider response into a
// usage_logs row, priced, and queues it on w. It never blocks: w drops rather
// than wait. A provider marked free in providers costs 0 per row.
func recordUsage(w *usage.Writer, providers map[string]config.ProviderConfig, logger *slog.Logger) usageFunc {
	return func(ctx context.Context, provider string, r usage.Result) {
		t, key, ok := middleware.TenantFrom(ctx)
		if !ok {
			// Auth runs before every provider route, so this is a wiring bug.
			logger.Error("usage with no tenant; row not written", "request_id", middleware.RequestIDFrom(ctx), "provider", provider)
			return
		}
		req, ok := usage.RequestFrom(ctx)
		if !ok {
			req = &usage.Request{Received: time.Now()}
		}

		row := usage.Row{
			RequestID:         middleware.RequestIDFrom(ctx),
			ProviderRequestID: r.ProviderRequestID,
			TenantID:          t.ID,
			APIKeyID:          key.ID,
			Provider:          provider,
			Model:             r.Model,
			Endpoint:          req.Path,
			Status:            r.Status,
			Usage:             r.Usage,
			Streamed:          r.Streamed,
			LatencyMS:         time.Since(req.Received).Milliseconds(),
			CreatedAt:         req.Received,
		}

		// Cost is recorded only when it is known. For a free provider it
		// always is: 0, even for a response cut short. Otherwise a response
		// cut short (or too large to parse) may be missing its usage, so its
		// cost is NULL, never a guess and never 0. A complete response with no
		// tokens, such as an upstream error, really did cost nothing.
		switch {
		case providers[provider].Free:
			row.CostMicros = new(int64)
		case !r.Complete:
			logger.Warn("usage incomplete; logged without cost",
				"request_id", row.RequestID, "provider", provider, "model", r.Model)
		case r.Usage.Tokens() == 0:
			row.CostMicros = new(int64)
		default:
			if cost, ok := pricing.Cost(provider, r.Usage, req.Model, req.Received); ok {
				row.CostMicros = &cost
			} else {
				logger.Warn("model not priced; usage logged without cost",
					"request_id", row.RequestID, "provider", provider, "model", r.Model)
			}
		}

		w.Write(row)
	}
}
