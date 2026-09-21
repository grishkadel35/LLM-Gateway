// Package proxy builds the transparent reverse proxy that forwards requests to
// an upstream LLM provider.
package proxy

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// New returns a reverse proxy that forwards everything it receives to p.
//
// It takes no error return: p.URL is already parsed and validated by the config
// package, which is the only thing that constructs a provider.Provider.
func New(p provider.Provider, logger *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director:       director(p),
		ModifyResponse: modifyResponse(p, logger),
		ErrorHandler:   errorHandler(p, logger),
		Transport:      transport(p.Timeout),
	}
}

// director rewrites each outbound request so it points at the upstream instead
// of at us. ReverseProxy calls it once per request, before sending.
//
// Go note: this function *returns a function*. Functions are ordinary values in
// Go, and the returned closure captures `target` and `logger` — the same idea as
// a Python closure or functools.partial. It's how we inject dependencies into a
// callback whose signature is fixed by the library.
func director(p provider.Provider) func(*http.Request) {
	target := p.URL

	return func(req *http.Request) {
		// Capture the host the client asked for before we overwrite it below.
		originalHost := req.Host

		// The client called us, so req.URL currently points at the gateway.
		// Swap in the upstream's scheme and host, and prefix any base path the
		// upstream URL carries (e.g. https://host/v1beta).
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path, req.URL.RawPath = joinURLPath(target, req.URL)

		// req.Host overrides the Host header that goes on the wire. Providers
		// route and TLS-verify on this, so it must be the upstream's host, not
		// "localhost:8080".
		req.Host = target.Host

		// Swap whatever credential the client sent for this provider's own key,
		// in the header shape this provider expects. Apply strips every known
		// credential header first, so a key meant for one provider can never
		// reach another.
		p.Apply(req.Header)

		// X-Forwarded-* headers tell the upstream who originally made the call.
		//
		// Important Go detail: when you set Director (as opposed to the newer
		// Rewrite field), ReverseProxy sets X-Forwarded-For *itself*, after the
		// Director runs. It appends the client IP to whatever is already there.
		// So if we called req.Header.Set("X-Forwarded-For", clientIP) here, the
		// upstream would receive the IP twice ("1.2.3.4, 1.2.3.4"). We therefore
		// let the standard library own X-Forwarded-For and add only the two
		// headers it does *not* set in Director mode.
		setIfEmpty(req.Header, "X-Forwarded-Host", originalHost)
		setIfEmpty(req.Header, "X-Forwarded-Proto", schemeOf(req))

		// Identify ourselves if the client didn't send a User-Agent. Without
		// this, Go's http client would send its own default, which makes
		// provider-side logs confusing.
		setIfEmpty(req.Header, "User-Agent", "llm-gateway")
	}
}

// modifyResponse runs after the upstream responds and before we copy the
// response back to the client.
//
// This is the hook where rate limiting, response caching, and token/usage
// accounting will plug in later. For now it just logs.
//
// Go note: returning a non-nil error here makes ReverseProxy call ErrorHandler
// instead of forwarding the response — that's how you'd reject or rewrite an
// upstream response later on.
func modifyResponse(p provider.Provider, logger *slog.Logger) func(*http.Response) error {
	return func(resp *http.Response) error {
		logger.Info("upstream response",
			"provider", p.Name,
			"status", resp.StatusCode,
			// ContentLength is -1 when the upstream uses chunked encoding,
			// which is what streaming (SSE) completions do.
			"content_length", resp.ContentLength,
			"content_type", resp.Header.Get("Content-Type"),
			"streaming", resp.ContentLength < 0,
		)
		return nil
	}
}

// errorHandler runs when we can't reach the upstream at all (DNS failure,
// connection refused, timeout). Without it, ReverseProxy logs to stderr and
// returns a bare 502 with an empty body; clients deserve JSON.
func errorHandler(p provider.Provider, logger *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Error("upstream request failed",
			"provider", p.Name,
			"method", r.Method,
			"path", r.URL.Path,
			"error", err,
		)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)

		// Go note: we ignore the encode error with `_ =` because the client has
		// likely gone away if this fails, and there's nothing useful left to do.
		// Go makes you write that out — an ignored error is always visible.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type":     "upstream_unavailable",
				"provider": p.Name,
				"message":  "the gateway could not reach the upstream provider",
			},
		})
	}
}

// transport controls how the outbound connection to the provider behaves.
//
// Note which timeout we use: ResponseHeaderTimeout, not a whole-request
// timeout. Streaming completions hold the response body open for minutes, so
// capping total request time would cut off long generations. What we actually
// want to catch is an upstream that never starts replying.
func transport(timeout time.Duration) http.RoundTripper {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
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

// schemeOf reports whether the *client* reached us over http or https.
func schemeOf(req *http.Request) string {
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

// joinURLPath joins the upstream's base path with the incoming request path,
// taking care not to produce a double slash or to lose one.
//
// Go note: multiple return values are normal here — no tuple type needed. Go
// also keeps an escaped copy of the path (RawPath) alongside the decoded one, so
// a path like /v1/models/gpt%2F4 survives the round trip unchanged.
func joinURLPath(target, req *url.URL) (path, rawPath string) {
	if target.RawPath == "" && req.RawPath == "" {
		return singleJoiningSlash(target.Path, req.Path), ""
	}

	targetPath, reqPath := target.EscapedPath(), req.EscapedPath()
	targetSlash := strings.HasSuffix(targetPath, "/")
	reqSlash := strings.HasPrefix(reqPath, "/")

	switch {
	case targetSlash && reqSlash:
		return target.Path + req.Path[1:], targetPath + reqPath[1:]
	case !targetSlash && !reqSlash:
		return target.Path + "/" + req.Path, targetPath + "/" + reqPath
	}
	return target.Path + req.Path, targetPath + reqPath
}

func singleJoiningSlash(a, b string) string {
	aSlash := strings.HasSuffix(a, "/")
	bSlash := strings.HasPrefix(b, "/")
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash:
		return a + "/" + b
	}
	return a + b
}
