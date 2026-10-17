package middleware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/cache"
	"github.com/grishkadel/llm-gateway/internal/provider"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// fakeCache stands in for the Redis cache. Each Get finds found (nothing when
// it is nil) or fails with getErr, and each Set fails with setErr. It records
// what it was asked.
type fakeCache struct {
	found          *cache.Entry
	getErr, setErr error

	gets []time.Duration // how long each Get had left
	sets []setCall
}

// setCall is one Set a fakeCache received.
type setCall struct {
	entry cache.Entry
	// timeout is how long it had left, and ctxErr its context's error when it
	// arrived: nil unless already done.
	timeout time.Duration
	ctxErr  error
}

func (f *fakeCache) Get(ctx context.Context, _ string) (cache.Entry, bool, error) {
	f.gets = append(f.gets, timeLeft(ctx))
	if f.getErr != nil || f.found == nil {
		return cache.Entry{}, false, f.getErr
	}
	return *f.found, true, nil
}

func (f *fakeCache) Set(ctx context.Context, _ string, e cache.Entry) error {
	f.sets = append(f.sets, setCall{e, timeLeft(ctx), ctx.Err()})
	return f.setErr
}

// timeLeft is how long ctx has until its deadline, or 0 if it has none.
func timeLeft(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline)
	}
	return 0
}

// discard is a logger for tests that don't read the logs.
var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// ignoreUsage is a usage callback for tests that don't read Cache's reports.
func ignoreUsage(context.Context, usage.Result) {}

// providerHeaders is what router has a hit send on a route with a fallback.
var providerHeaders = http.Header{"X-Gateway-Provider": {"ollama"}, "X-Gateway-Fallback": {"false"}}

// withCache is next behind Cache on route ollama, as router wires it: after
// RequestID, Auth (as acme) and ReadBody. A hit sends no headers of its own,
// and its usage report is dropped.
func withCache(store CacheStore, logger *slog.Logger, next http.HandlerFunc) http.Handler {
	return withCacheReporting(store, ignoreUsage, nil, logger, next)
}

// withCacheReporting is withCache with a hit's usage reported to onUsage, and
// hitHeader sent with it.
func withCacheReporting(store CacheStore, onUsage usage.Callback, hitHeader http.Header, logger *slog.Logger, next http.HandlerFunc) http.Handler {
	return RequestID(asTenant(acme, usage.ReadBody(provider.FormatOpenAI, 32<<20)(Cache(store, "ollama", onUsage, hitHeader, logger)(next))))
}

// cacheable is a chat body at temperature 0, which the cache takes.
const cacheable = `{"model":"m","temperature":0,"messages":[]}`

// chatPost is a chat request with body.
func chatPost(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
}

// completeUsage is a provider's usage for a whole response, not streamed.
var completeUsage = usage.Result{Usage: usage.Usage{Model: "m-snapshot"}, Status: http.StatusOK, Complete: true}

// answer is a handler that answers as provider's proxy does: status with a
// JSON body, then result as provider's usage.
func answer(provider string, status int, result usage.Result) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"ok":true}`)
		ReportUsage(r.Context(), provider, result)
	}
}

// TestCacheStoreRules: once a miss's handler returns, its response is stored
// only if the usage last reported says it is complete, not streamed, and from
// the route's own provider, and it is a 2xx without Content-Encoding. The
// entry is what the client got, with the model the usage named, and Get and
// Set each get cacheTimeout at most.
func TestCacheStoreRules(t *testing.T) {
	incomplete, streamed := completeUsage, completeUsage
	incomplete.Complete, streamed.Streamed = false, true
	failed := usage.Result{Status: http.StatusInternalServerError, Complete: true}

	cases := []struct {
		name   string
		next   http.HandlerFunc
		stored bool
	}{
		{"complete 2xx from the route's provider", answer("ollama", http.StatusCreated, completeUsage), true},
		// net/http ignores the second, and the client gets the 201.
		{"two statuses", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"ok":true}`)
			ReportUsage(r.Context(), "ollama", completeUsage)
		}, true},
		{"no usage reported", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }, false},
		// net/http sends a 200 for it, without the wrapper's knowing.
		{"nothing written", func(w http.ResponseWriter, r *http.Request) { ReportUsage(r.Context(), "ollama", completeUsage) }, false},
		{"incomplete", answer("ollama", http.StatusOK, incomplete), false},
		{"streamed", answer("ollama", http.StatusOK, streamed), false},
		{"not 2xx", answer("ollama", http.StatusInternalServerError, failed), false},
		// A fallback: the route's provider fails, and another answers.
		{"another provider's answer", func(w http.ResponseWriter, r *http.Request) {
			ReportUsage(r.Context(), "ollama", failed)
			answer("groq", http.StatusOK, completeUsage)(w, r)
		}, false},
		{"Content-Encoding", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "br")
			answer("ollama", http.StatusOK, completeUsage)(w, r)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeCache{}
			rec := httptest.NewRecorder()
			withCache(store, discard, tc.next).ServeHTTP(rec, chatPost(cacheable))

			if got := rec.Header().Get(HeaderCache); got != "miss" {
				t.Errorf("%s = %q, want miss", HeaderCache, got)
			}
			if len(store.gets) != 1 || store.gets[0] <= 0 || store.gets[0] > cacheTimeout {
				t.Errorf("Get calls had %v left, want one with a deadline within %v", store.gets, cacheTimeout)
			}
			if !tc.stored {
				if len(store.sets) != 0 {
					t.Errorf("Set calls = %+v, want none", store.sets)
				}
				return
			}
			want := cache.Entry{Status: http.StatusCreated, ContentType: "application/json", Model: "m-snapshot", Body: []byte(`{"ok":true}`)}
			if len(store.sets) != 1 || !reflect.DeepEqual(store.sets[0].entry, want) {
				t.Fatalf("Set calls = %+v, want one storing %+v", store.sets, want)
			}
			if s := store.sets[0]; s.timeout <= 0 || s.timeout > cacheTimeout || s.ctxErr != nil {
				t.Errorf("Set had %v left, context error %v; want a live deadline within %v", s.timeout, s.ctxErr, cacheTimeout)
			}
		})
	}
}

// TestCacheCopyLimit: a miss's body is copied up to cache.MaxBody. One byte
// more and the copy is dropped, so nothing is stored, but the client still
// gets every byte.
func TestCacheCopyLimit(t *testing.T) {
	cases := []struct {
		name   string
		size   int
		stored bool
	}{
		{"exactly cache.MaxBody", cache.MaxBody, true},
		{"a byte over", cache.MaxBody + 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Repeat([]byte("a"), tc.size)
			store := &fakeCache{}
			rec := httptest.NewRecorder()
			withCache(store, discard, func(w http.ResponseWriter, r *http.Request) {
				// In two writes, as ReverseProxy passes a body on.
				_, _ = w.Write(body[:tc.size/2])
				_, _ = w.Write(body[tc.size/2:])
				ReportUsage(r.Context(), "ollama", completeUsage)
			}).ServeHTTP(rec, chatPost(cacheable))

			if !bytes.Equal(rec.Body.Bytes(), body) {
				t.Errorf("the client got %d bytes, want all %d", rec.Body.Len(), tc.size)
			}
			if tc.stored != (len(store.sets) == 1) {
				t.Fatalf("%d Set calls, want stored = %v", len(store.sets), tc.stored)
			}
			if tc.stored && !bytes.Equal(store.sets[0].entry.Body, body) {
				t.Errorf("stored %d bytes, want the %d the client got", len(store.sets[0].entry.Body), tc.size)
			}
		})
	}
}

// brokenPipe is a ResponseWriter whose client has gone: every Write fails.
type brokenPipe struct{ *httptest.ResponseRecorder }

func (brokenPipe) Write([]byte) (int, error) { return 0, errors.New("write: broken pipe") }

// TestCacheFailedWrite: after a Write fails, the copy isn't what the client
// got, so nothing is stored, even when the handler returns normally.
func TestCacheFailedWrite(t *testing.T) {
	store := &fakeCache{}
	withCache(store, discard, answer("ollama", http.StatusOK, completeUsage)).ServeHTTP(brokenPipe{httptest.NewRecorder()}, chatPost(cacheable))

	if len(store.sets) != 0 {
		t.Errorf("Set calls = %+v, want none", store.sets)
	}
}

// headerLog is a ResponseWriter that logs each status sent, with the
// HeaderCache values it went out with. Unlike httptest.ResponseRecorder, it
// sees every 1xx.
type headerLog struct {
	*httptest.ResponseRecorder
	sent []string
}

func (l *headerLog) WriteHeader(code int) {
	l.sent = append(l.sent, fmt.Sprint(code, " ", l.Header().Values(HeaderCache)))
	// The recorder keeps the first status it is given, and refuses a body
	// after a 1xx, so it gets only the final one.
	if code >= 200 {
		l.ResponseRecorder.WriteHeader(code)
	}
}

// TestCacheHeaderAcrossA1xx: HeaderCache goes out with every status, 1xx
// included, in place of the upstream's own, and the store keeps the final
// status, not the 1xx.
func TestCacheHeaderAcrossA1xx(t *testing.T) {
	store := &fakeCache{}
	sent := &headerLog{ResponseRecorder: httptest.NewRecorder()}
	withCache(store, discard, func(w http.ResponseWriter, r *http.Request) {
		// As ReverseProxy forwards an upstream's 103 and then its 200, each
		// with the upstream's own X-Gateway-Cache: it clears the header map
		// after the 1xx.
		//
		// Go note: clear (built in since Go 1.21) deletes every entry of a
		// map.
		w.Header().Add(HeaderCache, "hit")
		w.WriteHeader(http.StatusEarlyHints)
		clear(w.Header())
		w.Header().Add(HeaderCache, "hit")
		answer("ollama", http.StatusOK, completeUsage)(w, r)
	}).ServeHTTP(sent, chatPost(cacheable))

	if want := []string{"103 [miss]", "200 [miss]"}; !slices.Equal(sent.sent, want) {
		t.Errorf("sent %q, want %q", sent.sent, want)
	}
	if len(store.sets) != 1 || store.sets[0].entry.Status != http.StatusOK {
		t.Errorf("Set calls = %+v, want one storing status 200", store.sets)
	}
}

// TestCacheFlushes: streamed responses still flush through the writer, on a
// miss and on a bypass.
func TestCacheFlushes(t *testing.T) {
	cases := []struct{ name, body string }{
		{"miss", cacheable},
		{"bypass", `{"model":"m","messages":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			withCache(&fakeCache{}, discard, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "data: chunk\n\n")
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Errorf("Flush() through Cache failed: %v", err)
				}
			}).ServeHTTP(rec, chatPost(tc.body))

			if !rec.Flushed || rec.Header().Get(HeaderCache) != tc.name {
				t.Errorf("flushed %v, %s %q; want flushed, %s", rec.Flushed, HeaderCache, rec.Header().Get(HeaderCache), tc.name)
			}
		})
	}
}

// TestCacheNoStoreAfterAbort: when ReverseProxy aborts a response with
// panic(http.ErrAbortHandler), nothing is stored, whatever was reported, and
// the panic goes on up to net/http.
func TestCacheNoStoreAfterAbort(t *testing.T) {
	store := &fakeCache{}
	h := withCache(store, discard, func(w http.ResponseWriter, r *http.Request) {
		answer("ollama", http.StatusOK, completeUsage)(w, r)
		panic(http.ErrAbortHandler)
	})

	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", p)
		}
		if len(store.sets) != 0 {
			t.Errorf("Set calls = %+v, want none", store.sets)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), chatPost(cacheable))
	t.Error("ServeHTTP returned; want the abort to go on up")
}

// TestCacheHit: a hit replays the entry's status, Content-Type (none if it has
// none) and body, with the hit headers Cache was given (none if nil), and
// nothing after Cache runs. Once the body is written, the hit reports one
// Result on the request's context: cached and complete, with the entry's
// status and model and no tokens.
func TestCacheHit(t *testing.T) {
	cases := []struct {
		name            string
		entry           cache.Entry
		hitHeader       http.Header
		wantContentType []string
	}{
		{"with a Content-Type", cache.Entry{Status: http.StatusCreated, ContentType: "application/json", Model: "m-snapshot", Body: []byte(`{"cached":true}`)}, nil, []string{"application/json"}},
		{"without one", cache.Entry{Status: http.StatusOK, Model: "m-snapshot", Body: []byte("plain")}, nil, nil},
		{"with hit headers", cache.Entry{Status: http.StatusOK, ContentType: "application/json", Model: "m-snapshot", Body: []byte(`{"cached":true}`)}, providerHeaders, []string{"application/json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeCache{found: &tc.entry}
			reached := false
			rec := httptest.NewRecorder()
			var reported []usage.Result
			var ids []string
			var written []int // the body bytes the client had at each report
			onUsage := func(ctx context.Context, r usage.Result) {
				reported = append(reported, r)
				ids = append(ids, RequestIDFrom(ctx))
				written = append(written, rec.Body.Len())
			}
			withCacheReporting(store, onUsage, tc.hitHeader, discard, func(http.ResponseWriter, *http.Request) { reached = true }).ServeHTTP(rec, chatPost(cacheable))

			res := rec.Result()
			if reached || len(store.sets) != 0 {
				t.Errorf("reached = %v, Set calls = %+v; want a hit that goes no further and stores nothing", reached, store.sets)
			}
			if res.StatusCode != tc.entry.Status || res.Header.Get(HeaderCache) != "hit" || rec.Body.String() != string(tc.entry.Body) {
				t.Errorf("status %d, %s %q, body %q; want %d, hit, %q",
					res.StatusCode, HeaderCache, res.Header.Get(HeaderCache), rec.Body, tc.entry.Status, tc.entry.Body)
			}
			if got := res.Header.Values("Content-Type"); !slices.Equal(got, tc.wantContentType) {
				t.Errorf("Content-Type = %q, want %q", got, tc.wantContentType)
			}
			for name := range providerHeaders {
				if got, want := res.Header.Values(name), tc.hitHeader.Values(name); !slices.Equal(got, want) {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}

			want := usage.Result{Usage: usage.Usage{Model: "m-snapshot"}, Status: tc.entry.Status, Complete: true, Cached: true}
			if len(reported) != 1 || reported[0] != want {
				t.Fatalf("reported %+v, want one %+v", reported, want)
			}
			if id := res.Header.Get(HeaderRequestID); ids[0] != id || written[0] != len(tc.entry.Body) {
				t.Errorf("reported with request ID %q after %d body bytes, want %q after all %d", ids[0], written[0], id, len(tc.entry.Body))
			}
		})
	}
}

// TestCacheReportsOnlyHits: a miss, after a lookup that worked or one that
// failed, and a bypass report nothing to Cache's usage callback, since the
// proxy's meter reports them, and send none of a hit's headers.
func TestCacheReportsOnlyHits(t *testing.T) {
	cases := []struct {
		name  string
		store *fakeCache
		body  string
		want  string
	}{
		{"miss", &fakeCache{}, cacheable, "miss"},
		{"miss after a failed lookup", &fakeCache{getErr: errors.New("redis: i/o timeout")}, cacheable, "miss"},
		{"bypass", &fakeCache{}, `{"model":"m","messages":[]}`, "bypass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reported []usage.Result
			onUsage := func(_ context.Context, r usage.Result) { reported = append(reported, r) }
			rec := httptest.NewRecorder()
			withCacheReporting(tc.store, onUsage, providerHeaders, discard, answer("ollama", http.StatusOK, completeUsage)).ServeHTTP(rec, chatPost(tc.body))

			if got := rec.Header().Get(HeaderCache); got != tc.want || len(reported) != 0 {
				t.Errorf("%s %q, reported %+v; want %s, nothing reported", HeaderCache, got, reported, tc.want)
			}
			for name := range providerHeaders {
				if got := rec.Header().Values(name); got != nil {
					t.Errorf("%s = %q, want none", name, got)
				}
			}
		})
	}
}

// TestCacheStoresWhenTheClientHasGone: a client that leaves once it has the
// whole response cancels the request's context. The response is still
// stored, under a context that isn't cancelled.
func TestCacheStoresWhenTheClientHasGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeCache{}
	withCache(store, discard, func(w http.ResponseWriter, r *http.Request) {
		answer("ollama", http.StatusOK, completeUsage)(w, r)
		cancel()
	}).ServeHTTP(httptest.NewRecorder(), chatPost(cacheable).WithContext(ctx))

	if len(store.sets) != 1 || store.sets[0].ctxErr != nil {
		t.Errorf("Set calls = %+v, want one with a live context", store.sets)
	}
}

// TestCacheSkipsALargeBody: a temperature-0 body over cache.MaxRequestBody is
// a bypass, and it is never copied only for cache.Key to refuse it.
func TestCacheSkipsALargeBody(t *testing.T) {
	large := `{"model":"m","temperature":0,"messages":[],"pad":"` + strings.Repeat("a", cache.MaxRequestBody) + `"}`
	cases := []struct {
		name, body, want string
		copies           int
	}{
		{"within cache.MaxRequestBody", cacheable, "miss", 1},
		{"over it", large, "bypass", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copies := 0
			// Between ReadBody and Cache, it counts the copies made of the
			// replayable body.
			counting := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					getBody := r.GetBody
					r.GetBody = func() (io.ReadCloser, error) {
						copies++
						return getBody()
					}
					next.ServeHTTP(w, r)
				})
			}
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
			h := asTenant(acme, usage.ReadBody(provider.FormatOpenAI, 32<<20)(counting(Cache(&fakeCache{}, "ollama", ignoreUsage, nil, discard)(next))))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, chatPost(tc.body))

			if got := rec.Header().Get(HeaderCache); got != tc.want || copies != tc.copies {
				t.Errorf("%s %q after %d copies of the body, want %s after %d", HeaderCache, got, copies, tc.want, tc.copies)
			}
		})
	}
}

// TestCacheWithoutTenant: a request with no tenant on its context means the
// routes are wired wrong. It is logged as an error and served uncached,
// without a lookup.
func TestCacheWithoutTenant(t *testing.T) {
	var logs bytes.Buffer
	store := &fakeCache{}
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	h := usage.ReadBody(provider.FormatOpenAI, 1<<20)(Cache(store, "ollama", ignoreUsage, nil, slog.New(slog.NewJSONHandler(&logs, nil)))(next))
	h.ServeHTTP(httptest.NewRecorder(), chatPost(cacheable))

	if !reached || len(store.gets) != 0 {
		t.Errorf("reached = %v, Get calls = %d; want served with no lookup", reached, len(store.gets))
	}
	if entry := logLine(t, &logs); entry["level"] != "ERROR" {
		t.Errorf("log line = %v, want ERROR", entry)
	}
}
