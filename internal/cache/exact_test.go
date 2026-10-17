package cache

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/grishkadel/llm-gateway/internal/redistest"
)

// newStore returns a Store over the test Redis with the given ttl, that
// client, and a key no other test uses. The key is deleted when the test ends.
func newStore(t *testing.T, ttl time.Duration) (*Store, *redis.Client, string) {
	t.Helper()
	rdb := redistest.New(t)
	key := "cache:{tn_test_" + rand.Text() + "}:x"
	t.Cleanup(func() { rdb.Del(context.Background(), key) })
	return New(rdb, ttl), rdb, key
}

// TestSetThenGet: an entry comes back exactly, whatever bytes it holds.
func TestSetThenGet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry Entry
	}{
		{"text that JSON would escape", Entry{
			Status:      200,
			ContentType: "application/json; charset=utf-8",
			Model:       "gpt-5\nmini", // a newline in the header mustn't end it early
			Body:        []byte("{\"text\":\"a < b && c > d\",\"é\":\"日本語 😀\"}\n\nline two\n"),
		}},
		{"bytes that aren't text", Entry{Status: 200, Body: []byte{0, 0xff, '\n', 0xfe, 0x80, '\n'}}},
		{"a body that looks like an entry", Entry{Status: 200, Body: []byte("v1\n{\"status\":500}\nx")}},
		{"an empty body", Entry{Status: 204, Body: []byte{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, _, key := newStore(t, time.Minute)
			if err := s.Set(ctx, key, tc.entry); err != nil {
				t.Fatal(err)
			}
			got, ok, err := s.Get(ctx, key)
			if err != nil || !ok || !reflect.DeepEqual(got, tc.entry) {
				t.Errorf("Get = %+v, %v, %v; want %+v", got, ok, err, tc.entry)
			}
		})
	}
}

// TestStoredValueLayout pins the bytes Set writes. Gateways of different
// versions can share one Redis, so the layout can't change under a "v1" marker.
func TestStoredValueLayout(t *testing.T) {
	ctx := context.Background()
	s, rdb, key := newStore(t, time.Minute)
	e := Entry{Status: 200, ContentType: "text/plain", Model: "m", Body: []byte("a < b\n")}
	if err := s.Set(ctx, key, e); err != nil {
		t.Fatal(err)
	}
	got, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	want := "v1\n{\"status\":200,\"content_type\":\"text/plain\",\"model\":\"m\"}\na < b\n"
	if got != want {
		t.Errorf("stored value = %q, want %q", got, want)
	}
}

func TestGetMissingKey(t *testing.T) {
	s, _, key := newStore(t, time.Minute)
	got, ok, err := s.Get(context.Background(), key)
	if err != nil || ok || !reflect.DeepEqual(got, Entry{}) {
		t.Errorf("Get = %+v, %v, %v; want Entry{}, false, nil", got, ok, err)
	}
}

// TestMaxBodyIsStored: a body of MaxBody bytes is allowed. (One byte more
// isn't: see TestOversizeSetWritesNothing.)
func TestMaxBodyIsStored(t *testing.T) {
	ctx := context.Background()
	s, _, key := newStore(t, time.Minute)
	body := []byte(strings.Repeat("x", MaxBody))
	if err := s.Set(ctx, key, Entry{Status: 200, Body: body}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, key)
	if err != nil || !ok || !bytes.Equal(got.Body, body) {
		t.Errorf("Get = a body of %d bytes, %v, %v; want the %d bytes stored", len(got.Body), ok, err, len(body))
	}
}

// TestSetSetsTheTTL: Redis counts down from the Store's ttl, whether go-redis
// sends it in seconds (EX) or, for a fraction of a second, in milliseconds
// (PX).
func TestSetSetsTheTTL(t *testing.T) {
	for _, ttl := range []time.Duration{90 * time.Second, 2500 * time.Millisecond} {
		t.Run(ttl.String(), func(t *testing.T) {
			ctx := context.Background()
			s, rdb, key := newStore(t, ttl)
			if err := s.Set(ctx, key, Entry{Status: 200}); err != nil {
				t.Fatal(err)
			}
			got, err := rdb.PTTL(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			// Read a moment after the SET: within a second of the TTL.
			if got > ttl || got <= ttl-time.Second {
				t.Errorf("TTL = %v, want within a second of %v", got, ttl)
			}
		})
	}
}

// TestEntryExpires: a 1 s entry is gone once its second has passed. The test
// polls up to a deadline rather than sleeping once, as Redis's clock and the
// test's don't agree to the millisecond.
func TestEntryExpires(t *testing.T) {
	ctx := context.Background()
	s, _, key := newStore(t, time.Second)
	if err := s.Set(ctx, key, Entry{Status: 200, Body: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Get(ctx, key); err != nil || !ok {
		t.Fatalf("right after Set: Get = _, %v, %v; want the entry", ok, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, ok, err := s.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the entry is still there 5 s after Set, want it gone after its 1 s TTL")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestValueThatIsNotAnEntryIsAMiss: Get reads a value it doesn't know as a
// miss, not an error, and the next Set overwrites it.
func TestValueThatIsNotAnEntryIsAMiss(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"garbage", "garbage"},
		{"an empty value", ""},
		{"only the prefix", "v1\n"},
		{"a header with no newline after it", "v1\n{\"status\":200}"},
		{"a header that isn't JSON", "v1\n{bad json}\nbody"},
		{"a header of the wrong shape", "v1\n{\"status\":\"200\"}\nbody"},
		{"no version marker", "{\"status\":200}\nbody"},
		{"another version", "v2\n{\"status\":200}\nbody"},
		// Set stores only a 2xx, and a missing status would replay as 0.
		{"a header with no status", "v1\n{}\nbody"},
		{"a null header", "v1\nnull\nbody"},
		{"a status that isn't 2xx", "v1\n{\"status\":500}\nbody"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, rdb, key := newStore(t, time.Minute)
			if err := rdb.Set(ctx, key, tc.value, time.Minute).Err(); err != nil {
				t.Fatal(err)
			}
			if got, ok, err := s.Get(ctx, key); err != nil || ok || !reflect.DeepEqual(got, Entry{}) {
				t.Errorf("Get = %+v, %v, %v; want Entry{}, false, nil", got, ok, err)
			}

			want := Entry{Status: 200, ContentType: "text/plain", Model: "m", Body: []byte("fresh")}
			if err := s.Set(ctx, key, want); err != nil {
				t.Fatal(err)
			}
			if got, ok, err := s.Get(ctx, key); err != nil || !ok || !reflect.DeepEqual(got, want) {
				t.Errorf("after Set: Get = %+v, %v, %v; want %+v", got, ok, err, want)
			}
		})
	}
}

// TestSetRefusesWithoutReachingRedis uses Stores with no client: if a call
// reached Redis, the nil client would panic.
func TestSetRefusesWithoutReachingRedis(t *testing.T) {
	ctx := context.Background()
	if err := New(nil, time.Minute).Set(ctx, "k", Entry{Status: 200, Body: make([]byte, MaxBody+1)}); err == nil {
		t.Error("Set with a body over MaxBody: want an error")
	}
	for _, status := range []int{0, 199, 300, 500} {
		if err := New(nil, time.Minute).Set(ctx, "k", Entry{Status: status, Body: []byte("x")}); err == nil {
			t.Errorf("Set with status %d: want an error", status)
		}
	}
	// For 0, go-redis sends no expiry at all, and the entry would never go.
	for _, ttl := range []time.Duration{0, -time.Second, redis.KeepTTL} {
		if err := New(nil, ttl).Set(ctx, "k", Entry{Status: 200, Body: []byte("x")}); err == nil {
			t.Errorf("Set with a ttl of %v: want an error", ttl)
		}
	}
}

// TestOversizeSetWritesNothing: a refused Set leaves Redis as it was, with no
// key if there was none and the old entry if there was one.
func TestOversizeSetWritesNothing(t *testing.T) {
	ctx := context.Background()
	s, rdb, key := newStore(t, time.Minute)
	big := Entry{Status: 200, Body: make([]byte, MaxBody+1)}

	if err := s.Set(ctx, key, big); err == nil {
		t.Fatal("Set with a body over MaxBody: want an error")
	}
	if n := rdb.Exists(ctx, key).Val(); n != 0 {
		t.Fatal("the refused Set created its key")
	}

	old := Entry{Status: 200, Body: []byte("old")}
	if err := s.Set(ctx, key, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, key, big); err == nil {
		t.Fatal("Set with a body over MaxBody: want an error")
	}
	if got, ok, err := s.Get(ctx, key); err != nil || !ok || !reflect.DeepEqual(got, old) {
		t.Errorf("after the refused Set: Get = %+v, %v, %v; want the old entry, %+v", got, ok, err, old)
	}
}

// TestRedisErrorsAreReturned: a Redis failure is an error, not a miss. A
// context whose deadline has passed is one.
func TestRedisErrorsAreReturned(t *testing.T) {
	s, _, key := newStore(t, time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	if _, ok, err := s.Get(ctx, key); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Get = _, %v, %v; want context.DeadlineExceeded", ok, err)
	}
	if err := s.Set(ctx, key, Entry{Status: 200}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Set = %v; want context.DeadlineExceeded", err)
	}
}
