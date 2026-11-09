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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	// Registers the "pgx" driver with database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/grishkadel/llm-gateway/internal/admin"
	"github.com/grishkadel/llm-gateway/internal/apierror"
	"github.com/grishkadel/llm-gateway/internal/budget"
	"github.com/grishkadel/llm-gateway/internal/cache"
	"github.com/grishkadel/llm-gateway/internal/concurrency"
	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/health"
	"github.com/grishkadel/llm-gateway/internal/metrics"
	"github.com/grishkadel/llm-gateway/internal/middleware"
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
// provider that served it, and each cache hit's, with the name of its route's
// provider.
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

// budgetLedger is what the routes need from the budget ledger: Reserve and
// Settle for the provider routes, and Spent for the admin API. *budget.Ledger
// satisfies it; routing tests substitute a fake.
type budgetLedger interface {
	middleware.Ledger
	admin.Ledger
}

// router builds the gateway's handler: one reverse proxy per configured
// provider behind tenant auth, the response cache, rate limiting and budgets,
// the admin API behind the admin key, /health, /ready, and a catch-all that
// rejects unknown prefixes, all behind the request ID and logging middleware.
// A nil limiter means no rate limiting, a nil cacheStore no response cache,
// a nil ledger no budgets and no budget report in the admin API, and a nil
// ready no /ready. /metrics isn't here: it has a port of its own.
//
// It's separated from run() so tests can exercise routing without starting a
// real server or handling signals.
func router(cfg *config.Config, store tenantStore, adminKey string, onUsage usageFunc, limiter middleware.Limiter, cacheStore middleware.CacheStore, ledger budgetLedger, m *metrics.Metrics, checker *health.Checker, ready http.Handler, logger *slog.Logger) (http.Handler, error) {
	providers, err := cfg.BuildProviders()
	if err != nil {
		return nil, err
	}

	// http.ServeMux is the standard library router. The most specific pattern
	// wins, so "/health" beats "/openai/" beats the catch-all "/".
	mux := http.NewServeMux()
	mux.Handle("/health", health.Handler(checker))
	// Handle panics on a nil handler, so no handler means no route, and the
	// catch-all answers.
	if ready != nil {
		mux.Handle("/ready", ready)
	}

	// One subtree behind AdminAuth, not a pattern per admin route: with
	// per-route patterns, ServeMux would answer an unauthenticated request for
	// the wrong method with 405 and an Allow header, before any auth ran.
	mux.Handle("/admin/", middleware.AdminAuth(adminKey)(admin.Handler(store, ledger, logger)))

	tenantAuth := middleware.Auth(store, logger)

	// Every provider's proxy first, budgeted when there are budgets, so a
	// route can hand a failed request to its fallback's. Each provider's
	// meter is kept too, for its route's cache to report hits to.
	proxies := make(map[string]http.Handler, len(providers))
	meters := make(map[string]usage.Callback, len(providers))
	for _, p := range providers {
		name, free := p.Name, cfg.Providers[p.Name].Free
		meter := func(ctx context.Context, r usage.Result) {
			// For the rate limiter to settle the request's charge by, the
			// budget the attempt's reservation, and the cache to decide by
			// whether to store the response.
			middleware.ReportUsage(ctx, name, r)
			// Priced once, as recordUsage prices the usage row, for the
			// metrics and the request line alike: an incomplete paid response
			// at what its budget holds. The model label comes from the names
			// it was priced by: the response's model, then the one this
			// attempt asked for.
			cost := middleware.UsageCost(ctx, name, free, r)
			t, _, _ := middleware.TenantFrom(ctx)
			requested := ""
			if req, ok := usage.RequestFrom(ctx); ok {
				requested = req.Model
			}
			m.Usage(t.ID, name, r.Usage, requested, cost)
			// For the request line.
			if rec := middleware.RecordFrom(ctx); rec != nil {
				rec.Usage = &middleware.UsageReport{Provider: name, Result: r, CostMicros: cost}
			}
			if onUsage != nil {
				onUsage(ctx, name, r)
			}
		}
		rp := proxy.New(p, logger, meter)
		// Each attempt's TTFB is timed right around the provider's transport,
		// inside the limiter, so it starts once the attempt has a slot.
		rp.Transport = m.UpstreamTTFB(p.Name, rp.Transport)
		// The limiter wraps the transport, so a slot is held only while the
		// upstream is working on the request.
		if p.MaxConcurrency > 0 {
			rp.Transport = concurrency.Limit(rp.Transport, p.MaxConcurrency, p.QueueTimeout, m.ConcurrencyWait(p.Name))
		}
		// The request's overhead is timed outermost, to its first attempt's
		// handoff, before the limiter: the wait for a slot is in neither
		// timing.
		rp.Transport = m.Overhead(rp.Transport)
		proxies[p.Name] = rp
		// Budget wraps the proxy itself, so each attempt is reserved at the
		// prices of the provider that serves it, on its own route or as
		// another's fallback.
		if ledger != nil {
			proxies[p.Name] = middleware.Budget(ledger, p.Name, cfg.Providers[p.Name].Free, logger)(rp)
		}
		meters[p.Name] = meter
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
		// A fallback retries on the target's proxy, budgeted at the target's
		// prices: the request has already passed auth and ReadBody, which
		// serve the target as well.
		if fb := cfg.Providers[p.Name].Fallback; fb.Enabled {
			h = proxy.Fallback(p.Name, h, fb.Provider, proxies[fb.Provider], fb.Models, m.Fallback, logger)
		}
		// Rate limiting needs ReadBody's estimate, so it goes inside ReadBody.
		// It wraps the fallback, so a request is charged once, whichever
		// providers try it.
		if limiter != nil {
			h = middleware.RateLimit(limiter, logger)(h)
		}
		// The cache keys ReadBody's body, so it goes inside ReadBody too. It
		// wraps the rate limit, so a hit is never charged, nor reserved
		// against a budget. A hit reports to the route's meter for its usage
		// row and its request line, and for nothing else: ReportUsage finds no
		// charge, reservation or pending miss on its request, and it has no
		// tokens to count.
		if cacheStore != nil {
			// A hit never reaches Fallback, so on a route with one it is given
			// the headers Fallback puts on every other response.
			var hitHeader http.Header
			if cfg.Providers[p.Name].Fallback.Enabled {
				hitHeader = http.Header{proxy.HeaderProvider: {p.Name}, proxy.HeaderFallback: {"false"}}
			}
			h = middleware.Cache(cacheStore, p.Name, meters[p.Name], hitHeader, logger)(h)
		}
		// RecordRequest, just inside ReadBody, records the client's model and
		// stream flag for the request line, whatever refuses the request next.
		body := usage.ReadBody(p.Format, cfg.MaxRequestBytes)(middleware.RecordRequest(h))
		metered := usage.RequireMetered(p.Format)(body)
		// The request metrics go outermost, so requests the gateway refuses
		// itself (401, 403, 413) are counted too. Route records the route
		// outside Auth, so their request lines name it too.
		mux.Handle(prefix+"/", m.Requests(p.Name)(middleware.Route(p.Name)(tenantAuth(http.StripPrefix(prefix, metered)))))
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

// metricsHandler returns the handler of the metrics port: m's metrics at
// /metrics and nothing else, behind the admin key. A request without it as a
// Bearer token, or with a tenant's key, gets the admin API's 401 whatever its
// path, as under /admin/; with it, any other path gets a JSON 404.
//
// It has no request ID and no request line: Prometheus scrapes it every few
// seconds, and a line for each scrape would bury the log.
func metricsHandler(m *metrics.Metrics, adminKey string) http.Handler {
	serveMetrics := m.Handler()
	return middleware.AdminAuth(adminKey)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			apierror.Write(w, http.StatusNotFound, "not_found", "this port serves only /metrics", nil)
			return
		}
		serveMetrics.ServeHTTP(w, r)
	}))
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

	// go-redis's own log lines go through logger. This is process-wide and
	// goes before the first connection, so a dial that fails at startup is
	// logged as JSON too.
	redis.SetLogger(redisLogger{logger})
	rdb, err := openRedis("REDIS_URL", os.Getenv("REDIS_URL"))
	if err != nil {
		return err
	}
	defer rdb.Close()

	// The ledger shares rdb unless BUDGET_REDIS_URL gives it a Redis of its
	// own, and either way that Redis must not be one that can evict.
	ledgerRdb, err := openLedgerRedis(rdb, os.Getenv("BUDGET_REDIS_URL"), logger)
	if err != nil {
		return err
	}
	if ledgerRdb != rdb {
		defer ledgerRdb.Close()
	}

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
	m.UsageWriter(writer.Queued, writer.Dropped)
	providers, err := cfg.BuildProviders()
	if err != nil {
		return err
	}
	checker := health.NewChecker(providers, m.ProviderUp, logger)

	ready := health.ReadyHandler(readyChecks(conn, rdb, ledgerRdb), logger)

	handler, err := router(cfg, stores{tenant.NewStore(conn), usage.NewReports(conn)}, adminKey, recordUsage(writer, cfg.Providers, logger), ratelimit.New(rdb), cache.New(rdb, cfg.Cache.TTL()), budget.New(ledgerRdb, conn, writer.Pending), m, checker, ready, logger)
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
	// /metrics has a port of its own, behind the admin key.
	metricsSrv := &http.Server{
		Addr:    cfg.MetricsAddr(),
		Handler: metricsHandler(m, adminKey),
		// As the main server's.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Go note: ListenAndServe is net.Listen followed by Serve. Doing the two
	// by hand opens both ports before either server serves, so a port that is
	// taken ends startup here, with an error naming its address, and not in a
	// goroutine after the gateway has said it is listening. Serve closes the
	// listener it is given when it returns, so a listener is closed by hand
	// only when its server never starts.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("gateway listener: %w", err)
	}
	metricsLn, err := net.Listen("tcp", metricsSrv.Addr)
	if err != nil {
		ln.Close()
		return fmt.Errorf("metrics listener: %w", err)
	}
	logger.Info("gateway listening",
		"addr", srv.Addr,
		"metrics_addr", metricsSrv.Addr,
		"providers", cfg.ProviderNames(),
	)

	// Go note: a channel is a typed pipe used to pass values between
	// goroutines. Here it carries the servers' exit errors out of the
	// goroutines we start them in. Buffered with size 2, one send for each
	// server, so a send never blocks, even if nobody is reading yet.
	serverErr := make(chan error, 2)

	// `go f()` runs f concurrently. Serve blocks until its server is shut down,
	// so each goes on its own goroutine while main waits for a shutdown signal.
	go func() { serverErr <- srv.Serve(ln) }()
	go func() { serverErr <- metricsSrv.Serve(metricsLn) }()

	// signal.NotifyContext gives us a context that is cancelled on Ctrl-C or on
	// the TERM signal Docker/Kubernetes send.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Go note: `defer` schedules a call to run when the surrounding function
	// returns, no matter which return path is taken. It's Go's version of a
	// `finally` block, and it's written next to the thing it cleans up.
	defer stop()

	// The provider checks run until ctx ends: at a shutdown signal, or when
	// run returns because a server failed.
	go checker.Run(ctx)

	// select waits on several channel operations at once and proceeds with
	// whichever is ready first.
	select {
	case err := <-serverErr:
		// From either server. ErrServerClosed is the expected error after a
		// graceful Shutdown, so it isn't a real failure.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil

	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// Shutdown stops accepting new connections and waits for in-flight
		// requests to finish, up to the deadline above. Both servers get that
		// one deadline. The main server drains first, so a scrape still works
		// while it does, and the metrics server goes down after it even if the
		// main one ran out of time.
		//
		// Go note: errors.Join returns one error holding every non-nil error
		// it is given, or nil when there are none.
		if err := errors.Join(srv.Shutdown(shutdownCtx), metricsSrv.Shutdown(shutdownCtx)); err != nil {
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

// openRedis connects to the Redis that redisURL, the value of the environment
// variable name, points at, and pings it, so a missing or unreachable Redis
// stops startup instead of leaving every request unlimited, and every paid
// one refused. Its errors name the variable, and never quote the URL.
func openRedis(name, redisURL string) (*redis.Client, error) {
	opts, err := redisOptions(name, redisURL)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("connecting to Redis with %s: %w", name, err)
	}
	return rdb, nil
}

// redisOptions parses redisURL, the value of the environment variable name
// (REDIS_URL or BUDGET_REDIS_URL), into the client options the rate limiter,
// the cache and the budget ledger need, whatever the URL asks for:
//   - ContextTimeoutEnabled, without which go-redis ignores context deadlines
//     on the socket and waits out its own read timeout, so a hung Redis
//     would hang every request.
//   - MaxRetries -1, no retries: the rate limit and budget scripts aren't
//     idempotent, and a retry after a lost reply would charge, refund,
//     reserve or settle twice.
//
// Its errors name the variable, and never quote the URL, which can carry a
// password.
func redisOptions(name, redisURL string) (*redis.Options, error) {
	if redisURL == "" {
		return nil, fmt.Errorf("%s is not set, e.g. redis://127.0.0.1:6379/0", name)
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		// url.Parse's errors quote the URL, password and all; go-redis's own
		// name only the part that's wrong.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, fmt.Errorf("%s is not a valid URL, e.g. redis://127.0.0.1:6379/0", name)
		}
		return nil, fmt.Errorf("%s is not a valid Redis URL (%w), e.g. redis://127.0.0.1:6379/0", name, err)
	}
	opts.ContextTimeoutEnabled = true
	opts.MaxRetries = -1
	return opts, nil
}

// openLedgerRedis returns the Redis client for the budget ledger: rdb itself
// when budgetURL, the value of BUDGET_REDIS_URL, is empty, and otherwise a
// client of its own for that URL. The rate limiter and the cache always use
// rdb. It stops startup when the ledger's Redis can evict keys, as
// checkEviction decides.
func openLedgerRedis(rdb *redis.Client, budgetURL string, logger *slog.Logger) (*redis.Client, error) {
	ledger := rdb
	if budgetURL != "" {
		var err error
		if ledger, err = openRedis("BUDGET_REDIS_URL", budgetURL); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, infoErr := ledger.Info(ctx, "memory").Result()
	if err := checkEviction(info, infoErr, logger); err != nil {
		if ledger != rdb {
			ledger.Close()
		}
		return nil, err
	}
	return ledger, nil
}

// checkEviction is the guard on the budget ledger's Redis. info is the text of
// its INFO memory reply, and infoErr the error from asking for it. Every
// budget key has a TTL, so any eviction policy, the volatile-* ones included,
// can drop a ledger, and the rebuild from Postgres can't see the reservations
// in flight at that moment. So when the Redis has a memory cap (maxmemory
// above 0) and a policy other than noeviction, checkEviction returns an error
// that names the policy and says what to do.
//
// Some managed Redis services restrict INFO. When it fails, or the reply
// lacks the fields, checkEviction logs a Warn and lets startup go on.
func checkEviction(info string, infoErr error, logger *slog.Logger) error {
	limit, limitErr := strconv.ParseInt(infoField(info, "maxmemory"), 10, 64)
	policy := infoField(info, "maxmemory_policy")
	if infoErr != nil || limitErr != nil || policy == "" {
		if infoErr == nil {
			infoErr = errors.New("the reply has no usable maxmemory or maxmemory_policy")
		}
		logger.Warn("can't tell whether the budget ledger's Redis can evict keys; it must run under maxmemory-policy noeviction", "error", infoErr)
		return nil
	}
	if limit > 0 && policy != "noeviction" {
		return fmt.Errorf("the budget ledger's Redis (BUDGET_REDIS_URL, or REDIS_URL when that is unset) has a memory cap and maxmemory-policy %s, so it can evict budget keys; point BUDGET_REDIS_URL at a Redis under maxmemory-policy noeviction", policy)
	}
	return nil
}

// infoField returns the value of the field name in the text of an INFO reply,
// or "" when there is none. A line is the name, a colon and the value, and
// ends in "\r\n". Matching the colon too keeps maxmemory apart from
// maxmemory_human and the other fields that begin with it.
func infoField(info, name string) string {
	for line := range strings.Lines(info) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), name+":"); ok {
			return value
		}
	}
	return ""
}

// readyChecks returns the checks for GET /ready: Postgres and Redis, and the
// budget ledger's Redis when it is a client of its own, not rdb.
func readyChecks(conn *sql.DB, rdb, ledgerRdb *redis.Client) map[string]func(context.Context) error {
	checks := map[string]func(context.Context) error{
		"postgres": conn.PingContext,
		"redis":    func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
	}
	if ledgerRdb != rdb {
		checks["redis_budget"] = func(ctx context.Context) error { return ledgerRdb.Ping(ctx).Err() }
	}
	return checks
}

// recordUsage returns a usageFunc that turns each provider response, and each
// cache hit, into a usage_logs row, priced, and queues it on w. It never
// blocks: w drops rather than wait. A cache hit costs 0, and so does every row
// of a provider marked free in providers. A paid response whose usage is
// incomplete costs what its budget holds (middleware.UsageCost).
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
			UsageComplete:     r.UsageComplete,
			Cached:            r.Cached,
			Streamed:          r.Streamed,
			LatencyMS:         time.Since(req.Received).Milliseconds(),
			CreatedAt:         req.Received,
		}

		// Cost is recorded only when it is known, and is NULL otherwise.
		// Budget settles a paid attempt at the same figure, so the ledger is
		// charged exactly the row's cost. Only a router with no budgets
		// leaves incomplete usage without one.
		free := providers[provider].Free
		row.CostMicros = middleware.UsageCost(ctx, provider, free, r)
		switch {
		case row.CostMicros == nil && !r.UsageComplete:
			logger.Warn("usage incomplete; logged without cost",
				"request_id", row.RequestID, "provider", provider, "model", r.Model)
		case row.CostMicros == nil:
			logger.Warn("model not priced; usage logged without cost",
				"request_id", row.RequestID, "provider", provider, "model", r.Model)
		case !r.UsageComplete && !free:
			logger.Warn("usage incomplete; charged the budget's hold",
				"request_id", row.RequestID, "provider", provider, "model", r.Model, "cost_micros", *row.CostMicros)
		}

		w.Write(row)
	}
}
