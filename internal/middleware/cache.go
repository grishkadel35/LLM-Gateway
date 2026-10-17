package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/grishkadel/llm-gateway/internal/apierror"
	"github.com/grishkadel/llm-gateway/internal/cache"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// HeaderCache is the response cache's header. A client may send it as on or
// off (see Cache), and every response that reaches the cache carries it as
// hit, miss or bypass.
const HeaderCache = "X-Gateway-Cache"

// CacheStore keeps cached responses. *cache.Store satisfies it; tests
// substitute a fake.
type CacheStore interface {
	// Get returns the entry stored under key, and false when there is none.
	Get(ctx context.Context, key string) (cache.Entry, bool, error)
	// Set stores e under key, replacing any entry there.
	Set(ctx context.Context, key string, e cache.Entry) error
}

// cacheTimeout bounds each call to the CacheStore, Get and Set alike. Like
// allowTimeout, it treats a Redis that takes 100 ms as down.
const cacheTimeout = 100 * time.Millisecond

// pending is a cache miss on its way through the provider. It rides on the
// request context as a pointer, so the request's usage can be reported onto
// it (ReportUsage), and once the request is over Cache decides by that usage
// whether to store the response. It needs no lock, for the reason charge
// gives.
type pending struct {
	// provider is the provider that reported result, the usage last reported
	// for the request; result is nil if none was.
	provider string
	result   *usage.Result
}

type pendingKey struct{}

// pendingFrom returns the miss Cache put on ctx, or nil if it put none.
func pendingFrom(ctx context.Context) *pending {
	p, _ := ctx.Value(pendingKey{}).(*pending)
	return p
}

// Cache returns middleware that answers repeated requests on provider's route
// from store, without calling the provider.
//
// A request is eligible when it isn't a stream, and it either sends
// X-Gateway-Cache: on or sets temperature to exactly 0 without sending
// X-Gateway-Cache: off. Any other value of that header, or a second one, gets
// a 400 invalid_request before anything else happens. An eligible request is
// looked up under its cache.Key, unless Key refuses it (a body over
// cache.MaxRequestBody, for one):
//   - A hit replays the stored response. Nothing after Cache runs, so neither
//     the rate limit nor the provider sees the request.
//   - A miss goes on, and its response is copied as it goes to the client.
//     Once the handler returns, the copy is stored if the usage reported for
//     the request (ReportUsage) says it is complete, not streamed, and from
//     provider itself, and it is a 2xx of at most cache.MaxBody bytes with no
//     Content-Encoding (see storable).
//
// Any other request is a bypass: it goes on, with no lookup and no store.
// HeaderCache tells the client which of the three each response was.
//
// The cache fails open. When a lookup fails (Redis is down, or slower than
// cacheTimeout), the request is a miss, logged with a Warn, and its response
// isn't stored. A store that fails is a Warn too, and changes nothing the
// client gets.
//
// It needs the tenant from Auth and the body from ReadBody, so it runs after
// both. It goes outside RateLimit, so a hit is never charged.
func Cache(store CacheStore, provider string, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mode, ok := cacheMode(r.Header)
			if !ok {
				apierror.Write(w, http.StatusBadRequest, "invalid_request",
					"X-Gateway-Cache must be sent once, as on or off", nil)
				return
			}

			ctx := r.Context()
			id := RequestIDFrom(ctx)
			t, _, authed := TenantFrom(ctx)
			req, bodyRead := usage.RequestFrom(ctx)
			if !authed || !bodyRead {
				// Auth and ReadBody run before every provider route, so this
				// is a wiring bug. The request still goes ahead.
				logger.Error("cache with no tenant or request; request not cached", "request_id", id)
				next.ServeHTTP(w, r)
				return
			}

			// The cheap checks first: the key decodes the whole body.
			eligible := mode != "off" && !req.Stream && (mode == "on" || req.ZeroTemperature)
			var key string
			if eligible {
				key, eligible = cacheKey(t.ID, provider, r)
			}
			if !eligible {
				next.ServeHTTP(newCacheWriter(w, "bypass", false), r)
				return
			}

			lookupCtx, cancel := context.WithTimeout(ctx, cacheTimeout)
			e, found, err := store.Get(lookupCtx, key)
			cancel()
			switch {
			case err != nil:
				// A miss that is never stored: with Redis failing, the store
				// would most likely fail too, after another cacheTimeout.
				logger.Warn("cache lookup failed; request not cached", "request_id", id, "tenant_id", t.ID, "error", err)
				next.ServeHTTP(newCacheWriter(w, "miss", false), r)
				return
			case found:
				if e.ContentType != "" {
					w.Header().Set("Content-Type", e.ContentType)
				}
				w.Header().Set(HeaderCache, "hit")
				w.WriteHeader(e.Status)
				_, _ = w.Write(e.Body)
				return
			}

			p := &pending{}
			cw := newCacheWriter(w, "miss", true)
			next.ServeHTTP(cw, r.WithContext(context.WithValue(ctx, pendingKey{}, p)))

			// A plain statement, never a defer: when ReverseProxy aborts a
			// response cut short with panic(http.ErrAbortHandler), this never
			// runs, and nothing is stored.
			if !cw.storable(p, provider) {
				return
			}
			// WithoutCancel, as in settle: a client that already has the
			// whole response may have gone, and the response is still good.
			setCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheTimeout)
			err = store.Set(setCtx, key, cache.Entry{Status: cw.status, ContentType: cw.contentType, Model: p.result.Model, Body: cw.body})
			cancel()
			if err != nil {
				logger.Warn("cache store failed", "request_id", id, "tenant_id", t.ID, "error", err)
			}
		})
	}
}

// cacheMode reads X-Gateway-Cache from h: "on" or "off" in any case, or ""
// when it is absent. ok is false for any other value, an empty one included,
// and for the header sent more than once.
func cacheMode(h http.Header) (mode string, ok bool) {
	values := h.Values(HeaderCache)
	switch {
	case len(values) == 0:
		return "", true
	case len(values) > 1:
		return "", false
	case strings.EqualFold(values[0], "on"):
		return "on", true
	case strings.EqualFold(values[0], "off"):
		return "off", true
	}
	return "", false
}

// cacheKey returns r's cache.Key over the body the proxy will forward, or
// ok == false when r can't be cached. ReadBody left that body replayable,
// with an exact ContentLength, so a body over cache.MaxRequestBody is refused
// here rather than copied only for Key to refuse it.
func cacheKey(tenantID, provider string, r *http.Request) (key string, ok bool) {
	if r.ContentLength > cache.MaxRequestBody {
		return "", false
	}
	body, err := r.GetBody()
	if err != nil {
		return "", false
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return "", false
	}
	return cache.Key(tenantID, provider, r, b)
}

// cacheWriter sets HeaderCache as the response headers go out, and on a miss
// copies the response as it goes to the client, for Cache to store.
//
// The header is set before every WriteHeader, 1xx included, as in
// requestIDWriter: ReverseProxy clears the header map after forwarding an
// informational response, and adds the upstream's headers to it, so the
// final response needs it set again, in place of any the upstream sent.
type cacheWriter struct {
	http.ResponseWriter
	value string // miss or bypass

	// copying is true while the body is copied into body: from the start of
	// a miss whose lookup worked, until the body goes past cache.MaxBody or a
	// Write fails. The copy is then dropped. The client gets every byte
	// either way.
	copying bool
	body    []byte

	wroteHeader bool
	// status is the first final status, and contentType and contentEncoding
	// are the headers it went out with.
	status                       int
	contentType, contentEncoding string
}

// newCacheWriter wraps w to send value in HeaderCache, copying the response if
// copying is set. It sets the header at once too: a handler that writes
// nothing gets its implicit 200 from net/http, which never calls the wrapper.
func newCacheWriter(w http.ResponseWriter, value string, copying bool) *cacheWriter {
	w.Header().Set(HeaderCache, value)
	return &cacheWriter{ResponseWriter: w, value: value, copying: copying}
}

func (w *cacheWriter) WriteHeader(code int) {
	w.Header().Set(HeaderCache, w.value)
	if code >= 200 && !w.wroteHeader {
		w.wroteHeader = true
		w.status = code
		w.contentType = w.Header().Get("Content-Type")
		w.contentEncoding = w.Header().Get("Content-Encoding")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cacheWriter) Write(b []byte) (int, error) {
	// Writing without WriteHeader implies a 200; the headers go out now.
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	if w.copying {
		if err != nil || len(w.body)+n > cache.MaxBody {
			w.copying, w.body = false, nil
		} else {
			w.body = append(w.body, b[:n]...)
		}
	}
	return n, err
}

// Unwrap lets http.NewResponseController reach Flush, as in responseRecorder.
func (w *cacheWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// storable reports whether the response w copied may be stored. p holds the
// usage last reported for the request, and provider is the route's.
func (w *cacheWriter) storable(p *pending, provider string) bool {
	r := p.result
	return w.copying && // within cache.MaxBody, and every Write went through
		r != nil && r.Complete && // read to the end
		!r.Streamed && // neither an event stream nor a stream in another shape
		p.provider == provider && // not a fallback's: the key names provider and its model
		w.status >= 200 && w.status <= 299 &&
		// An entry can't replay this header, and the meter calls a body it
		// can't decode complete.
		w.contentEncoding == ""
}
