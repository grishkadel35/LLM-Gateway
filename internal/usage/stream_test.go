package usage

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

const openAIStream = "data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n" +
	"data: {\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":10,\"prompt_tokens_details\":{\"cached_tokens\":5}}}\n\n" +
	"data: [DONE]\n\n"

const openAIBody = `{"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":5}}}`

var wantOpenAI = Usage{Model: "gpt-4o", Input: 15, CachedInput: 5, Output: 10}

// response builds an upstream response whose body is read through r.
func response(status int, contentType string, r io.Reader) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", contentType)
	h.Set("X-Request-Id", "req_provider_1")
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(r),
		Request:    (&http.Request{}).WithContext(context.Background()),
	}
}

// meter wraps resp and records every callback.
func meter(resp *http.Response, format provider.Format) *[]Result {
	var calls []Result
	Meter(resp, format, func(_ context.Context, r Result) { calls = append(calls, r) })
	return &calls
}

// TestMeterPassesBytesThrough: the client must receive exactly what the
// provider sent, metered or not, however the bytes arrive.
func TestMeterPassesBytesThrough(t *testing.T) {
	cases := []struct {
		name, contentType, body string
		streamed                bool
	}{
		{"sse", "text/event-stream; charset=utf-8", openAIStream, true},
		{"json", "application/json", openAIBody, false},
	}
	for _, tc := range cases {
		for _, oneByte := range []bool{false, true} {
			var r io.Reader = strings.NewReader(tc.body)
			if oneByte {
				// Every event split at every possible point.
				r = iotest.OneByteReader(r)
			}
			resp := response(http.StatusOK, tc.contentType, r)
			calls := meter(resp, provider.FormatOpenAI)

			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("%s: reading: %v", tc.name, err)
			}
			_ = resp.Body.Close()

			if string(got) != tc.body {
				t.Errorf("%s (one byte = %v): client got %q, want the upstream bytes", tc.name, oneByte, got)
			}
			if len(*calls) != 1 {
				t.Fatalf("%s (one byte = %v): callback ran %d times, want once", tc.name, oneByte, len(*calls))
			}
			res := (*calls)[0]
			if res.Usage != wantOpenAI || res.Streamed != tc.streamed || !res.Complete {
				t.Errorf("%s (one byte = %v): result = %+v, want usage %+v, streamed %v, complete", tc.name, oneByte, res, wantOpenAI, tc.streamed)
			}
			if res.Status != http.StatusOK || res.ProviderRequestID != "req_provider_1" {
				t.Errorf("%s: status %d, provider request ID %q", tc.name, res.Status, res.ProviderRequestID)
			}
		}
	}
}

// TestMeterOnEarlyClose: a client that disconnects mid-stream makes the
// proxy close the body before EOF. The usage seen so far is still reported,
// once, marked incomplete.
func TestMeterOnEarlyClose(t *testing.T) {
	cut := strings.Index(openAIStream, "data: [DONE]")
	resp := response(http.StatusOK, "text/event-stream", strings.NewReader(openAIStream))
	calls := meter(resp, provider.FormatOpenAI)

	buf := make([]byte, cut)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_ = resp.Body.Close()

	if len(*calls) != 1 {
		t.Fatalf("callback ran %d times, want once", len(*calls))
	}
	// The usage chunk came before the cut, but the stream didn't end: its
	// usage is incomplete too.
	if res := (*calls)[0]; res.Complete || res.UsageComplete || res.Usage != wantOpenAI {
		t.Errorf("result = %+v, want usage %+v, incomplete", res, wantOpenAI)
	}
}

// TestMeterUpstreamError: an error response is metered too, with no tokens,
// so every request that reached a provider leaves a record.
func TestMeterUpstreamError(t *testing.T) {
	resp := response(http.StatusTooManyRequests, "application/json",
		strings.NewReader(`{"error":{"message":"Rate limit reached","type":"rate_limit_exceeded"}}`))
	calls := meter(resp, provider.FormatOpenAI)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if len(*calls) != 1 {
		t.Fatalf("callback ran %d times, want once", len(*calls))
	}
	if res := (*calls)[0]; res.Status != http.StatusTooManyRequests || res.Usage != (Usage{}) || !res.UsageComplete {
		t.Errorf("result = %+v, want status 429 and no usage, complete", res)
	}
}

// TestMeterAnthropicRequestID: Anthropic names its request ID header
// differently.
func TestMeterAnthropicRequestID(t *testing.T) {
	resp := response(http.StatusOK, "application/json", strings.NewReader(`{}`))
	resp.Header.Del("X-Request-Id")
	resp.Header.Set("Request-Id", "req_anthropic_1")
	calls := meter(resp, provider.FormatOpenAI)
	_, _ = io.ReadAll(resp.Body)

	if got := (*calls)[0].ProviderRequestID; got != "req_anthropic_1" {
		t.Errorf("ProviderRequestID = %q, want req_anthropic_1", got)
	}
}

// TestMeterGeminiRequestID: Gemini sends no request ID header, so its ID is the
// responseId in the body, streamed or not. A header, if one did arrive, wins.
func TestMeterGeminiRequestID(t *testing.T) {
	chunk := `{"candidates":[{"content":{"parts":[{"text":"w"}]}}],"usageMetadata":{"promptTokenCount":3},` +
		`"modelVersion":"gemini-3.8-flash","responseId":"` + geminiResponseID + `"}`
	cases := []struct {
		name, contentType, body, header string
		streamed                        bool
		want                            string
	}{
		{"json", "application/json", chunk, "", false, geminiResponseID},
		{"sse", "text/event-stream", "data: " + chunk + "\n\ndata: " + chunk + "\n\n", "", true, geminiResponseID},
		{"header wins", "application/json", chunk, "req_header_1", false, "req_header_1"},
	}
	for _, tc := range cases {
		resp := response(http.StatusOK, tc.contentType, strings.NewReader(tc.body))
		resp.Header.Del("X-Request-Id")
		if tc.header != "" {
			resp.Header.Set("X-Request-Id", tc.header)
		}
		calls := meter(resp, provider.FormatGemini)
		_, _ = io.ReadAll(resp.Body)

		if len(*calls) != 1 {
			t.Fatalf("%s: callback ran %d times, want once", tc.name, len(*calls))
		}
		if res := (*calls)[0]; res.ProviderRequestID != tc.want || res.Streamed != tc.streamed {
			t.Errorf("%s: provider request ID %q, streamed %v; want %q, %v", tc.name, res.ProviderRequestID, res.Streamed, tc.want, tc.streamed)
		}
	}
}

// TestMeterFallsBackToRequestedModel: error responses rarely name a model,
// and Gemini keeps it in the URL; the requested model fills the gap.
func TestMeterFallsBackToRequestedModel(t *testing.T) {
	resp := response(http.StatusBadRequest, "application/json", strings.NewReader(`{"error":{"message":"bad"}}`))
	resp.Request = resp.Request.WithContext(context.WithValue(context.Background(), requestKey{}, &Request{Model: "gpt-4o"}))
	calls := meter(resp, provider.FormatOpenAI)
	_, _ = io.ReadAll(resp.Body)

	if got := (*calls)[0].Model; got != "gpt-4o" {
		t.Errorf("Model = %q, want the requested gpt-4o", got)
	}
}

// TestMeterReportsJSONStreamAsStreamed: Gemini's default stream is a JSON
// array sent as application/json. It is parsed as one body, but it was still
// a streamed response.
func TestMeterReportsJSONStreamAsStreamed(t *testing.T) {
	resp := response(http.StatusOK, "application/json", strings.NewReader(`[{"usageMetadata":{"promptTokenCount":3}}]`))
	resp.Request = resp.Request.WithContext(context.WithValue(context.Background(), requestKey{}, &Request{Stream: true}))
	calls := meter(resp, provider.FormatGemini)
	_, _ = io.ReadAll(resp.Body)

	if res := (*calls)[0]; !res.Streamed || res.Input != 3 {
		t.Errorf("result = %+v, want streamed with input 3", res)
	}
}

// TestMeterOverflowIsIncomplete: a body too large to parse has unknown
// usage, which must not look like a complete response with no tokens.
func TestMeterOverflowIsIncomplete(t *testing.T) {
	cases := []struct {
		name, contentType, body string
	}{
		{"json", "application/json", `{"pad":"` + strings.Repeat("a", maxBufferedBody) + `"}`},
		// One event that never ends: many data lines, no blank line.
		{"sse", "text/event-stream", strings.Repeat("data: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", maxBufferedBody/30)},
	}
	for _, tc := range cases {
		resp := response(http.StatusOK, tc.contentType, strings.NewReader(tc.body))
		calls := meter(resp, provider.FormatOpenAI)
		n, _ := io.Copy(io.Discard, resp.Body)

		if n != int64(len(tc.body)) {
			t.Errorf("%s: client got %d bytes, want all %d", tc.name, n, len(tc.body))
		}
		if res := (*calls)[0]; res.Complete || res.UsageComplete {
			t.Errorf("%s: result = Complete or UsageComplete, want incomplete after overflow", tc.name)
		}
	}
}

// TestMeterUsageComplete: a 2xx's usage is complete only when its body was
// read to the end and carried the provider's final usage report, as each
// format sends it:
//   - openai (Groq and Ollama too): a usage object, in a body or on a chunk,
//     Groq's finish chunk included
//   - anthropic: a body's usage, or a stream's message_delta usage, never
//     message_start's alone
//   - gemini: a body's usageMetadata, the JSON array's last element's, and on
//     SSE the one in the chunk that carries finishReason, or after it
//
// Any other status reports no usage, so it is complete once read to the end.
// A body closed short of its end is complete in no case, its report seen or
// not.
func TestMeterUsageComplete(t *testing.T) {
	const sse, jsonType = "text/event-stream", "application/json"
	events := func(data ...string) string {
		var b strings.Builder
		for _, d := range data {
			b.WriteString("data: " + d + "\n\n")
		}
		return b.String()
	}
	openAIDelta := `{"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hi"}}],"usage":null}`
	openAIFinish := `{"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`
	groqUsage := `{"prompt_tokens":20,"completion_tokens":10}`
	groqFinish := `{"model":"openai/gpt-oss-120b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":` + groqUsage +
		`,"x_groq":{"id":"req_1","usage":` + groqUsage + `}}`
	messageStart := `{"type":"message_start","message":{"model":"claude-sonnet-4","usage":{"input_tokens":15,"output_tokens":1}}}`
	messageDelta := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}`
	messageStop := `{"type":"message_stop"}`
	geminiRunning := `{"candidates":[{"content":{"parts":[{"text":"w"}]}}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":1}}`
	geminiFinish := `{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":10}}`
	geminiFinishBare := `{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}]}`
	geminiUsageOnly := `{"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":10}}`

	cases := []struct {
		name        string
		format      provider.Format
		status      int
		contentType string
		body        string
		want        bool // UsageComplete, read to the end
	}{
		{"openai body", provider.FormatOpenAI, http.StatusOK, jsonType, openAIBody, true},
		{"openai body without usage", provider.FormatOpenAI, http.StatusOK, jsonType, `{"model":"gpt-4o","choices":[]}`, false},
		{"openai body with usage null", provider.FormatOpenAI, http.StatusOK, jsonType, `{"model":"gpt-4o","choices":[],"usage":null}`, false},
		{"openai stream", provider.FormatOpenAI, http.StatusOK, sse, openAIStream, true},
		{"openai stream without the usage chunk", provider.FormatOpenAI, http.StatusOK, sse, events(openAIDelta, openAIFinish, "[DONE]"), false},
		{"groq stream: the finish chunk's usage", provider.FormatOpenAI, http.StatusOK, sse, events(openAIDelta, groqFinish, "[DONE]"), true},
		{"anthropic body", provider.FormatAnthropic, http.StatusOK, jsonType,
			`{"type":"message","model":"claude-sonnet-4","usage":{"input_tokens":15,"output_tokens":10}}`, true},
		{"anthropic body without usage", provider.FormatAnthropic, http.StatusOK, jsonType, `{"type":"message","model":"claude-sonnet-4","content":[]}`, false},
		{"anthropic stream", provider.FormatAnthropic, http.StatusOK, sse, events(messageStart, messageDelta, messageStop), true},
		{"anthropic stream with message_start alone", provider.FormatAnthropic, http.StatusOK, sse, events(messageStart, messageStop), false},
		{"gemini body", provider.FormatGemini, http.StatusOK, jsonType, geminiFinish, true},
		// A blocked prompt has no candidates, so no finishReason, and its
		// usageMetadata is still the report.
		{"gemini body without a finishReason", provider.FormatGemini, http.StatusOK, jsonType,
			`{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":20}}`, true},
		{"gemini body without usageMetadata", provider.FormatGemini, http.StatusOK, jsonType, geminiFinishBare, false},
		{"gemini sse: usage on the finishing chunk", provider.FormatGemini, http.StatusOK, sse, events(geminiRunning, geminiFinish), true},
		{"gemini sse: usage after the finishing chunk", provider.FormatGemini, http.StatusOK, sse,
			events(geminiRunning, geminiFinishBare, geminiUsageOnly), true},
		{"gemini sse: running totals only", provider.FormatGemini, http.StatusOK, sse, events(geminiRunning, geminiRunning), false},
		{"gemini sse: a finish without usage", provider.FormatGemini, http.StatusOK, sse, events(geminiRunning, geminiFinishBare), false},
		{"gemini json array", provider.FormatGemini, http.StatusOK, jsonType, "[" + geminiRunning + ",\n" + geminiFinish + "]", true},
		{"gemini json array: the last element without usage", provider.FormatGemini, http.StatusOK, jsonType,
			"[" + geminiRunning + ",\n" + geminiFinishBare + "]", false},
		{"a provider's 429 without usage", provider.FormatOpenAI, http.StatusTooManyRequests, jsonType, `{"error":{"message":"Rate limit reached"}}`, true},
		{"a provider's 502 without usage", provider.FormatAnthropic, http.StatusBadGateway, jsonType, `{"type":"error","error":{"type":"api_error"}}`, true},
		{"a provider's 504 without usage", provider.FormatGemini, http.StatusGatewayTimeout, jsonType, `{"error":{"code":504,"status":"DEADLINE_EXCEEDED"}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := response(tc.status, tc.contentType, strings.NewReader(tc.body))
			calls := meter(resp, tc.format)
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if res := (*calls)[0]; !res.Complete || res.UsageComplete != tc.want {
				t.Errorf("read to the end: complete %v, usage complete %v; want complete, usage complete %v", res.Complete, res.UsageComplete, tc.want)
			}

			// One byte short of the end: the report, if there is one, came
			// before the cut.
			cut := response(tc.status, tc.contentType, strings.NewReader(tc.body))
			calls = meter(cut, tc.format)
			if _, err := io.ReadFull(cut.Body, make([]byte, len(tc.body)-1)); err != nil {
				t.Fatal(err)
			}
			_ = cut.Body.Close()
			if res := (*calls)[0]; res.Complete || res.UsageComplete {
				t.Errorf("cut short: complete %v, usage complete %v; want neither", res.Complete, res.UsageComplete)
			}
		})
	}
}
