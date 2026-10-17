package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/cache"
	"github.com/grishkadel/llm-gateway/internal/config"
	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
	"github.com/grishkadel/llm-gateway/internal/metrics"
	"github.com/grishkadel/llm-gateway/internal/middleware"
	"github.com/grishkadel/llm-gateway/internal/mockprovider"
	"github.com/grishkadel/llm-gateway/internal/pricing"
	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// fakeCache stands in for the Redis cache, so routing tests need no Redis. It
// keeps entries in a map, fails every Get with getErr and every Set with
// setErr when they are set, and records the calls. Behind an httptest.Server
// it is called from the server's goroutines, hence the lock.
type fakeCache struct {
	getErr, setErr error

	mu      sync.Mutex
	entries map[string]cache.Entry
	gets    int
	sets    []cache.Entry
}

func (f *fakeCache) Get(_ context.Context, key string) (cache.Entry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return cache.Entry{}, false, f.getErr
	}
	e, ok := f.entries[key]
	return e, ok, nil
}

func (f *fakeCache) Set(_ context.Context, key string, e cache.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, e)
	if f.setErr != nil {
		return f.setErr
	}
	if f.entries == nil {
		f.entries = map[string]cache.Entry{}
	}
	f.entries[key] = e
	return nil
}

// recorded returns how many Gets the cache got, and each entry Set got.
func (f *fakeCache) recorded() (gets int, sets []cache.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, slices.Clone(f.sets)
}

// countingUpstream starts a provider that answers every request with answer,
// and returns its URL and the count of requests it got.
func countingUpstream(t *testing.T, answer http.HandlerFunc) (string, *atomic.Int64) {
	t.Helper()
	calls := new(atomic.Int64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, calls
}

// cacheConfig is a config file with two routes, openai and gemini, both to
// the URL it is formatted with.
const cacheConfig = `
providers:
  openai:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: bearer
    format: openai
  gemini:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: x-goog-api-key
    format: gemini
`

// cachedRouter builds the gateway from cacheConfig in front of upstream, over
// fakeStore, with responses cached in store, rate limited by limiter unless
// it is nil.
func cachedRouter(t *testing.T, upstream string, store middleware.CacheStore, limiter middleware.Limiter, logger *slog.Logger) http.Handler {
	t.Helper()
	t.Setenv("TEST_MOCK_KEY", "mock")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(cacheConfig, upstream)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load() returned error: %v", err)
	}
	r, err := router(cfg, fakeStore{}, testAdminKey, nil, limiter, store, metrics.New(), nil, logger)
	if err != nil {
		t.Fatalf("router() returned error: %v", err)
	}
	return r
}

// discardLogger is for tests that don't read the logs.
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// zeroTemperature is a chat body at temperature 0, which the cache takes.
const zeroTemperature = `{"model":"gpt-4o","temperature":0,"messages":[{"role":"user","content":"hi"}]}`

// tenantPost is a tenant's POST of body to path.
func tenantPost(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testTenantKey)
	return req
}

// openaiChat is a tenant's chat request on route openai.
func openaiChat(body string) *http.Request {
	return tenantPost("/openai/v1/chat/completions", body)
}

// serve sends req through r and returns the response.
func serve(r http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// verdict is the X-Gateway-Cache the response went out with, its values
// joined by commas: "" for none.
func verdict(rec *httptest.ResponseRecorder) string {
	return strings.Join(rec.Result().Header.Values(middleware.HeaderCache), ",")
}

// TestCacheHitAndMiss: of two identical temperature-0 requests, only the
// first reaches the provider and is charged. The second is a hit, with the
// first's status, Content-Type and body and a request ID of its own, and so
// is the same request with its fields reordered and its whitespace changed.
// A hit is served even while the limiter would refuse the request.
func TestCacheHitAndMiss(t *testing.T) {
	upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
	store := &fakeCache{}
	limiter := &fakeLimiter{allowed: true}
	r := cachedRouter(t, upstream, store, limiter, discardLogger)

	miss := serve(r, openaiChat(zeroTemperature))
	if miss.Code != http.StatusOK || verdict(miss) != "miss" {
		t.Fatalf("first request: status %d, X-Gateway-Cache %q; want 200, miss; body %s", miss.Code, verdict(miss), miss.Body)
	}
	for _, body := range []string{
		zeroTemperature,
		// Reordered at every level, and spaced out.
		`{ "messages": [ {"content": "hi", "role": "user"} ],
			"temperature": 0, "model": "gpt-4o" }`,
	} {
		hit := serve(r, openaiChat(body))
		if verdict(hit) != "hit" || hit.Code != miss.Code || hit.Header().Get("Content-Type") != miss.Header().Get("Content-Type") ||
			hit.Body.String() != miss.Body.String() {
			t.Errorf("%s: X-Gateway-Cache %q, status %d, Content-Type %q, body %s; want hit, %d, %q, %s", body, verdict(hit),
				hit.Code, hit.Header().Get("Content-Type"), hit.Body, miss.Code, miss.Header().Get("Content-Type"), miss.Body)
		}
		if id := hit.Header().Get("X-Request-ID"); !strings.HasPrefix(id, "req_") || id == miss.Header().Get("X-Request-ID") {
			t.Errorf("%s: X-Request-ID = %q, want one of the hit's own", body, id)
		}
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("the provider got %d requests, want 1", n)
	}
	if allows, _ := limiter.recorded(); len(allows) != 1 {
		t.Errorf("Allow calls = %+v, want one, for the miss", allows)
	}
	want := cache.Entry{Status: http.StatusOK, ContentType: "application/json", Model: "gpt-4o", Body: miss.Body.Bytes()}
	if _, sets := store.recorded(); len(sets) != 1 || !reflect.DeepEqual(sets[0], want) {
		t.Errorf("Set calls = %+v, want one: %+v", sets, want)
	}

	t.Run("while the limiter would refuse", func(t *testing.T) {
		refusing := &fakeLimiter{wait: time.Minute}
		r := cachedRouter(t, upstream, store, refusing, discardLogger)

		if hit := serve(r, openaiChat(zeroTemperature)); hit.Code != http.StatusOK || verdict(hit) != "hit" {
			t.Errorf("status %d, X-Gateway-Cache %q; want 200, hit", hit.Code, verdict(hit))
		}
		// A request that isn't cached meets the limiter, and is refused.
		if other := serve(r, openaiChat(`{"model":"gpt-4o","temperature":0,"messages":[]}`)); other.Code != http.StatusTooManyRequests {
			t.Errorf("an uncached request: status %d, want 429", other.Code)
		}
		if allows, _ := refusing.recorded(); len(allows) != 1 {
			t.Errorf("Allow calls = %+v, want one, for the uncached request", allows)
		}
	})
}

// TestCacheBypass: a stream, a request at another temperature without "on",
// one that sends "off" in any case, and one with a body over
// cache.MaxRequestBody all go straight on. Each gets bypass, reaches the
// provider every time, and has nothing looked up or stored.
func TestCacheBypass(t *testing.T) {
	large := `{"model":"gpt-4o","temperature":0,"messages":[{"role":"user","content":"` + strings.Repeat("a", cache.MaxRequestBody) + `"}]}`
	const chat = "/openai/v1/chat/completions"
	cases := []struct{ name, path, header, body string }{
		{"stream: true", chat, "", `{"model":"gpt-4o","stream":true,"temperature":0,"messages":[]}`},
		{"Gemini streamGenerateContent", "/gemini/v1beta/models/gemini-3.8-flash:streamGenerateContent", "", `{"generationConfig":{"temperature":0},"contents":[]}`},
		{"temperature 0.7 without the header", chat, "", `{"model":"gpt-4o","temperature":0.7,"messages":[]}`},
		{"off", chat, "off", zeroTemperature},
		{"OFF", chat, "OFF", zeroTemperature},
		{"a temperature-0 body over cache.MaxRequestBody", chat, "", large},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
			store := &fakeCache{}
			r := cachedRouter(t, upstream, store, nil, discardLogger)

			for range 2 {
				req := tenantPost(tc.path, tc.body)
				if tc.header != "" {
					req.Header.Set(middleware.HeaderCache, tc.header)
				}
				if rec := serve(r, req); rec.Code != http.StatusOK || verdict(rec) != "bypass" {
					t.Errorf("status %d, X-Gateway-Cache %q; want 200, bypass", rec.Code, verdict(rec))
				}
			}
			if n := calls.Load(); n != 2 {
				t.Errorf("the provider got %d requests, want 2", n)
			}
			if gets, sets := store.recorded(); gets != 0 || len(sets) != 0 {
				t.Errorf("%d Get calls, Set calls %+v; want none of either", gets, sets)
			}
		})
	}
}

// TestCacheOnAtAnyTemperature: X-Gateway-Cache: on, in any case, makes a
// request at temperature 0.7 eligible. It misses, then hits.
func TestCacheOnAtAnyTemperature(t *testing.T) {
	for _, value := range []string{"on", "On"} {
		t.Run(value, func(t *testing.T) {
			upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
			r := cachedRouter(t, upstream, &fakeCache{}, nil, discardLogger)

			var got []string
			for range 2 {
				req := openaiChat(`{"model":"gpt-4o","temperature":0.7,"messages":[]}`)
				req.Header.Set(middleware.HeaderCache, value)
				got = append(got, verdict(serve(r, req)))
			}
			if !slices.Equal(got, []string{"miss", "hit"}) || calls.Load() != 1 {
				t.Errorf("X-Gateway-Cache %q with %d requests at the provider, want miss then hit, with 1", got, calls.Load())
			}
		})
	}
}

// TestCacheRefusesABadHeader: an X-Gateway-Cache that isn't on or off, sent
// once, gets 400 invalid_request with no X-Gateway-Cache of its own, and
// reaches nothing: no lookup, no limiter, no provider.
func TestCacheRefusesABadHeader(t *testing.T) {
	cases := []struct {
		name   string
		values []string
	}{
		{"yes", []string{"yes"}},
		{"empty", []string{""}},
		{"two values on one line", []string{"on, off"}},
		{"two lines", []string{"on", "on"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
			store := &fakeCache{}
			limiter := &fakeLimiter{allowed: true}
			r := cachedRouter(t, upstream, store, limiter, discardLogger)

			req := openaiChat(zeroTemperature)
			for _, v := range tc.values {
				req.Header.Add(middleware.HeaderCache, v)
			}
			rec := serve(r, req)

			var body struct {
				Error struct{ Type string } `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusBadRequest || body.Error.Type != "invalid_request" {
				t.Errorf("status %d, body %s; want 400 invalid_request", rec.Code, rec.Body)
			}
			if verdict(rec) != "" {
				t.Errorf("X-Gateway-Cache = %q, want none", verdict(rec))
			}
			gets, _ := store.recorded()
			allows, _ := limiter.recorded()
			if calls.Load() != 0 || gets != 0 || len(allows) != 0 {
				t.Errorf("%d requests at the provider, %d Get calls, %d Allow calls; want none", calls.Load(), gets, len(allows))
			}
		})
	}
}

// cutShort answers 200 with a Content-Length it never fills. The server then
// closes the connection, so the body ends early.
func cutShort(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", "1000")
	_, _ = io.WriteString(w, `{"model":"gpt-4o","choices":[`)
}

// TestCacheNeverStores: a response that mustn't be kept is a miss that
// leaves nothing stored, so the next identical request reaches the provider
// again.
func TestCacheNeverStores(t *testing.T) {
	large := `{"model":"gpt-4o","pad":"` + strings.Repeat("a", cache.MaxBody) + `"}`
	cases := []struct {
		name   string
		answer http.HandlerFunc
		// bodyLen, if not 0, is the length of body the client must get.
		bodyLen int
	}{
		{"an upstream 500", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"the model failed"}}`)
		}, 0},
		// Behind a ResponseRecorder, ReverseProxy doesn't abort but returns
		// normally, so only the incomplete usage stops the store. For the
		// abort, behind a real server, see TestCacheNeverStoresABodyCutShort.
		{"a 2xx body cut short", cutShort, 0},
		{"an event-stream answer", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n")
		}, 0},
		{"a body over cache.MaxBody", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, large)
		}, len(large)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := countingUpstream(t, tc.answer)
			store := &fakeCache{}
			r := cachedRouter(t, upstream, store, nil, discardLogger)

			for range 2 {
				rec := serve(r, openaiChat(zeroTemperature))
				if verdict(rec) != "miss" {
					t.Errorf("X-Gateway-Cache = %q, want miss", verdict(rec))
				}
				if tc.bodyLen != 0 && rec.Body.Len() != tc.bodyLen {
					t.Errorf("the client got %d bytes, want all %d", rec.Body.Len(), tc.bodyLen)
				}
			}
			if n := calls.Load(); n != 2 {
				t.Errorf("the provider got %d requests, want 2", n)
			}
			if _, sets := store.recorded(); len(sets) != 0 {
				t.Errorf("Set calls = %+v, want none", sets)
			}
		})
	}
}

// TestCacheNeverStoresARateLimitedRequest: the limiter's 429 is a miss that
// isn't stored. Once the limiter admits the same request, it reaches the
// provider.
func TestCacheNeverStoresARateLimitedRequest(t *testing.T) {
	upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
	store := &fakeCache{}
	limiter := &fakeLimiter{wait: time.Second}
	r := cachedRouter(t, upstream, store, limiter, discardLogger)

	if rec := serve(r, openaiChat(zeroTemperature)); rec.Code != http.StatusTooManyRequests || verdict(rec) != "miss" {
		t.Errorf("status %d, X-Gateway-Cache %q; want 429, miss", rec.Code, verdict(rec))
	}
	if _, sets := store.recorded(); len(sets) != 0 {
		t.Errorf("Set calls = %+v, want none", sets)
	}

	limiter.mu.Lock()
	limiter.allowed = true
	limiter.mu.Unlock()
	if rec := serve(r, openaiChat(zeroTemperature)); rec.Code != http.StatusOK || verdict(rec) != "miss" || calls.Load() != 1 {
		t.Errorf("once admitted: status %d, X-Gateway-Cache %q, %d requests at the provider; want 200, miss, 1",
			rec.Code, verdict(rec), calls.Load())
	}
}

// TestCacheNeverStoresABodyCutShort: behind a real server, as in production,
// ReverseProxy aborts a 2xx body the upstream cuts short with
// panic(http.ErrAbortHandler), and the server hangs up on the client. The
// store never runs, and the same request reaches the provider again.
func TestCacheNeverStoresABodyCutShort(t *testing.T) {
	upstream, calls := countingUpstream(t, cutShort)
	store := &fakeCache{}
	r := cachedRouter(t, upstream, store, nil, discardLogger)
	// done receives a value as the gateway is through with each request,
	// abort included.
	done := make(chan struct{}, 2)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() { done <- struct{}{} }()
		r.ServeHTTP(w, req)
	}))
	t.Cleanup(gw.Close)

	for range 2 {
		req, err := http.NewRequest(http.MethodPost, gw.URL+"/openai/v1/chat/completions", strings.NewReader(zeroTemperature))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testTenantKey)
		// The abort loses whatever the server still held, which for a body
		// this small is the whole response, headers included.
		resp, err := gw.Client().Do(req)
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		if err == nil {
			t.Error("the response arrived whole, want it cut short")
		}
		receive(t, done, "the gateway to finish the request")
	}

	if n := calls.Load(); n != 2 {
		t.Errorf("the provider got %d requests, want 2", n)
	}
	if _, sets := store.recorded(); len(sets) != 0 {
		t.Errorf("Set calls = %+v, want none", sets)
	}
}

// TestCacheFailsOpen: a failing cache never fails a request. A failed lookup
// is a Warn and a miss: the provider answers, and nothing is stored after
// it. A failed store is a Warn, and the client gets the provider's response
// as it was.
func TestCacheFailsOpen(t *testing.T) {
	cases := []struct {
		name     string
		store    *fakeCache
		wantWarn string
		wantSets int
	}{
		{"Get fails", &fakeCache{getErr: errors.New("redis: i/o timeout")}, "cache lookup failed; request not cached", 0},
		{"Set fails", &fakeCache{setErr: errors.New("redis: i/o timeout")}, "cache store failed", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
			var logs lockedBuffer
			logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			r := cachedRouter(t, upstream, tc.store, nil, logger)

			rec := serve(r, openaiChat(zeroTemperature))
			if rec.Code != http.StatusOK || verdict(rec) != "miss" || !strings.Contains(rec.Body.String(), mockprovider.Reply) || calls.Load() != 1 {
				t.Errorf("status %d, X-Gateway-Cache %q, %d requests at the provider, body %s; want 200, miss, 1, the mock's reply",
					rec.Code, verdict(rec), calls.Load(), rec.Body)
			}
			_, sets := tc.store.recorded()
			if len(sets) != tc.wantSets || tc.wantSets == 1 && !bytes.Equal(sets[0].Body, rec.Body.Bytes()) {
				t.Errorf("Set calls = %+v, want %d, given the response the client got", sets, tc.wantSets)
			}

			var entry map[string]any
			if err := json.Unmarshal([]byte(logs.String()), &entry); err != nil {
				t.Fatalf("logs %q, want one JSON line: %v", logs.String(), err)
			}
			if entry["level"] != "WARN" || entry["msg"] != tc.wantWarn || entry["request_id"] != rec.Header().Get("X-Request-ID") ||
				entry["tenant_id"] != "tn_test" || entry["error"] != "redis: i/o timeout" {
				t.Errorf("log line = %v, want a WARN %q with the request ID, tenant and error", entry, tc.wantWarn)
			}
		})
	}
}

// TestCacheHeaderSurvivesA1xx: X-Gateway-Cache goes out on the final response
// in place of the upstream's own, even after a 1xx, after which ReverseProxy
// clears the response headers. The 1xx doesn't upset the store either: the
// next identical request is a hit. It runs over a real server and client:
// httptest.ResponseRecorder keeps the first status it is given, so it can't
// show what a 1xx does.
func TestCacheHeaderSurvivesA1xx(t *testing.T) {
	upstream, calls := countingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// The upstream's own X-Gateway-Cache, on the 103 and on the 200.
		w.Header().Set(middleware.HeaderCache, "hit")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	})
	gw := httptest.NewServer(cachedRouter(t, upstream, &fakeCache{}, nil, discardLogger))
	t.Cleanup(gw.Close)

	cases := []struct {
		name, body string
		want       []string
	}{
		{"miss", zeroTemperature, []string{"miss"}},
		{"bypass", `{"model":"gpt-4o","temperature":0.7,"messages":[]}`, []string{"bypass"}},
		{"hit", zeroTemperature, []string{"hit"}},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodPost, gw.URL+"/openai/v1/chat/completions", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testTenantKey)
		res, err := gw.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		// Read to the end, so the gateway is through with the request.
		_, _ = io.ReadAll(res.Body)
		res.Body.Close()

		if got := res.Header.Values(middleware.HeaderCache); res.StatusCode != http.StatusOK || !slices.Equal(got, tc.want) {
			t.Errorf("%s: status %d, X-Gateway-Cache %q; want 200, %q", tc.name, res.StatusCode, got, tc.want)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the provider got %d requests, want 2: the miss and the bypass", n)
	}
}

// TestCacheStreamStillFlushes: a stream through a gateway with a cache
// bypasses it, and its events reach the client as they come, while the
// upstream holds the stream open.
func TestCacheStreamStillFlushes(t *testing.T) {
	store := &fakeCache{}
	r := cachedRouter(t, sseUpstream(t, 1), store, nil, discardLogger)
	// done closes when the gateway is through with the request, abort
	// included.
	done := make(chan struct{})
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer close(done)
		r.ServeHTTP(w, req)
	}))
	t.Cleanup(gw.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/openai/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"temperature":0,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testTenantKey)

	// The first event is all the upstream sends before it waits, so it
	// arrives only if each layer flushed it. A goroutine reads it, so that a
	// stream stuck in a buffer fails the test instead of hanging it.
	type event struct {
		verdict []string
		line    string
		err     error
	}
	first := make(chan event, 1)
	go func() {
		resp, err := gw.Client().Do(req)
		if err != nil {
			first <- event{err: err}
			return
		}
		defer resp.Body.Close()
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		first <- event{resp.Header.Values(middleware.HeaderCache), line, err}
	}()
	got := receive(t, first, "the first event while the upstream holds the stream open")
	if got.err != nil || !strings.HasPrefix(got.line, "data: ") || !slices.Equal(got.verdict, []string{"bypass"}) {
		t.Errorf("first line %q, error %v, X-Gateway-Cache %q; want a data line, bypass", got.line, got.err, got.verdict)
	}

	cancel()
	receive(t, done, "the gateway to finish the request")
	if gets, sets := store.recorded(); gets != 0 || len(sets) != 0 {
		t.Errorf("%d Get calls, Set calls %+v; want none of either", gets, sets)
	}
}

// TestCacheHitUsageRow: of the same temperature-0 request twice, against
// Postgres, the miss leaves its usual row, priced and not cached, and the hit
// a row of its own: its own request ID and key, the route's provider, the
// stored model and status, no tokens, cost 0, cached, not streamed, and no
// provider request ID. The hit adds nothing to gateway_tokens_total, and the
// usage report counts both requests at the miss's cost.
func TestCacheHitUsageRow(t *testing.T) {
	conn := dbtest.New(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatalf("Migrate() returned error: %v", err)
	}
	store := stores{tenant.NewStore(conn), usage.NewReports(conn)}
	tn, key, err := store.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	writer := usage.NewWriter(conn, discardLogger)
	r, err := router(mockConfig(t), store, testAdminKey, recordUsage(writer, nil, discardLogger), nil, &fakeCache{}, metrics.New(), nil, discardLogger)
	if err != nil {
		t.Fatal(err)
	}

	// send sends the request as acme, and returns the X-Request-ID it got.
	send := func(want string) string {
		t.Helper()
		req := openaiChat(zeroTemperature)
		req.Header.Set("Authorization", "Bearer "+key.Plaintext)
		rec := serve(r, req)
		if rec.Code != http.StatusOK || verdict(rec) != want {
			t.Fatalf("status %d, X-Gateway-Cache %q; want 200, %s; body %s", rec.Code, verdict(rec), want, rec.Body)
		}
		return rec.Header().Get("X-Request-ID")
	}
	// tokens returns the gateway_tokens_total samples on /metrics.
	tokens := func() []string {
		var samples []string
		for line := range strings.Lines(scrapeMetrics(t, r)) {
			if strings.HasPrefix(line, "gateway_tokens_total{") {
				samples = append(samples, line)
			}
		}
		return samples
	}

	missID := send("miss")
	before := tokens()
	hitID := send("hit")
	if after := tokens(); len(before) == 0 || !slices.Equal(after, before) {
		t.Errorf("gateway_tokens_total went from %q to %q across the hit; want the miss's tokens, unchanged", before, after)
	}
	if err := writer.Close(ctx); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM usage_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 || hitID == missID {
		t.Fatalf("usage_logs has %d rows, for request IDs %s and %s; want 2, under two IDs", n, missID, hitID)
	}

	// row is the usage_logs columns the test reads.
	type row struct {
		TenantID, Provider, Model, Endpoint    string
		KeyID                                  int64
		Status                                 int
		Input, CachedInput, CacheWrite, Output int64
		Cost                                   sql.NullInt64
		Cached, Streamed                       bool
		ProviderRequestID                      sql.NullString
	}
	read := func(id string) row {
		t.Helper()
		var got row
		err := conn.QueryRow(`
			SELECT tenant_id, provider, model, endpoint, api_key_id, status, input_tokens, cached_input_tokens,
			       cache_write_tokens, output_tokens, cost_micros, cached, streamed, provider_request_id
			FROM usage_logs WHERE request_id = $1`, id,
		).Scan(&got.TenantID, &got.Provider, &got.Model, &got.Endpoint, &got.KeyID, &got.Status, &got.Input,
			&got.CachedInput, &got.CacheWrite, &got.Output, &got.Cost, &got.Cached, &got.Streamed, &got.ProviderRequestID)
		if err != nil {
			t.Fatalf("no row with request ID %s: %v", id, err)
		}
		return got
	}

	// The mock's usage: 15 input tokens, 5 cached and 10 output.
	wantCost, ok := pricing.Cost("openai", usage.Usage{Model: "gpt-4o", Input: 15, CachedInput: 5, Output: 10}, "", time.Now())
	if !ok || wantCost <= 0 {
		t.Fatal("openai/gpt-4o should be priced")
	}
	want := row{TenantID: tn.ID, Provider: "openai", Model: "gpt-4o", Endpoint: "/v1/chat/completions", KeyID: key.ID,
		Status: http.StatusOK, Input: 15, CachedInput: 5, Output: 10, Cost: sql.NullInt64{Int64: wantCost, Valid: true}}
	if got := read(missID); got != want {
		t.Errorf("the miss's row = %+v, want %+v", got, want)
	}
	// The hit's is the same, but for no tokens, cost 0 and cached.
	want.Input, want.CachedInput, want.Output, want.Cost, want.Cached = 0, 0, 0, sql.NullInt64{Valid: true}, true
	if got := read(hitID); got != want {
		t.Errorf("the hit's row = %+v, want %+v", got, want)
	}

	// The usage report counts both requests, at the miss's cost.
	req := httptest.NewRequest(http.MethodGet, "/admin/tenants/"+tn.ID+"/usage", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminKey)
	rec := serve(r, req)
	var report struct {
		Total usage.Totals `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &report); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("usage report: status %d, body %s", rec.Code, rec.Body)
	}
	if report.Total.Requests != 2 || report.Total.CostMicros != wantCost || report.Total.UnpricedRequests != 0 {
		t.Errorf("report total = %+v, want 2 requests, %d micro-$, none unpriced", report.Total, wantCost)
	}
}

// fallbackConfig is a config file with two providers, at the URLs it is
// formatted with: ollama, which falls back to groq for qwen3.5:9b, and groq.
const fallbackConfig = `
providers:
  ollama:
    url: %s
    auth: none
    format: openai
    fallback:
      enabled: true
      provider: groq
      models:
        "qwen3.5:9b": qwen/qwen3.8-27b
  groq:
    url: %s/openai
    key: ${TEST_GROQ_KEY}
    auth: bearer
    format: openai
`

// fallbackRouter builds the gateway from fallbackConfig over fakeStore, with
// responses cached in store.
func fallbackRouter(t *testing.T, ollama, groq string, store middleware.CacheStore) http.Handler {
	t.Helper()
	t.Setenv("TEST_GROQ_KEY", "sk-groq")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(fallbackConfig, ollama, groq)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load() returned error: %v", err)
	}
	r, err := router(cfg, fakeStore{}, testAdminKey, nil, nil, store, metrics.New(), nil, discardLogger)
	if err != nil {
		t.Fatalf("router() returned error: %v", err)
	}
	return r
}

// qwenAtZero is a chat body at temperature 0 for the model ollama's fallback
// maps.
const qwenAtZero = `{"model":"qwen3.5:9b","temperature":0,"messages":[{"role":"user","content":"ping"}]}`

// TestCacheNeverStoresAFallbacksAnswer: on a route with a fallback, when the
// route's provider fails and the fallback answers, the client gets the
// fallback's answer, but nothing is stored: the key names the route's
// provider. So the same request is a miss again, and goes to the route's
// provider again, as gateway_fallbacks_total shows.
func TestCacheNeverStoresAFallbacksAnswer(t *testing.T) {
	failing := newFakeOllama(t, ollamaKnobs{status: http.StatusInternalServerError})
	cases := []struct{ name, ollama, reason string }{
		{"ollama answers 500", failing.server.URL, "server_error"},
		// Port 1 on loopback: nothing listens there.
		{"ollama is unreachable", "http://127.0.0.1:1", "unreachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groq, groqCalls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
			store := &fakeCache{}
			r := fallbackRouter(t, tc.ollama, groq, store)

			for range 2 {
				rec := serve(r, tenantPost("/ollama/v1/chat/completions", qwenAtZero))
				if rec.Code != http.StatusOK || verdict(rec) != "miss" || rec.Header().Get("X-Gateway-Provider") != "groq" ||
					rec.Header().Get("X-Gateway-Fallback") != "true" {
					t.Errorf("status %d, X-Gateway-Cache %q, X-Gateway-Provider %q, X-Gateway-Fallback %q; want 200, miss, groq, true",
						rec.Code, verdict(rec), rec.Header().Get("X-Gateway-Provider"), rec.Header().Get("X-Gateway-Fallback"))
				}
			}
			if _, sets := store.recorded(); len(sets) != 0 {
				t.Errorf("Set calls = %+v, want none", sets)
			}
			fallbacks := fmt.Sprintf(`gateway_fallbacks_total{from="ollama",reason=%q,to="groq"} 2`, tc.reason)
			if !strings.Contains(scrapeMetrics(t, r), fallbacks+"\n") || groqCalls.Load() != 2 {
				t.Errorf("groq got %d requests; want 2, each after ollama failed, with /metrics showing %s", groqCalls.Load(), fallbacks)
			}
		})
	}
}

// TestCacheHitProviderHeaders: on a route with a fallback, an answer from the
// route's own provider is stored, and the hit on it carries the headers every
// other response on the route does, naming the route's provider. A hit on a
// route without a fallback carries neither, like every other response there.
// Each provider sees only its route's miss.
func TestCacheHitProviderHeaders(t *testing.T) {
	ollama := newFakeOllama(t, ollamaKnobs{})
	groq, groqCalls := countingUpstream(t, mockprovider.Handler().ServeHTTP)
	r := fallbackRouter(t, ollama.server.URL, groq, &fakeCache{})

	cases := []struct {
		route, body        string
		provider, fallback []string // the two headers' values: nil for none
	}{
		{"ollama", qwenAtZero, []string{"ollama"}, []string{"false"}},
		{"groq", zeroTemperature, nil, nil},
	}
	for _, tc := range cases {
		for _, want := range []string{"miss", "hit"} {
			rec := serve(r, tenantPost("/"+tc.route+"/v1/chat/completions", tc.body))
			h := rec.Result().Header
			if rec.Code != http.StatusOK || verdict(rec) != want || !slices.Equal(h.Values("X-Gateway-Provider"), tc.provider) ||
				!slices.Equal(h.Values("X-Gateway-Fallback"), tc.fallback) {
				t.Errorf("%s: status %d, X-Gateway-Cache %q, X-Gateway-Provider %q, X-Gateway-Fallback %q; want 200, %s, %q, %q",
					tc.route, rec.Code, verdict(rec), h.Values("X-Gateway-Provider"), h.Values("X-Gateway-Fallback"), want, tc.provider, tc.fallback)
			}
		}
	}
	if n, m := len(ollama.arrivals), groqCalls.Load(); n != 1 || m != 1 {
		t.Errorf("ollama got %d requests and groq %d, want 1 each", n, m)
	}
}
