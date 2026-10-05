package mockprovider

import (
	"encoding/json"
	"net/http"
	"strings"
)

// gemini serves POST /v1beta/models/{model}:generateContent and
// :streamGenerateContent.
func gemini(w http.ResponseWriter, r *http.Request) {
	model, action, _ := strings.Cut(r.PathValue("modelAction"), ":")
	if action != "generateContent" && action != "streamGenerateContent" {
		writeJSON(w, http.StatusNotFound, object{
			"error": object{"type": "not_found", "message": "unknown method " + action},
		})
		return
	}
	if !requireHeader(w, r, "X-Goog-Api-Key") {
		return
	}

	// The model comes from the path, so the body is only checked for being
	// valid JSON.
	var req struct{}
	if !decodeBody(w, r, &req) {
		return
	}

	chunk := func(text string, outputSoFar int, finished bool) object {
		candidate := object{
			"content": object{"parts": []object{{"text": text}}, "role": "model"},
			"index":   0,
		}
		if finished {
			candidate["finishReason"] = "STOP"
		}
		// Thinking finishes before the reply starts, so thoughtsTokenCount is
		// already final on the first chunk. It is not part of
		// candidatesTokenCount, but totalTokenCount includes it, as it does in
		// the real API (verified live 2026-10-04).
		return object{
			"candidates": []object{candidate},
			"usageMetadata": object{
				"promptTokenCount":        PromptTokens,
				"cachedContentTokenCount": CachedPromptTokens,
				"candidatesTokenCount":    outputSoFar,
				"thoughtsTokenCount":      ThoughtsTokens,
				"totalTokenCount":         PromptTokens + outputSoFar + ThoughtsTokens,
			},
			"modelVersion": model,
			"responseId":   "mock",
		}
	}

	if action == "generateContent" {
		writeJSON(w, http.StatusOK, chunk(Reply, OutputTokens, true))
		return
	}

	// Every streamed chunk carries usageMetadata, and candidatesTokenCount is a
	// running total, not a delta, so a parser must keep the last value rather
	// than sum them. As with the real API, the final chunk holds the finish
	// reason and no text.
	var chunks []object
	for i, word := range words() {
		chunks = append(chunks, chunk(word, i+1, false))
	}
	chunks = append(chunks, chunk("", OutputTokens, true))

	// alt=sse is what the SDKs request. Without it the real API streams one
	// JSON array, element by element, as application/json. The gateway has to
	// meter both.
	if r.URL.Query().Get("alt") == "sse" {
		s := startSSE(w)
		for _, c := range chunks {
			s.event("", c)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	for i, c := range chunks {
		sep := ",\n"
		if i == 0 {
			sep = "["
		}
		b, _ := json.Marshal(c)
		_, _ = w.Write([]byte(sep))
		_, _ = w.Write(b)
		_ = rc.Flush()
	}
	_, _ = w.Write([]byte("]\n"))
}
