package usage

import (
	"testing"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// parseBody runs a complete non-streaming body through the format's parser.
func parseBody(t *testing.T, format provider.Format, body string) Usage {
	t.Helper()
	p := newParser(format)
	if p == nil {
		t.Fatalf("no parser for format %q", format)
	}
	p.body([]byte(body))
	return p.usage()
}

// parseEvents runs SSE data payloads through the format's parser.
func parseEvents(t *testing.T, format provider.Format, events ...string) Usage {
	t.Helper()
	p := newParser(format)
	if p == nil {
		t.Fatalf("no parser for format %q", format)
	}
	for _, e := range events {
		p.event([]byte(e))
	}
	return p.usage()
}

// OpenAI's prompt_tokens includes cache reads; Usage.Input excludes them, so
// every format means the same thing by it: tokens billed at the input rate.
func TestOpenAIBody(t *testing.T) {
	got := parseBody(t, provider.FormatOpenAI, `{
		"id":"chatcmpl-1","model":"gpt-4o-2024-08-06",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],
		"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30,
		         "prompt_tokens_details":{"cached_tokens":5},
		         "completion_tokens_details":{"reasoning_tokens":4}}}`)

	want := Usage{Model: "gpt-4o-2024-08-06", Input: 15, CachedInput: 5, Output: 10}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestOpenAIStream(t *testing.T) {
	got := parseEvents(t, provider.FormatOpenAI,
		`{"id":"c","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hi"}}],"usage":null}`,
		`{"id":"c","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`,
		`{"id":"c","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":5}}}`,
		`[DONE]`,
	)

	want := Usage{Model: "gpt-4o", Input: 15, CachedInput: 5, Output: 10}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// Groq reports usage on the finish chunk even without include_usage, and the
// include_usage chunk repeats it: the last usage wins, never a sum.
func TestGroqStreamCountsUsageOnce(t *testing.T) {
	u := `{"prompt_tokens":20,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":5}}`
	got := parseEvents(t, provider.FormatOpenAI,
		`{"model":"llama-3.3-70b-versatile","choices":[{"delta":{"content":"hi"}}]}`,
		`{"model":"llama-3.3-70b-versatile","choices":[{"delta":{},"finish_reason":"stop"}],"usage":`+u+`,"x_groq":{"id":"req_1","usage":`+u+`}}`,
		`{"model":"llama-3.3-70b-versatile","choices":[],"usage":`+u+`}`,
		`[DONE]`,
	)

	want := Usage{Model: "llama-3.3-70b-versatile", Input: 15, CachedInput: 5, Output: 10}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// An error body, or anything that isn't a completion, has no usage: zeros,
// not a failure.
func TestOpenAINoUsage(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"Invalid model","type":"invalid_request_error"}}`,
		`not json`,
		``,
	} {
		if got := parseBody(t, provider.FormatOpenAI, body); got != (Usage{}) {
			t.Errorf("body %q: usage = %+v, want zero", body, got)
		}
	}
}
