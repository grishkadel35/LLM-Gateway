package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// Request is what the gateway knows about a request body. ReadBody puts it
// on the request context; RequestFrom reads it back.
type Request struct {
	// Model is the model the client asked for, or "" if the body didn't say
	// (or wasn't JSON). Gemini names it in the URL instead of the body.
	Model string
	// Stream reports whether the client asked for a streamed response.
	Stream bool
}

type requestKey struct{}

// RequestFrom returns the Request ReadBody attached to ctx.
func RequestFrom(ctx context.Context) (*Request, bool) {
	req, ok := ctx.Value(requestKey{}).(*Request)
	return req, ok
}

// ReadBody returns middleware that is the single owner of the request body:
// the only code that reads it, and the only code that rewrites it.
//
// It reads the body once, up to maxBytes (413 request_too_large beyond
// that), and decodes it into map[string]json.RawMessage, so unknown fields
// and integers too large for a float64 pass through untouched. Every rewrite
// goes through here: for now, forcing stream_options.include_usage on for
// OpenAI-format streams, and from Week 3 the default output cap. When nothing
// is rewritten, the provider receives the client's exact bytes. A body that
// isn't JSON is forwarded unchanged for the provider to reject.
//
// It expects the provider-relative path, after StripPrefix: Gemini keeps the
// model and the stream flag in the URL.
func ReadBody(format provider.Format, maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
						"request body is larger than the gateway's limit of "+formatBytes(maxBytes))
					return
				}
				writeError(w, http.StatusBadRequest, "invalid_request", "could not read the request body")
				return
			}

			req := &Request{}
			var fields map[string]json.RawMessage
			if json.Unmarshal(body, &fields) == nil {
				req.Model, req.Stream = modelAndStream(format, r.URL.Path, fields)
				if format == provider.FormatOpenAI && req.Stream && includeUsage(fields) {
					if rewritten, err := json.Marshal(fields); err == nil {
						body = rewritten
					}
				}
			}

			setBody(r, body)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestKey{}, req)))
		})
	}
}

// modelAndStream reads the model and stream flag wherever the format keeps
// them. Wrong JSON types leave them at their zero values.
func modelAndStream(format provider.Format, path string, fields map[string]json.RawMessage) (model string, stream bool) {
	if format == provider.FormatGemini {
		// /v1beta/models/{model}:{action}; Metered has already checked the
		// shape.
		_, rest, _ := strings.Cut(path, "/models/")
		model, action, _ := strings.Cut(rest, ":")
		return model, action == "streamGenerateContent"
	}
	_ = json.Unmarshal(fields["model"], &model)
	_ = json.Unmarshal(fields["stream"], &stream)
	return model, stream
}

// includeUsage sets stream_options.include_usage to true, keeping any other
// stream options, and reports whether it changed anything. A stream_options
// that is neither an object nor null is left alone for the provider to
// reject.
func includeUsage(fields map[string]json.RawMessage) bool {
	opts := map[string]json.RawMessage{}
	if raw, ok := fields["stream_options"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &opts) != nil || opts == nil {
			return false
		}
	}
	if string(opts["include_usage"]) == "true" {
		return false
	}
	opts["include_usage"] = json.RawMessage("true")

	encoded, err := json.Marshal(opts)
	if err != nil {
		return false
	}
	fields["stream_options"] = encoded
	return true
}

// setBody replaces r's body with b, which can be read any number of times.
// ReverseProxy sends ContentLength as the Content-Length header, and GetBody
// lets the transport replay the body if it has to retry the request.
func setBody(r *http.Request, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

func formatBytes(n int64) string {
	const mib = 1 << 20
	if n >= mib && n%mib == 0 {
		return strconv.FormatInt(n/mib, 10) + " MiB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"type": errType, "message": message},
	})
}
