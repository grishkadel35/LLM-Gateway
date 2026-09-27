// Package mockprovider fakes the OpenAI, Anthropic and Gemini APIs, streaming
// and non-streaming, so the gateway can be tested without spending money.
//
// Every response is deterministic: the same reply text and the same usage
// numbers on every call. Tests can assert exact token counts, and later exact
// costs, without depending on a real model.
//
// Only the response *shapes* are faithful. The quirks a usage parser must get
// right are reproduced on purpose and noted where they occur.
package mockprovider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Reply is the text every mock completion returns.
const Reply = "This is a deterministic reply from the llm-gateway mock provider."

// Usage reported for every completion. The providers disagree about whether
// cached tokens are part of the input count, and the mock reproduces that:
//
//   - OpenAI and Gemini report PromptTokens as the input count, with
//     CachedPromptTokens as a subset of it.
//   - Anthropic reports input_tokens *excluding* cache reads, so its
//     input_tokens is PromptTokens - CachedPromptTokens.
//
// Either way, a correct parser arrives at the same totals for all three.
const (
	PromptTokens       = 20
	CachedPromptTokens = 5
	// OutputTokens is one token per word of Reply; streams send one word per
	// chunk.
	OutputTokens = 10
)

// Handler serves all three providers' APIs from one server. Their paths don't
// collide, so the gateway can point every provider at the same address.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", openAIChat)
	mux.HandleFunc("POST /v1/messages", anthropicMessages)
	// Gemini puts the method after a colon in the last path segment:
	// /v1beta/models/gemini-x:generateContent. ServeMux wildcards match whole
	// segments, so the handler splits it.
	mux.HandleFunc("POST /v1beta/models/{modelAction}", gemini)
	return mux
}

// object keeps the nested JSON literals below readable.
type object = map[string]any

// words splits Reply into the chunks a stream sends, keeping the spaces so
// concatenating them rebuilds Reply exactly.
func words() []string {
	return strings.SplitAfter(Reply, " ")
}

// decodeBody reads a JSON request body into v, writing a 400 if it's malformed.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, object{
			"error": object{"type": "invalid_request_error", "message": "malformed JSON body: " + err.Error()},
		})
		return false
	}
	return true
}

// requireHeader writes a 401 and returns false if header is missing. It checks
// presence only: the point is to catch a gateway that presents a provider's
// key in the wrong header, not to validate keys.
func requireHeader(w http.ResponseWriter, r *http.Request, header string) bool {
	if r.Header.Get(header) != "" {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, object{
		"error": object{"type": "authentication_error", "message": "missing " + header + " header"},
	})
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// sseWriter writes server-sent events, flushing after each one so the client
// sees them arrive one at a time, as it would from a real provider.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startSSE(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

// event writes one event. OpenAI and Gemini send data-only events; Anthropic
// names each one, so name is optional.
func (s *sseWriter) event(name string, data any) {
	payload, ok := data.(string)
	if !ok {
		b, _ := json.Marshal(data)
		payload = string(b)
	}
	if name != "" {
		fmt.Fprintf(s.w, "event: %s\n", name)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", payload)
	_ = s.rc.Flush()
}
