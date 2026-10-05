// Package middleware holds HTTP middleware: handlers that wrap other handlers.
package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// Logging returns middleware that logs one structured JSON line per request.
//
// Go note: the shape `func(http.Handler) http.Handler` is the standard
// middleware signature. You take a handler and return a new one that does
// something extra before and after calling the original — Python's decorators,
// minus the @ syntax. Because it's just a function, you compose middleware by
// nesting calls: Logging(logger)(Recover(mux)).
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// Go note: http.HandlerFunc is an adapter. http.Handler is an interface
		// with one method, ServeHTTP. Rather than declaring a struct just to
		// hang that method on, we convert a plain function to the named type
		// http.HandlerFunc, which has a ServeHTTP method that calls the function
		// itself. That's how a function satisfies an interface in Go.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// http.ResponseWriter won't tell us what status was written, so we
			// wrap it in a recorder that remembers.
			rec := &responseRecorder{ResponseWriter: w}

			// Logged in a defer so the line is written even when next panics.
			// That is not hypothetical: when a client disconnects mid-stream,
			// ReverseProxy aborts with panic(http.ErrAbortHandler), which
			// net/http recovers silently. A plain call after ServeHTTP would
			// never run, and aborted streams would leave no trace.
			defer func() {
				attrs := []any{
					"request_id", RequestIDFrom(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"status", rec.status(),
					"duration_ms", float64(time.Since(start).Microseconds()) / 1000.0,
					"bytes", rec.bytes,
					"remote_addr", r.RemoteAddr,
				}
				// The client's own ID, if it sent one, lets an operator match
				// this line to the client's logs. It isn't the gateway's ID:
				// nothing makes it unique.
				if id := r.Header.Get(HeaderRequestID); id != "" {
					attrs = append(attrs, "client_request_id", id)
				}
				logger.Info("request", attrs...)
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

// responseRecorder wraps an http.ResponseWriter to capture the status code and
// the number of bytes written.
//
// Go note: embedding http.ResponseWriter (a field with no name) promotes all of
// its methods onto responseRecorder. That's composition, not inheritance: we get
// Header() and everything else for free, and we override only what we need.
type responseRecorder struct {
	http.ResponseWriter
	wroteHeader bool
	code        int
	bytes       int
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.code = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	// Writing without calling WriteHeader first implies a 200.
	if !r.wroteHeader {
		r.code = http.StatusOK
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// status reports the status code, defaulting to 200 for handlers that wrote
// nothing at all.
func (r *responseRecorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

// Unwrap lets http.NewResponseController reach the original ResponseWriter.
//
// This matters for us specifically: streaming LLM responses need Flush() to push
// each server-sent event to the client as it arrives. Our wrapper doesn't
// implement Flusher, and without Unwrap the reverse proxy would buffer the whole
// stream — a classic middleware bug. Since Go 1.20 the convention is to expose
// Unwrap and let ResponseController find the underlying capabilities.
func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
