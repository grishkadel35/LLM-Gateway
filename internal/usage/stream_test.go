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
	if res := (*calls)[0]; res.Complete || res.Usage != wantOpenAI {
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
	if res := (*calls)[0]; res.Status != http.StatusTooManyRequests || res.Usage != (Usage{}) {
		t.Errorf("result = %+v, want status 429 and no usage", res)
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
		if res := (*calls)[0]; res.Complete {
			t.Errorf("%s: result = Complete, want incomplete after overflow", tc.name)
		}
	}
}
