// Package mockprovider fakes the OpenAI, Groq, Anthropic and Gemini APIs, streaming
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
	"time"
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
//
// Gemini also reports thinking separately: ThoughtsTokens in
// thoughtsTokenCount, outside candidatesTokenCount, though it is billed as
// output. Its output total is therefore OutputTokens + ThoughtsTokens.
const (
	PromptTokens       = 20
	CachedPromptTokens = 5
	// OutputTokens is one token per word of Reply; streams send one word per
	// chunk.
	OutputTokens   = 10
	ThoughtsTokens = 7
)

// GeminiResponseID is the responseId every Gemini response carries, on every
// chunk of a stream too. The real API sends no request ID header, so this body
// field is the only ID Gemini gives for a request (verified live 2026-10-17).
const GeminiResponseID = "mock"

// Options tunes the handler. The zero value is what Handler serves.
type Options struct {
	// ChunkDelay is the pause before every event of a stream after the first.
	// Zero sends a stream back to back. Non-streaming answers never pause.
	ChunkDelay time.Duration
}

// Handler serves all the providers' APIs from one server, with streams sent
// back to back. Their paths don't collide, so the gateway can point every
// provider at the same address.
func Handler() http.Handler {
	return NewHandler(Options{})
}

// NewHandler is Handler with opts applied.
func NewHandler(opts Options) http.Handler {
	m := &mock{chunkDelay: opts.ChunkDelay}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", m.openAIChat)
	// Groq's real base URL ends in /openai, so this path also exercises the
	// gateway's base-path preservation.
	mux.HandleFunc("POST /openai/v1/chat/completions", m.groqChat)
	mux.HandleFunc("POST /v1/messages", m.anthropicMessages)
	// Gemini puts the method after a colon in the last path segment:
	// /v1beta/models/gemini-x:generateContent. ServeMux wildcards match whole
	// segments, so the handler splits it.
	mux.HandleFunc("POST /v1beta/models/{modelAction}", m.gemini)
	return mux
}

// mock carries the settings the handlers share.
type mock struct {
	chunkDelay time.Duration
}

// pause waits out the chunk delay before a stream's next event, and reports
// false if the client went away first. With no delay it never looks at the
// context, so a stream is written exactly as it would be without this.
func (m *mock) pause(r *http.Request) bool {
	if m.chunkDelay <= 0 {
		return true
	}
	t := time.NewTimer(m.chunkDelay)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
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
// sees them arrive one at a time, as it would from a real provider. Events
// after the first wait out the mock's chunk delay, and are dropped once the
// client is gone, so a handler's loop ends at once instead of sleeping on.
type sseWriter struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	r    *http.Request
	m    *mock
	sent bool
}

func (m *mock) startSSE(w http.ResponseWriter, r *http.Request) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return &sseWriter{w: w, rc: http.NewResponseController(w), r: r, m: m}
}

// event writes one event. OpenAI and Gemini send data-only events; Anthropic
// names each one, so name is optional.
func (s *sseWriter) event(name string, data any) {
	if s.sent && !s.m.pause(s.r) {
		return
	}
	s.sent = true
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
