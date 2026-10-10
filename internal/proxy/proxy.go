// Package proxy builds the transparent reverse proxy that forwards requests to
// an upstream LLM provider.
package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/grishkadel/llm-gateway/internal/apierror"
	"github.com/grishkadel/llm-gateway/internal/concurrency"
	"github.com/grishkadel/llm-gateway/internal/provider"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// New returns a reverse proxy that forwards everything it receives to p.
// onUsage, if not nil, receives each response's token usage once its body has
// been read (see usage.Meter).
//
// It takes no error return: p.URL is already parsed and validated by the config
// package, which is the only thing that constructs a provider.Provider.
func New(p provider.Provider, logger *slog.Logger, onUsage usage.Callback) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite:        rewrite(p),
		ModifyResponse: modifyResponse(p, logger, onUsage),
		ErrorHandler:   errorHandler(p, logger, onUsage),
		Transport:      transport(p.Timeout),
	}
}

// rewrite points each outbound request at the upstream instead of at us.
// ReverseProxy calls it once per request, before sending.
//
// It uses Rewrite rather than the older Director hook. With Director,
// ReverseProxy strips hop-by-hop headers *after* the hook runs, including any
// header the client names in "Connection:", so a client sending
// "Connection: X-Api-Key" could delete the key the gateway just set. Rewrite
// runs after that stripping, so what it sets is what goes out.
//
// Go note: this function *returns a function*. Functions are ordinary values in
// Go, and the returned closure captures `p` — the same idea as a Python closure
// or functools.partial. It's how we inject dependencies into a callback whose
// signature is fixed by the library.
func rewrite(p provider.Provider) func(*httputil.ProxyRequest) {
	return func(pr *httputil.ProxyRequest) {
		// SetURL swaps in the upstream's scheme and host, prefixes any base
		// path the upstream URL carries (e.g. https://host/openai), and makes
		// the Host header the upstream's. Providers route and TLS-verify on
		// that, so it must not be "localhost:8080".
		pr.SetURL(p.URL)

		// Swap whatever credential the client sent for this provider's own key,
		// in the header shape this provider expects. Apply strips every known
		// credential header and query parameter first, so a key meant for one
		// provider can never reach another.
		p.Apply(pr.Out)

		// Let the Transport negotiate compression itself. When the request
		// carries no Accept-Encoding, Go's Transport asks for gzip and
		// decompresses the reply transparently; when the client's header is
		// passed through, the body arrives still compressed. Usage metering
		// has to read the body, so it must be plain bytes.
		pr.Out.Header.Del("Accept-Encoding")

		// No X-Forwarded-* headers are sent. In Rewrite mode ReverseProxy
		// removes any the client supplied and adds none unless we call
		// pr.SetXForwarded(). Providers are third parties: they have no use
		// for our clients' IP addresses.

		// Identify ourselves if the client didn't send a User-Agent. Without
		// this, Go's http client would send its own default, which makes
		// provider-side logs confusing.
		setIfEmpty(pr.Out.Header, "User-Agent", "llm-gateway")
	}
}

// modifyResponse runs after the upstream responds and before we copy the
// response back to the client.
//
// It logs the response and wraps its body so usage is read as the bytes pass
// through to the client. Response caching will plug in here too.
//
// Go note: returning a non-nil error here makes ReverseProxy call ErrorHandler
// instead of forwarding the response — that's how you'd reject or rewrite an
// upstream response later on.
func modifyResponse(p provider.Provider, logger *slog.Logger, onUsage usage.Callback) func(*http.Response) error {
	return func(resp *http.Response) error {
		logger.Info("upstream response",
			"provider", p.Name,
			"status", resp.StatusCode,
			// ContentLength is -1 when the upstream uses chunked encoding.
			// That isn't the same as streaming: some providers (Groq) chunk
			// ordinary JSON replies too.
			"content_length", resp.ContentLength,
			"content_type", resp.Header.Get("Content-Type"),
			"streaming", usage.IsEventStream(resp),
		)
		if onUsage != nil {
			usage.Meter(resp, p.Format, onUsage)
		}

		// A 5xx with a fallback armed: the fallback answers instead. The body
		// is read here, through the meter, so this attempt's usage row is
		// complete; errFallback then makes ReverseProxy close it, which frees
		// any concurrency slot, and forward nothing.
		if a := armedFrom(resp.Request.Context()); a != nil && resp.StatusCode >= 500 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
			a.reason = reasonServerError
			return errFallback
		}
		return nil
	}
}

// StatusClientClosedRequest is recorded when the client disconnects before the
// upstream responds. There is no standard code for this; 499 is nginx's, and
// it keeps these requests distinguishable from real upstream failures in logs.
const StatusClientClosedRequest = 499

// errorHandler runs when we get no usable response from the upstream (DNS
// failure, connection refused, timeout, the client giving up first, or no
// concurrency slot free). Without it, ReverseProxy logs to stderr and returns
// a bare 502 with an empty body; clients deserve JSON.
//
// Each upstream failure is also reported to onUsage: the provider may have
// received the request and billed it, so it must leave a usage row, with
// unknown cost.
func errorHandler(p provider.Provider, logger *slog.Logger, onUsage usage.Callback) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		// modifyResponse handed the response to the fallback, metered.
		if errors.Is(err, errFallback) {
			return
		}

		// The request waited its whole queue timeout for a concurrency slot.
		// Nothing reached the provider, so there is no usage row. The queue
		// already did the waiting, so the client may retry at once. A busy
		// provider is working, not failing, so this never falls back.
		if errors.Is(err, concurrency.ErrBusy) {
			logger.Warn("provider busy: no concurrency slot within the queue timeout",
				"provider", p.Name,
				"method", r.Method,
				"path", r.URL.Path,
			)
			w.Header().Set("Retry-After", "1")
			apierror.Write(w, http.StatusServiceUnavailable, "provider_busy",
				"the provider is at its concurrency limit; retry shortly", map[string]any{"provider": p.Name})
			return
		}

		// The client went away, so nobody will read a response. It isn't an
		// upstream failure either, so it doesn't belong at ERROR level.
		if errors.Is(err, context.Canceled) {
			if onUsage != nil {
				onUsage(r.Context(), usage.Unanswered(r.Context(), StatusClientClosedRequest))
			}
			logger.Info("client closed request before upstream responded",
				"provider", p.Name,
				"method", r.Method,
				"path", r.URL.Path,
			)
			w.WriteHeader(StatusClientClosedRequest)
			return
		}

		status, errType, message, reason := http.StatusBadGateway, "upstream_unavailable", "the gateway could not reach the upstream provider", reasonUnreachable
		if isTimeout(err) {
			status, errType, message, reason = http.StatusGatewayTimeout, "upstream_timeout", "the upstream provider did not respond in time", reasonTimeout
		}

		if onUsage != nil {
			onUsage(r.Context(), usage.Unanswered(r.Context(), status))
		}

		// With a fallback armed, it answers instead: nothing is written here.
		// r is the outbound request, but its context is the inbound one.
		if a := armedFrom(r.Context()); a != nil {
			a.reason = reason
			logger.Warn("upstream request failed; falling back",
				"provider", p.Name,
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"error", err,
			)
			return
		}

		logger.Error("upstream request failed",
			"provider", p.Name,
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"error", err,
		)

		apierror.Write(w, status, errType, message, map[string]any{"provider": p.Name})
	}
}

// isTimeout reports whether err is a timeout: a dial, TLS handshake, or
// response-header deadline.
//
// Go note: errors.As walks the chain of wrapped errors looking for one that
// fits the target's type, here any error with a Timeout() method.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// Fixed bounds on connection setup. They are short and independent of the
// provider's timeout: a provider that is reachable at all connects in well
// under a second, even when its reply will take minutes.
const (
	dialTimeout         = 10 * time.Second
	tlsHandshakeTimeout = 10 * time.Second
)

// transport controls how the outbound connection to the provider behaves.
//
// Note which timeout we use: ResponseHeaderTimeout, not a whole-request
// timeout. Streaming completions hold the response body open for minutes, so
// capping total request time would cut off long generations. What we actually
// want to catch is an upstream that never starts replying. For a non-streaming
// request that is the whole generation, which is why the default is long.
func transport(timeout time.Duration) http.RoundTripper {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
}

// setIfEmpty sets a header only when the client didn't already provide it.
func setIfEmpty(h http.Header, key, value string) {
	if value != "" && h.Get(key) == "" {
		h.Set(key, value)
	}
}
