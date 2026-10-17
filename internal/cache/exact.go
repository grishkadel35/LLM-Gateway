// Package cache is the gateway's exact-match response cache. Key names a
// request, and a Store keeps a response in Redis under that name, so that an
// identical request can be answered without calling the provider.
//
// Entries are per tenant: every key carries its tenant, in its name and in its
// hash, so one tenant never reads another's.
//
// It is not the providers' prompt caching, which still runs the model and only
// discounts the input tokens the provider has seen before. This cache keeps
// whole responses.
package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// MaxBody is the largest body Set stores: 1 MiB. It bounds what one entry can
// add to Redis's memory. Text answers fit, and generated images mostly don't.
const MaxBody = 1 << 20

// prefix starts every value Set writes, and names the layout of the rest. A
// later layout gets another prefix, so Get can tell its own values from these.
const prefix = "v1\n"

// Entry is a cached response: what a hit replays to the client.
//
// Go note: the `json:"..."` after each field is a struct tag, which
// encoding/json reads. It names the field in the JSON, and "-" leaves the
// field out of it.
type Entry struct {
	// Status is the response's HTTP status, always a 2xx: Set stores nothing
	// else.
	Status int `json:"status"`
	// ContentType is the response's Content-Type header.
	ContentType string `json:"content_type"`
	// Model is the model the original request's usage row named, for the row a
	// hit writes.
	Model string `json:"model"`
	// Body is the response body exactly as the client got it. It is stored
	// after the JSON as raw bytes, so it needs no escaping.
	Body []byte `json:"-"`
}

// Store keeps entries in Redis, each for a fixed time.
type Store struct {
	rdb redis.Cmdable
	ttl time.Duration
}

// New returns a Store that keeps its entries in rdb, usually a *redis.Client,
// each for ttl. A context's deadline reaches Redis only if the client was built
// with ContextTimeoutEnabled: otherwise go-redis ignores it while it waits.
func New(rdb redis.Cmdable, ttl time.Duration) *Store {
	return &Store{rdb: rdb, ttl: ttl}
}

// Get returns the entry stored under key, and false when there is none. A
// value that isn't a current-version entry counts as none, not as an error:
// it may come from another layout, or from other code using the key. The next
// Set overwrites it. Any error from Redis, a passed deadline included, is
// returned.
func (s *Store) Get(ctx context.Context, key string) (Entry, bool, error) {
	value, err := s.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	e, ok := decode(value)
	return e, ok, nil
}

// decode reads a value Set wrote, or reports ok == false when value isn't a
// current-version entry.
func decode(value []byte) (e Entry, ok bool) {
	rest, ok := bytes.CutPrefix(value, []byte(prefix))
	if !ok {
		return Entry{}, false
	}
	header, body, ok := bytes.Cut(rest, []byte("\n"))
	if !ok {
		return Entry{}, false
	}
	// A status that isn't 2xx is no entry Set wrote. Replaying one could even
	// panic: a header without a status decodes as 0, and net/http's
	// WriteHeader panics on a status below 100.
	if err := json.Unmarshal(header, &e); err != nil || !is2xx(e.Status) {
		return Entry{}, false
	}
	e.Body = body
	return e, true
}

// is2xx reports whether status is a success, the only kind of response the
// cache keeps.
func is2xx(status int) bool { return status >= 200 && status <= 299 }

// Set stores e under key for the Store's ttl, replacing any entry there. A body
// over MaxBody, a status that isn't 2xx, or a ttl that isn't positive is an
// error, and nothing is sent to Redis.
//
// The value is "v1\n", the JSON of everything but the body, "\n", and then the
// body's raw bytes. JSON escapes every newline inside a string, so the first
// "\n" after the prefix ends the JSON, and the body can hold any bytes at all.
func (s *Store) Set(ctx context.Context, key string, e Entry) error {
	if len(e.Body) > MaxBody {
		return fmt.Errorf("cache: body is %d bytes, over the limit of %d", len(e.Body), MaxBody)
	}
	if !is2xx(e.Status) {
		return fmt.Errorf("cache: only a 2xx response is stored, got status %d", e.Status)
	}
	if s.ttl <= 0 {
		return fmt.Errorf("cache: ttl must be positive, got %v", s.ttl)
	}

	header, err := json.Marshal(e)
	if err != nil {
		return err
	}
	value := make([]byte, 0, len(prefix)+len(header)+1+len(e.Body))
	value = append(value, prefix...)
	value = append(value, header...)
	value = append(value, '\n')
	value = append(value, e.Body...)

	// Go note: SET key value EX seconds writes the value and its expiry in one
	// command, so no entry ever exists without a TTL. A SET followed by an
	// EXPIRE would leave a gap in which a crash or a lost connection could
	// leave an entry that never expires. go-redis sends EX for a whole number
	// of seconds and PX (milliseconds) otherwise. For a TTL of 0 it sends
	// neither, and the key would live forever: hence the check above.
	return s.rdb.Set(ctx, key, value, s.ttl).Err()
}
