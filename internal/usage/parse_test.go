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

// Anthropic's input_tokens already excludes cache reads and writes, so it
// maps to Input unchanged. Cache writes are split by TTL, which pricing needs:
// the two TTLs cost different amounts.
func TestAnthropicBody(t *testing.T) {
	got := parseBody(t, provider.FormatAnthropic, `{
		"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-20250514",
		"content":[{"type":"text","text":"hi"}],
		"usage":{"input_tokens":15,"cache_read_input_tokens":5,"cache_creation_input_tokens":5,
		         "cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":2},
		         "output_tokens":10}}`)

	want := Usage{Model: "claude-sonnet-4-20250514", Input: 15, CachedInput: 5, CacheWrite5m: 3, CacheWrite1h: 2, Output: 10}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// Without the TTL breakdown, cache writes are the default 5-minute TTL.
func TestAnthropicCacheWritesWithoutBreakdown(t *testing.T) {
	got := parseBody(t, provider.FormatAnthropic,
		`{"type":"message","model":"claude-x","usage":{"input_tokens":15,"cache_creation_input_tokens":4,"output_tokens":10}}`)

	if got.CacheWrite5m != 4 || got.CacheWrite1h != 0 {
		t.Errorf("cache writes = %d (5m), %d (1h); want 4, 0", got.CacheWrite5m, got.CacheWrite1h)
	}
}

const anthropicStart = `{"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4","content":[],
	"usage":{"input_tokens":15,"cache_read_input_tokens":5,"cache_creation_input_tokens":5,
	         "cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":2},"output_tokens":1}}}`

// Streams put input usage in message_start, where output_tokens is only a
// placeholder, and the real output count in the final message_delta.
func TestAnthropicStream(t *testing.T) {
	got := parseEvents(t, provider.FormatAnthropic,
		anthropicStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}`,
		`{"type":"message_stop"}`,
	)

	want := Usage{Model: "claude-sonnet-4", Input: 15, CachedInput: 5, CacheWrite5m: 3, CacheWrite1h: 2, Output: 10}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// A stream cut before message_delta has its input usage but no real output
// count; message_start's placeholder is not reported as output.
func TestAnthropicStreamCutAfterStart(t *testing.T) {
	got := parseEvents(t, provider.FormatAnthropic, anthropicStart)

	if got.Input != 15 || got.CachedInput != 5 || got.Output != 0 {
		t.Errorf("usage = %+v, want input 15, cached 5, output 0", got)
	}
}

// message_delta's usage is cumulative and may restate input counts; when it
// does, its numbers win.
func TestAnthropicDeltaRestatesInput(t *testing.T) {
	got := parseEvents(t, provider.FormatAnthropic,
		anthropicStart,
		`{"type":"message_delta","delta":{},"usage":{"input_tokens":18,"cache_read_input_tokens":5,"output_tokens":10}}`,
	)

	if got.Input != 18 || got.CachedInput != 5 || got.CacheWrite5m != 3 || got.Output != 10 {
		t.Errorf("usage = %+v, want input 18, cached 5, cache write 3 (unrestated), output 10", got)
	}
}

// A delta that restates the cache-write total without its TTL breakdown must
// not erase the breakdown message_start gave: 1-hour writes cost more.
func TestAnthropicDeltaKeepsCacheWriteBreakdown(t *testing.T) {
	got := parseEvents(t, provider.FormatAnthropic,
		anthropicStart,
		`{"type":"message_delta","delta":{},"usage":{"cache_creation_input_tokens":5,"output_tokens":10}}`,
	)

	if got.CacheWrite5m != 3 || got.CacheWrite1h != 2 {
		t.Errorf("cache writes = %d (5m), %d (1h); want the breakdown 3, 2 kept", got.CacheWrite5m, got.CacheWrite1h)
	}
}
