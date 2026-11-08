package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The Redis the budget ledger uses, apart from REDIS_URL's: the options both
// URLs are parsed with, the client the ledger gets, the guard that refuses a
// ledger Redis that can evict, the check /ready runs on it, and the two
// variables' errors.

// infoMemory returns INFO memory text as Redis sends it: the lines end in
// \r\n, and maxmemory_human sits between maxmemory and maxmemory_policy.
func infoMemory(maxmemory int64, policy string) string {
	return fmt.Sprintf("# Memory\r\nused_memory:1529904\r\nmaxmemory:%d\r\nmaxmemory_human:256.00M\r\nmaxmemory_policy:%s\r\nallocator_frag_ratio:1.00\r\n", maxmemory, policy)
}

// TestCheckEviction: the guard on the ledger's Redis refuses a memory cap
// under any policy but noeviction, naming the policy, and otherwise lets
// startup go on, with a Warn when INFO couldn't tell it.
func TestCheckEviction(t *testing.T) {
	const capped = 64 << 20
	type testCase struct {
		name    string
		info    string
		infoErr error
		refused string // the policy the error must name; "" when startup goes on
		warned  bool
	}
	cases := []testCase{
		{name: "noeviction with a cap", info: infoMemory(capped, "noeviction")},
		{name: "noeviction without one", info: infoMemory(0, "noeviction")},
		{name: "allkeys-lru without a cap", info: infoMemory(0, "allkeys-lru")},
		{name: "volatile-ttl without a cap", info: infoMemory(0, "volatile-ttl")},
		// maxmemory_human is not maxmemory, whichever comes first.
		{name: "maxmemory_human first", info: "maxmemory_human:256.00M\r\nmaxmemory:268435456\r\nmaxmemory_policy:allkeys-lru\r\n", refused: "allkeys-lru"},

		{name: "an INFO error", infoErr: errors.New("NOPERM this user has no permissions to run the 'info' command"), warned: true},
		{name: "an empty reply", warned: true},
		{name: "no fields", info: "# Memory\r\nused_memory:1529904\r\n", warned: true},
		{name: "no maxmemory", info: "maxmemory_policy:allkeys-lru\r\n", warned: true},
		{name: "no policy", info: "maxmemory:268435456\r\n", warned: true},
		{name: "a maxmemory that isn't a number", info: "maxmemory:lots\r\nmaxmemory_policy:allkeys-lru\r\n", warned: true},
	}
	for _, policy := range []string{"allkeys-lru", "allkeys-lfu", "allkeys-random", "volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl"} {
		cases = append(cases, testCase{name: policy + " with a cap", info: infoMemory(capped, policy), refused: policy})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			err := checkEviction(tc.info, tc.infoErr, slog.New(slog.NewJSONHandler(&logs, nil)))
			switch {
			case tc.refused != "":
				if err == nil || !strings.Contains(err.Error(), tc.refused) ||
					!strings.Contains(err.Error(), "BUDGET_REDIS_URL") || !strings.Contains(err.Error(), "noeviction") {
					t.Errorf("checkEviction() = %v, want an error naming %s, BUDGET_REDIS_URL and noeviction", err, tc.refused)
				}
			case err != nil:
				t.Errorf("checkEviction() = %v, want startup to go on", err)
			}
			if warned := strings.Contains(logs.String(), `"level":"WARN"`); warned != tc.warned {
				t.Errorf("warned = %v, want %v; log:\n%s", warned, tc.warned, logs.String())
			}
		})
	}
}

// TestRedisOptionsKeepTLS: a rediss:// URL keeps the TLS config go-redis
// builds from it, with the server name set to the URL's host and TLS 1.2 as
// the minimum, and keeps its password, for REDIS_URL and BUDGET_REDIS_URL
// alike. A redis:// URL gets no TLS. Nothing connects.
func TestRedisOptionsKeepTLS(t *testing.T) {
	for _, name := range []string{"REDIS_URL", "BUDGET_REDIS_URL"} {
		t.Run(name, func(t *testing.T) {
			opts, err := redisOptions(name, "rediss://:s3cret@redis.example.com:6380/2")
			if err != nil {
				t.Fatalf("redisOptions() returned error: %v", err)
			}
			if opts.TLSConfig == nil {
				t.Fatal("redisOptions() dropped the TLS config of a rediss:// URL")
			}
			if opts.TLSConfig.ServerName != "redis.example.com" || opts.TLSConfig.MinVersion != tls.VersionTLS12 || opts.TLSConfig.InsecureSkipVerify {
				t.Errorf("TLS config: ServerName %q, MinVersion %#x, InsecureSkipVerify %v; want redis.example.com, %#x (TLS 1.2), false",
					opts.TLSConfig.ServerName, opts.TLSConfig.MinVersion, opts.TLSConfig.InsecureSkipVerify, tls.VersionTLS12)
			}
			if opts.Password != "s3cret" || opts.Addr != "redis.example.com:6380" || opts.DB != 2 {
				t.Errorf("Password %q, Addr %q, DB %d; want s3cret, redis.example.com:6380, 2", opts.Password, opts.Addr, opts.DB)
			}
			if !opts.ContextTimeoutEnabled || opts.MaxRetries != -1 {
				t.Errorf("ContextTimeoutEnabled %v, MaxRetries %d; want true, -1", opts.ContextTimeoutEnabled, opts.MaxRetries)
			}

			plain, err := redisOptions(name, "redis://:s3cret@127.0.0.1:6379/0")
			if err != nil {
				t.Fatalf("redisOptions() returned error: %v", err)
			}
			if plain.TLSConfig != nil || plain.Password != "s3cret" {
				t.Errorf("a redis:// URL: TLSConfig %v, Password %q; want none, s3cret", plain.TLSConfig, plain.Password)
			}
		})
	}
}

// redisSetting returns the value of the CONFIG parameter name on rdb's Redis.
func redisSetting(t *testing.T, rdb *redis.Client, name string) string {
	t.Helper()
	m, err := rdb.ConfigGet(context.Background(), name).Result()
	if err != nil {
		t.Fatalf("CONFIG GET %s: %v", name, err)
	}
	return m[name]
}

// evictingRedis returns the client for REDIS_URL's Redis, and its policy, when
// that Redis has a memory cap and a policy other than noeviction, as the
// compose redis has. Otherwise the test is skipped: CI's Redis has no cap.
func evictingRedis(t *testing.T) (*redis.Client, string) {
	t.Helper()
	rdb := gatewayRedis(t)
	policy := redisSetting(t, rdb, "maxmemory-policy")
	if redisSetting(t, rdb, "maxmemory") == "0" || policy == "noeviction" {
		t.Skip("REDIS_URL's Redis has no memory cap or runs under noeviction; skipping the eviction tests")
	}
	return rdb, policy
}

// TestOpenLedgerRedisRefusesARedisThatCanEvict: with BUDGET_REDIS_URL unset,
// the ledger would share REDIS_URL's Redis, and the guard refuses that one
// when it has a cap and evicts. The error names the policy and says where to
// point BUDGET_REDIS_URL.
func TestOpenLedgerRedisRefusesARedisThatCanEvict(t *testing.T) {
	rdb, policy := evictingRedis(t)

	_, err := openLedgerRedis(rdb, "", discardLogger)
	if err == nil || !strings.Contains(err.Error(), policy) || !strings.Contains(err.Error(), "BUDGET_REDIS_URL") {
		t.Errorf("openLedgerRedis() = %v, want an error naming %s and BUDGET_REDIS_URL", err, policy)
	}
}

// TestOpenLedgerRedisUsesBudgetRedisURL: with BUDGET_REDIS_URL set, the ledger
// gets a client of its own, for a Redis the guard passes, and what it writes
// lands in that Redis and not in REDIS_URL's.
func TestOpenLedgerRedisUsesBudgetRedisURL(t *testing.T) {
	budgetURL := os.Getenv("BUDGET_REDIS_URL")
	if budgetURL == "" {
		t.Skip("BUDGET_REDIS_URL not set; skipping the ledger Redis tests")
	}
	rdb := gatewayRedis(t)

	ledger, err := openLedgerRedis(rdb, budgetURL, discardLogger)
	if err != nil {
		t.Fatalf("openLedgerRedis() with BUDGET_REDIS_URL returned error: %v", err)
	}
	t.Cleanup(func() { ledger.Close() })
	if ledger == rdb {
		t.Fatal("the ledger shares REDIS_URL's client, though BUDGET_REDIS_URL is set")
	}
	if ledger.Options().Addr == rdb.Options().Addr {
		t.Skip("BUDGET_REDIS_URL and REDIS_URL name the same Redis; there is nothing to tell apart")
	}

	ctx := context.Background()
	const key = "test:ledger-redis"
	if err := ledger.Set(ctx, key, 1, time.Minute).Err(); err != nil {
		t.Fatalf("writing through the ledger's client: %v", err)
	}
	t.Cleanup(func() { ledger.Del(ctx, key) })
	if n, err := rdb.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Errorf("a key written through the ledger's client is in REDIS_URL's Redis too: Exists = %d, %v", n, err)
	}
}

// TestOpenLedgerRedisSharesTheClientWhenUnset: with BUDGET_REDIS_URL unset,
// the ledger gets the very client it is given, REDIS_URL's, not a second one
// to the same URL.
func TestOpenLedgerRedisSharesTheClientWhenUnset(t *testing.T) {
	// The guard runs on this client, so it must be one for a Redis that passes
	// it. REDIS_URL's, under the compose stack, is capped and evicts, and is
	// refused above; BUDGET_REDIS_URL's passes. CI has only REDIS_URL, with no
	// cap.
	var rdb *redis.Client
	if u := os.Getenv("BUDGET_REDIS_URL"); u != "" {
		var err error
		if rdb, err = openRedis("BUDGET_REDIS_URL", u); err != nil {
			t.Fatalf("openRedis() returned error: %v", err)
		}
		t.Cleanup(func() { rdb.Close() })
	} else {
		rdb = gatewayRedis(t)
	}

	ledger, err := openLedgerRedis(rdb, "", discardLogger)
	if err != nil {
		t.Fatalf("openLedgerRedis() returned error: %v", err)
	}
	if ledger != rdb {
		t.Error("with BUDGET_REDIS_URL unset, the ledger got a client of its own, not the one it was given")
	}
}

// TestReadyChecks: /ready checks Postgres and Redis, and the budget ledger's
// Redis too when the ledger has a client of its own. redis_budget pings the
// ledger's client, and redis pings the other.
func TestReadyChecks(t *testing.T) {
	good := gatewayRedis(t)
	// Port 1 on loopback: nothing listens there, so every command fails.
	opts, err := redisOptions("REDIS_URL", "redis://127.0.0.1:1/0")
	if err != nil {
		t.Fatal(err)
	}
	opts.DialerRetries = 1
	dead := redis.NewClient(opts)
	t.Cleanup(func() { dead.Close() })

	// The database is never pinged here.
	shared := readyChecks(nil, good, good)
	if len(shared) != 2 || shared["postgres"] == nil || shared["redis"] == nil {
		t.Errorf("with a shared client, the checks are %v, want postgres and redis only", shared)
	}

	ctx := context.Background()
	cases := []struct {
		name                       string
		rdb, ledgerRdb             *redis.Client
		redisDown, redisBudgetDown bool
	}{
		{"the ledger's Redis down", good, dead, false, true},
		{"the other Redis down", dead, good, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checks := readyChecks(nil, tc.rdb, tc.ledgerRdb)
			if len(checks) != 3 || checks["postgres"] == nil {
				t.Fatalf("with a client of its own, the checks are %v, want postgres, redis and redis_budget", checks)
			}
			if err := checks["redis"](ctx); (err != nil) != tc.redisDown {
				t.Errorf("redis check returned %v, want down: %v", err, tc.redisDown)
			}
			if err := checks["redis_budget"](ctx); (err != nil) != tc.redisBudgetDown {
				t.Errorf("redis_budget check returned %v, want down: %v", err, tc.redisBudgetDown)
			}
		})
	}
}

// wrongPasswordURL returns REDIS_URL with its password swapped for a wrong
// one, with that password and the right one. The test is skipped when
// REDIS_URL carries no password: a Redis with none accepts any, as CI's does.
func wrongPasswordURL(t *testing.T) (wrongURL, wrong, right string) {
	t.Helper()
	u, err := url.Parse(os.Getenv("REDIS_URL"))
	if err != nil {
		t.Fatalf("parsing REDIS_URL: %v", err)
	}
	right, ok := u.User.Password()
	if !ok {
		t.Skip("REDIS_URL carries no password; skipping the wrong-password tests")
	}
	wrong = "wrong-password-0123"
	u.User = url.UserPassword(u.User.Username(), wrong)
	return u.String(), wrong, right
}

// TestRunRefusesAWrongRedisPassword: a wrong password in either URL stops
// startup with an error that names the variable, and neither the error nor the
// log holds a password.
func TestRunRefusesAWrongRedisPassword(t *testing.T) {
	wrongURL, wrong, right := wrongPasswordURL(t)

	for _, variable := range []string{"REDIS_URL", "BUDGET_REDIS_URL"} {
		t.Run(variable, func(t *testing.T) {
			// REDIS_URL keeps its right password when the wrong one is the
			// ledger's.
			t.Setenv(variable, wrongURL)

			logs, err := runLogged(t, loadConfig(t, fmt.Sprintf(ollamaConfig, "http://127.0.0.1:1")))
			if err == nil || !strings.HasPrefix(err.Error(), "connecting to Redis with "+variable+": ") {
				t.Fatalf("run() returned %v, want an error from connecting to Redis with %s", err, variable)
			}
			for what, text := range map[string]string{"error": err.Error(), "log": logs} {
				for _, password := range []string{wrong, right} {
					if strings.Contains(text, password) {
						t.Errorf("the %s holds a password:\n%s", what, text)
					}
				}
			}
		})
	}
}

// TestRunRefusesALedgerRedisThatCanEvict: with BUDGET_REDIS_URL unset, run()
// refuses to start when REDIS_URL's Redis, which the ledger then shares, has a
// memory cap and evicts, as the compose redis does. It opens no port.
func TestRunRefusesALedgerRedisThatCanEvict(t *testing.T) {
	_, policy := evictingRedis(t)
	t.Setenv("BUDGET_REDIS_URL", "")

	logs, err := runLogged(t, loadConfig(t, fmt.Sprintf(ollamaConfig, "http://127.0.0.1:1")))
	if err == nil || !strings.Contains(err.Error(), policy) || !strings.Contains(err.Error(), "BUDGET_REDIS_URL") {
		t.Fatalf("run() returned %v, want the guard's error naming %s and BUDGET_REDIS_URL", err, policy)
	}
	if strings.Contains(logs, "gateway listening") {
		t.Errorf("run() said it was listening:\n%s", logs)
	}
}
