package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/grishkadel/llm-gateway/internal/apierror"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// Headers on every response from a route whose provider has a fallback.
const (
	// HeaderProvider names the provider that answered.
	HeaderProvider = "X-Gateway-Provider"
	// HeaderFallback is "true" when the fallback answered, "false" when the
	// route's own provider did.
	HeaderFallback = "X-Gateway-Fallback"
)

// Why a request fell back, as gateway_fallbacks_total's reason label says.
const (
	reasonUnreachable = "unreachable"
	reasonTimeout     = "timeout"
	reasonServerError = "server_error"
)

// maxDrain bounds how much of a 5xx body is read before the fallback takes
// over: enough for any error body, so its usage row is complete.
const maxDrain = 1 << 20

// errFallback is modifyResponse's error for a response the fallback replaces.
// ReverseProxy then closes the response and forwards none of it.
var errFallback = errors.New("response replaced by the fallback")

// armed is a request's fallback while the primary provider tries it: why the
// primary failed in a way the fallback fixes, or "" while it hasn't.
//
// It needs no lock. ReverseProxy calls ModifyResponse and ErrorHandler on the
// goroutine serving the request, before its ServeHTTP returns, and Fallback
// reads reason only after that.
type armed struct{ reason string }

type armedKey struct{}

// armedFrom returns the fallback armed on ctx, or nil.
func armedFrom(ctx context.Context) *armed {
	a, _ := ctx.Value(armedKey{}).(*armed)
	return a
}

// Fallback serves each request with primary, the proxy of the provider named
// from. When primary fails in a way another provider can fix (it is
// unreachable, it times out, or it answers 5xx), the request is served again
// by target, the proxy of the provider named to, with "model" swapped for its
// entry in models. The client gets target's response, whatever it is.
//
// Only a non-streaming request for a model in models can fall back; any other
// is served by primary exactly as without a fallback. A 4xx, a client that
// went away, or a busy primary with no concurrency slot free never falls
// back. Every response carries HeaderProvider and HeaderFallback, and
// onFallback hears of each fallback.
//
// Fallback goes inside ReadBody: it reads the request's model and stream flag
// from it, and replays the body it made replayable. target is a provider's
// plain proxy, so auth and ReadBody don't run twice.
func Fallback(from string, primary http.Handler, to string, target http.Handler, models map[string]string, onFallback func(from, to, reason string), logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pw := &providerWriter{ResponseWriter: w, provider: from, fallback: "false"}

		req, _ := usage.RequestFrom(r.Context())
		model, ok := "", false
		if req != nil && !req.Stream {
			model, ok = models[req.Model]
		}
		if !ok {
			primary.ServeHTTP(pw, r)
			return
		}

		// Armed, primary's proxy writes nothing when it fails in a way the
		// fallback fixes; it records why instead.
		a := &armed{}
		primary.ServeHTTP(pw, r.WithContext(context.WithValue(r.Context(), armedKey{}, a)))
		if a.reason == "" {
			return
		}

		// The retry starts from r, the request as it arrived: its context
		// isn't armed, so target's own failures reach the client like any
		// other.
		retry, err := usage.WithModel(r, model)
		if err != nil {
			// Unreachable while ReadBody owns the body: it decoded this one
			// to find the model.
			logger.Error("fallback: could not rewrite the request", "from", from, "to", to, "error", err)
			apierror.Write(pw, http.StatusBadGateway, "upstream_unavailable",
				"the gateway could not reach the upstream provider", map[string]any{"provider": from})
			return
		}

		logger.Warn("falling back",
			"from", from,
			"to", to,
			"reason", a.reason,
			"model", req.Model,
			"fallback_model", model,
		)
		onFallback(from, to, a.reason)
		pw.provider, pw.fallback = to, "true"
		target.ServeHTTP(pw, retry)
	})
}

// providerWriter sets HeaderProvider and HeaderFallback as each response's
// headers go out, 1xx included. Setting them once up front isn't enough:
// ReverseProxy clears the header map after forwarding an informational
// response, so the final one would go out without them. It works like
// middleware's requestIDWriter.
type providerWriter struct {
	http.ResponseWriter
	provider, fallback string
	wroteHeader        bool
}

func (w *providerWriter) WriteHeader(code int) {
	w.Header().Set(HeaderProvider, w.provider)
	w.Header().Set(HeaderFallback, w.fallback)
	if code >= 200 {
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *providerWriter) Write(b []byte) (int, error) {
	// Writing without WriteHeader implies a 200; the headers go out now.
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.NewResponseController reach Flush, as in middleware's
// responseRecorder.
func (w *providerWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
