package mockprovider

import "net/http"

// openAIChat serves POST /v1/chat/completions.
func openAIChat(w http.ResponseWriter, r *http.Request) {
	chatCompletions(w, r, false)
}

// groqChat serves Groq's OpenAI-compatible POST /openai/v1/chat/completions.
// It differs from OpenAI only in where a stream reports usage.
func groqChat(w http.ResponseWriter, r *http.Request) {
	chatCompletions(w, r, true)
}

func chatCompletions(w http.ResponseWriter, r *http.Request, groq bool) {
	if !requireHeader(w, r, "Authorization") {
		return
	}

	var req struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	usage := object{
		"prompt_tokens":         PromptTokens,
		"completion_tokens":     OutputTokens,
		"total_tokens":          PromptTokens + OutputTokens,
		"prompt_tokens_details": object{"cached_tokens": CachedPromptTokens},
	}

	if !req.Stream {
		writeJSON(w, http.StatusOK, object{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion",
			"created": 0,
			"model":   req.Model,
			"choices": []object{{
				"index":         0,
				"message":       object{"role": "assistant", "content": Reply},
				"finish_reason": "stop",
			}},
			"usage": usage,
		})
		return
	}

	chunk := func(choices []object) object {
		return object{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   req.Model,
			"choices": choices,
		}
	}

	s := startSSE(w)
	s.event("", chunk([]object{{"index": 0, "delta": object{"role": "assistant", "content": ""}, "finish_reason": nil}}))
	for _, word := range words() {
		s.event("", chunk([]object{{"index": 0, "delta": object{"content": word}, "finish_reason": nil}}))
	}
	final := chunk([]object{{"index": 0, "delta": object{}, "finish_reason": "stop"}})
	// Groq (checked live 2026-10-02) puts usage on the finish chunk whether or
	// not the request opts in, at the top level and again under x_groq. With
	// include_usage it also sends the extra chunk below, so the same usage
	// arrives twice: a parser must keep the last value, not sum them.
	if groq {
		final["usage"] = usage
		final["x_groq"] = object{"id": "req_mock", "usage": usage}
	}
	s.event("", final)

	// OpenAI reports usage in a stream only when the request opts in, as one
	// extra chunk with empty choices. Without it a stream carries no usage at
	// all, which is why the gateway will inject include_usage in Week 2.
	if req.StreamOptions.IncludeUsage {
		c := chunk([]object{})
		c["usage"] = usage
		s.event("", c)
	}
	s.event("", "[DONE]")
}
