package mockprovider

import "net/http"

// anthropicMessages serves POST /v1/messages.
func anthropicMessages(w http.ResponseWriter, r *http.Request) {
	// The real API rejects requests missing either header, and both are
	// supplied by gateway config (auth: x-api-key, plus a static header), so
	// checking them here catches a misconfigured provider block.
	if !requireHeader(w, r, "X-Api-Key") || !requireHeader(w, r, "Anthropic-Version") {
		return
	}

	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	if !req.Stream {
		writeJSON(w, http.StatusOK, object{
			"id":            "msg_mock",
			"type":          "message",
			"role":          "assistant",
			"model":         req.Model,
			"content":       []object{{"type": "text", "text": Reply}},
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage": object{
				"input_tokens":                PromptTokens - CachedPromptTokens,
				"cache_read_input_tokens":     CachedPromptTokens,
				"cache_creation_input_tokens": 0,
				"output_tokens":               OutputTokens,
			},
		})
		return
	}

	s := startSSE(w)

	// Input usage arrives up front in message_start. Its output_tokens is a
	// placeholder (1, as the real API sends); the true count comes in
	// message_delta at the end. A parser that stops at message_start
	// undercounts output.
	s.event("message_start", object{
		"type": "message_start",
		"message": object{
			"id":            "msg_mock",
			"type":          "message",
			"role":          "assistant",
			"model":         req.Model,
			"content":       []object{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": object{
				"input_tokens":                PromptTokens - CachedPromptTokens,
				"cache_read_input_tokens":     CachedPromptTokens,
				"cache_creation_input_tokens": 0,
				"output_tokens":               1,
			},
		},
	})
	s.event("content_block_start", object{
		"type":          "content_block_start",
		"index":         0,
		"content_block": object{"type": "text", "text": ""},
	})
	for _, word := range words() {
		s.event("content_block_delta", object{
			"type":  "content_block_delta",
			"index": 0,
			"delta": object{"type": "text_delta", "text": word},
		})
	}
	s.event("content_block_stop", object{"type": "content_block_stop", "index": 0})
	s.event("message_delta", object{
		"type":  "message_delta",
		"delta": object{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": object{"output_tokens": OutputTokens},
	})
	s.event("message_stop", object{"type": "message_stop"})
}
