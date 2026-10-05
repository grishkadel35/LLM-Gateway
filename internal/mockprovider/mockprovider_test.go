package mockprovider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// providerHeaders are the credential headers each provider expects, as the
// gateway would send them.
var (
	openAIHeaders    = map[string]string{"Authorization": "Bearer k"}
	anthropicHeaders = map[string]string{"x-api-key": "k", "anthropic-version": "2023-06-01"}
	geminiHeaders    = map[string]string{"x-goog-api-key": "k"}
)

// post sends a JSON body to the mock and returns the response and its body.
func post(t *testing.T, path string, headers map[string]string, body string) (*http.Response, string) {
	t.Helper()

	srv := httptest.NewServer(Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(b)
}

type sseEvent struct {
	name string
	data string
}

// parseSSE splits a server-sent event stream into its events.
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()

	var events []sseEvent
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var e sseEvent
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				e.name = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				e.data = v
			} else {
				t.Fatalf("unexpected SSE line %q", line)
			}
		}
		events = append(events, e)
	}
	return events
}

func mustUnmarshal(t *testing.T, data string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(data), v); err != nil {
		t.Fatalf("parsing %q: %v", data, err)
	}
}

func TestWordsRebuildReply(t *testing.T) {
	w := words()
	if got := strings.Join(w, ""); got != Reply {
		t.Errorf("joined words = %q, want %q", got, Reply)
	}
	if len(w) != OutputTokens {
		t.Errorf("len(words) = %d, want OutputTokens = %d", len(w), OutputTokens)
	}
}

type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func checkOpenAIUsage(t *testing.T, u *openAIUsage) {
	t.Helper()
	if u == nil {
		t.Fatal("no usage reported")
	}
	if u.PromptTokens != PromptTokens || u.CompletionTokens != OutputTokens || u.PromptTokensDetails.CachedTokens != CachedPromptTokens {
		t.Errorf("usage = %+v, want prompt %d, completion %d, cached %d", *u, PromptTokens, OutputTokens, CachedPromptTokens)
	}
}

func TestOpenAINonStreaming(t *testing.T) {
	res, body := post(t, "/v1/chat/completions", openAIHeaders, `{"model":"gpt-x"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}

	var got struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *openAIUsage `json:"usage"`
	}
	mustUnmarshal(t, body, &got)

	if got.Model != "gpt-x" {
		t.Errorf("model = %q, want the requested model echoed", got.Model)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != Reply {
		t.Errorf("choices = %+v, want one with content %q", got.Choices, Reply)
	}
	checkOpenAIUsage(t, got.Usage)
}

func TestOpenAIStreaming(t *testing.T) {
	for _, includeUsage := range []bool{false, true} {
		name := "without include_usage"
		body := `{"model":"gpt-x","stream":true}`
		if includeUsage {
			name = "with include_usage"
			body = `{"model":"gpt-x","stream":true,"stream_options":{"include_usage":true}}`
		}

		t.Run(name, func(t *testing.T) {
			res, raw := post(t, "/v1/chat/completions", openAIHeaders, body)
			if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
				t.Errorf("Content-Type = %q, want text/event-stream", ct)
			}

			events := parseSSE(t, raw)
			if last := events[len(events)-1].data; last != "[DONE]" {
				t.Fatalf("last event = %q, want [DONE]", last)
			}

			var text strings.Builder
			var usage *openAIUsage
			for _, e := range events[:len(events)-1] {
				var c struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
					Usage *openAIUsage `json:"usage"`
				}
				mustUnmarshal(t, e.data, &c)
				for _, ch := range c.Choices {
					text.WriteString(ch.Delta.Content)
				}
				if c.Usage != nil {
					usage = c.Usage
				}
			}

			if text.String() != Reply {
				t.Errorf("streamed text = %q, want %q", text.String(), Reply)
			}
			if includeUsage {
				checkOpenAIUsage(t, usage)
			} else if usage != nil {
				t.Errorf("usage = %+v, want none without include_usage", *usage)
			}
		})
	}
}

// TestGroqStreaming checks the Groq quirk: usage arrives on the finish chunk
// even without include_usage, and with it a second time in OpenAI's extra
// chunk.
func TestGroqStreaming(t *testing.T) {
	for _, includeUsage := range []bool{false, true} {
		name := "without include_usage"
		body := `{"model":"gpt-x","stream":true}`
		wantUsageChunks := 1
		if includeUsage {
			name = "with include_usage"
			body = `{"model":"gpt-x","stream":true,"stream_options":{"include_usage":true}}`
			wantUsageChunks = 2
		}

		t.Run(name, func(t *testing.T) {
			res, raw := post(t, "/openai/v1/chat/completions", openAIHeaders, body)
			if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
				t.Errorf("Content-Type = %q, want text/event-stream", ct)
			}

			events := parseSSE(t, raw)
			if last := events[len(events)-1].data; last != "[DONE]" {
				t.Fatalf("last event = %q, want [DONE]", last)
			}

			usageChunks := 0
			for _, e := range events[:len(events)-1] {
				var c struct {
					Choices []struct {
						FinishReason *string `json:"finish_reason"`
					} `json:"choices"`
					Usage *openAIUsage `json:"usage"`
					XGroq *struct {
						Usage *openAIUsage `json:"usage"`
					} `json:"x_groq"`
				}
				mustUnmarshal(t, e.data, &c)

				finishing := len(c.Choices) == 1 && c.Choices[0].FinishReason != nil
				if finishing {
					checkOpenAIUsage(t, c.Usage)
					if c.XGroq == nil {
						t.Fatal("finish chunk has no x_groq")
					}
					checkOpenAIUsage(t, c.XGroq.Usage)
				}
				if c.Usage != nil {
					checkOpenAIUsage(t, c.Usage)
					usageChunks++
				}
			}

			if usageChunks != wantUsageChunks {
				t.Errorf("chunks carrying usage = %d, want %d", usageChunks, wantUsageChunks)
			}
		})
	}
}

type anthropicUsage struct {
	InputTokens          int `json:"input_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
	OutputTokens         int `json:"output_tokens"`
}

func TestAnthropicNonStreaming(t *testing.T) {
	res, body := post(t, "/v1/messages", anthropicHeaders, `{"model":"claude-x","max_tokens":64}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}

	var got struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage anthropicUsage `json:"usage"`
	}
	mustUnmarshal(t, body, &got)

	if len(got.Content) != 1 || got.Content[0].Text != Reply {
		t.Errorf("content = %+v, want one block with %q", got.Content, Reply)
	}
	want := anthropicUsage{PromptTokens - CachedPromptTokens, CachedPromptTokens, OutputTokens}
	if got.Usage != want {
		t.Errorf("usage = %+v, want %+v", got.Usage, want)
	}
}

func TestAnthropicStreaming(t *testing.T) {
	_, raw := post(t, "/v1/messages", anthropicHeaders, `{"model":"claude-x","max_tokens":64,"stream":true}`)
	events := parseSSE(t, raw)

	var names []string
	var text strings.Builder
	var start, delta anthropicUsage
	for _, e := range events {
		names = append(names, e.name)

		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
			Usage anthropicUsage `json:"usage"`
		}
		mustUnmarshal(t, e.data, &ev)
		if ev.Type != e.name {
			t.Errorf("event %q has data type %q", e.name, ev.Type)
		}

		switch e.name {
		case "message_start":
			start = ev.Message.Usage
		case "content_block_delta":
			text.WriteString(ev.Delta.Text)
		case "message_delta":
			delta = ev.Usage
		}
	}

	if names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Errorf("events = %v, want message_start first and message_stop last", names)
	}
	if text.String() != Reply {
		t.Errorf("streamed text = %q, want %q", text.String(), Reply)
	}
	if start.InputTokens != PromptTokens-CachedPromptTokens || start.CacheReadInputTokens != CachedPromptTokens {
		t.Errorf("message_start usage = %+v, want input %d, cache read %d", start, PromptTokens-CachedPromptTokens, CachedPromptTokens)
	}
	// message_start's output count is a placeholder; the real one comes last.
	if start.OutputTokens == OutputTokens {
		t.Errorf("message_start output_tokens = %d, want a placeholder, not the final count", start.OutputTokens)
	}
	if delta.OutputTokens != OutputTokens {
		t.Errorf("message_delta output_tokens = %d, want %d", delta.OutputTokens, OutputTokens)
	}
}

type geminiChunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
		TotalTokenCount         int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

// checkGeminiChunks checks a whole Gemini response (one chunk, or a stream):
// the text reassembles, output counts only grow, and the last chunk holds the
// final usage and finish reason.
func checkGeminiChunks(t *testing.T, chunks []geminiChunk) {
	t.Helper()

	var text strings.Builder
	prev := 0
	for _, c := range chunks {
		if c.ModelVersion != "gemini-x" {
			t.Errorf("modelVersion = %q, want the model from the path", c.ModelVersion)
		}
		for _, p := range c.Candidates[0].Content.Parts {
			text.WriteString(p.Text)
		}
		if n := c.UsageMetadata.CandidatesTokenCount; n < prev {
			t.Errorf("candidatesTokenCount went from %d to %d; it should be a running total", prev, n)
		}
		prev = c.UsageMetadata.CandidatesTokenCount
	}

	if text.String() != Reply {
		t.Errorf("text = %q, want %q", text.String(), Reply)
	}
	last := chunks[len(chunks)-1]
	if last.Candidates[0].FinishReason != "STOP" {
		t.Errorf("last finishReason = %q, want STOP", last.Candidates[0].FinishReason)
	}
	u := last.UsageMetadata
	if u.PromptTokenCount != PromptTokens || u.CachedContentTokenCount != CachedPromptTokens || u.CandidatesTokenCount != OutputTokens || u.ThoughtsTokenCount != ThoughtsTokens {
		t.Errorf("final usage = %+v, want prompt %d, cached %d, candidates %d, thoughts %d", u, PromptTokens, CachedPromptTokens, OutputTokens, ThoughtsTokens)
	}
	// Thinking is billed but sits outside candidatesTokenCount; only the total
	// shows it. A parser reading candidates alone undercounts output.
	if want := PromptTokens + OutputTokens + ThoughtsTokens; u.TotalTokenCount != want {
		t.Errorf("totalTokenCount = %d, want %d (prompt + candidates + thoughts)", u.TotalTokenCount, want)
	}
}

func TestGeminiNonStreaming(t *testing.T) {
	res, body := post(t, "/v1beta/models/gemini-x:generateContent", geminiHeaders, `{}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}

	var c geminiChunk
	mustUnmarshal(t, body, &c)
	checkGeminiChunks(t, []geminiChunk{c})
}

func TestGeminiStreamingSSE(t *testing.T) {
	res, raw := post(t, "/v1beta/models/gemini-x:streamGenerateContent?alt=sse", geminiHeaders, `{}`)
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	var chunks []geminiChunk
	for _, e := range parseSSE(t, raw) {
		var c geminiChunk
		mustUnmarshal(t, e.data, &c)
		chunks = append(chunks, c)
	}
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want a multi-chunk stream", len(chunks))
	}
	checkGeminiChunks(t, chunks)
}

func TestGeminiStreamingJSONArray(t *testing.T) {
	res, raw := post(t, "/v1beta/models/gemini-x:streamGenerateContent", geminiHeaders, `{}`)
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var chunks []geminiChunk
	mustUnmarshal(t, raw, &chunks)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want a multi-chunk array", len(chunks))
	}
	checkGeminiChunks(t, chunks)
}

func TestMissingCredentialRejected(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		headers map[string]string
	}{
		{"openai", "/v1/chat/completions", nil},
		// A key in the wrong provider's header must not count.
		{"anthropic with bearer", "/v1/messages", map[string]string{"Authorization": "Bearer k", "anthropic-version": "2023-06-01"}},
		{"anthropic without version", "/v1/messages", map[string]string{"x-api-key": "k"}},
		{"gemini", "/v1beta/models/gemini-x:generateContent", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, body := post(t, tt.path, tt.headers, `{}`)
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (body %s)", res.StatusCode, body)
			}
		})
	}
}

func TestMalformedBodyRejected(t *testing.T) {
	res, _ := post(t, "/v1/chat/completions", openAIHeaders, `{not json`)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestGeminiUnknownMethod(t *testing.T) {
	res, _ := post(t, "/v1beta/models/gemini-x:countTokens", geminiHeaders, `{}`)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}
