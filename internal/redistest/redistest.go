// Package redistest gives tests a client for a real Redis.
//
// Like dbtest, it lives outside the _test.go files so more than one package's
// tests can use it.
package redistest

import (
	"context"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

// New returns a client for the Redis that REDIS_URL names, closed when the
// test ends. It sets ContextTimeoutEnabled, as the gateway must: without it,
// go-redis ignores context deadlines while it waits on Redis.
//
// Tests that need Redis skip when REDIS_URL is unset, so a plain
// `go test ./...` still works without one.
//
// Unlike dbtest's databases, this Redis is shared by every test, so each test
// must use keys no other test uses, e.g. under a random tenant ID.
func New(t *testing.T) *redis.Client {
	t.Helper()

	u := os.Getenv("REDIS_URL")
	if u == "" {
		t.Skip("REDIS_URL not set; skipping Redis tests")
	}

	opts, err := redis.ParseURL(u)
	if err != nil {
		t.Fatalf("parsing REDIS_URL: %v", err)
	}
	opts.ContextTimeoutEnabled = true

	rdb := redis.NewClient(opts)
	t.Cleanup(func() { rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("connecting to Redis: %v", err)
	}
	return rdb
}
