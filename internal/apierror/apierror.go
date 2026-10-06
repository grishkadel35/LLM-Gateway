// Package apierror writes the gateway's JSON error responses, in one shape
// everywhere: {"error": {"type": "...", "message": "...", ...extra fields}}.
package apierror

import (
	"encoding/json"
	"net/http"
)

// Write sends status with the error body. extra adds fields alongside type
// and message, such as the provider that failed; it may be nil.
func Write(w http.ResponseWriter, status int, errType, message string, extra map[string]any) {
	body := map[string]any{"type": errType, "message": message}
	for k, v := range extra {
		body[k] = v
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// Go note: we ignore the encode error with `_ =` because the client has
	// likely gone away if this fails, and there's nothing useful left to do.
	// Go makes you write that out — an ignored error is always visible.
	_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
}
