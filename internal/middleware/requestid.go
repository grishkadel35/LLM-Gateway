package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// HeaderRequestID carries the gateway's request ID on every response.
const HeaderRequestID = "X-Request-ID"

type requestIDKey struct{}

// RequestIDFrom returns the ID RequestID gave this request, or "" for a
// request that never passed through it.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// RequestID gives every request a fresh ID, puts it on the request context and
// returns it to the client in X-Request-ID. It goes outermost, so even
// requests rejected before reaching a provider carry one.
//
// A client-sent X-Request-ID is never used as the gateway's ID: nothing makes
// it unique. Logging records it separately.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		// Also set up front: a handler that writes nothing gets its implicit
		// 200 from net/http, which never calls the wrapper below.
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(&requestIDWriter{ResponseWriter: w, id: id}, r.WithContext(ctx))
	})
}

// requestIDWriter sets X-Request-ID again as the response headers go out.
// ReverseProxy *adds* the upstream's headers to the response, and OpenAI and
// Groq send an x-request-id of their own: without this, the client would
// receive two IDs.
type requestIDWriter struct {
	http.ResponseWriter
	id          string
	wroteHeader bool
}

func (w *requestIDWriter) WriteHeader(code int) {
	// Set before every call, 1xx included: ReverseProxy clears the header map
	// after forwarding an informational response, so the final one needs the
	// ID set again.
	w.Header().Set(HeaderRequestID, w.id)
	if code >= 200 {
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *requestIDWriter) Write(b []byte) (int, error) {
	// Writing without WriteHeader implies a 200; the headers go out now.
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.NewResponseController reach Flush, as in responseRecorder.
func (w *requestIDWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// newRequestID returns "req_" plus 16 random bytes in hex.
func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}
