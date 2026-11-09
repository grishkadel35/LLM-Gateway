package usage_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/grishkadel/llm-gateway/internal/provider"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// These tests go through usage.Meter only, so they hold however the parsers
// are arranged inside it.

// event is one server-sent event: its data payload, and its name, for the
// providers that send one (Anthropic).
type event struct{ name, data string }

// sseStream is one provider's streamed response, as the events it sends.
type sseStream struct {
	name   string
	format provider.Format
	// header is the response header: the content type, and the provider's
	// request ID for the providers that send it there.
	header http.Header
	// events add up to want. Each is JSON, and the last carries a part of the
	// usage, so a last event the gateway lost would show. (Real streams end with
	// OpenAI's [DONE] or Anthropic's message_stop, which carry none.)
	events []event
	want   usage.Usage
	// requestID is the provider's request ID the Result should carry: the
	// header's, or Gemini's responseId from the body.
	requestID string
}

// geminiChunk is one streamed Gemini response. Its usageMetadata is a running
// total, so the last chunk's counts win. finish, when not "", is the
// candidate's finishReason, which the last chunk of a real stream carries.
func geminiChunk(candidates, finish string) event {
	candidate := `{"content":{"parts":[{"text":"w"}]}}`
	if finish != "" {
		candidate = `{"content":{"parts":[{"text":"w"}]},"finishReason":"` + finish + `"}`
	}
	return event{data: `{"candidates":[` + candidate + `],"usageMetadata":{"promptTokenCount":20,` +
		`"cachedContentTokenCount":5,"candidatesTokenCount":` + candidates + `,"thoughtsTokenCount":7},` +
		`"modelVersion":"gemini-3.8-flash","responseId":"NxrUat6QM-XCytMPmIzNwQc"}`}
}

var sseStreams = []sseStream{
	{
		name:   "openai",
		format: provider.FormatOpenAI,
		header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"req_openai_1"}},
		events: []event{
			{data: `{"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hi"}}],"usage":null}`},
			{data: `{"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`},
			{data: `{"id":"c1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":23,"completion_tokens":11,"prompt_tokens_details":{"cached_tokens":7}}}`},
		},
		want:      usage.Usage{Model: "gpt-4o", Input: 16, CachedInput: 7, Output: 11},
		requestID: "req_openai_1",
	},
	{
		name:   "anthropic",
		format: provider.FormatAnthropic,
		header: http.Header{"Content-Type": {"text/event-stream"}, "Request-Id": {"req_anthropic_1"}},
		events: []event{
			// Input usage arrives in message_start, whose output_tokens is a
			// placeholder, and the real output count in message_delta.
			{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4","content":[],` +
				`"usage":{"input_tokens":17,"cache_read_input_tokens":6,"cache_creation_input_tokens":5,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":2},"output_tokens":1}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}`},
		},
		want:      usage.Usage{Model: "claude-sonnet-4", Input: 17, CachedInput: 6, CacheWrite: 3, CacheWrite1h: 2, Output: 12},
		requestID: "req_anthropic_1",
	},
	{
		name:      "gemini sse",
		format:    provider.FormatGemini,
		header:    http.Header{"Content-Type": {"text/event-stream"}},
		events:    []event{geminiChunk("1", ""), geminiChunk("2", ""), geminiChunk("10", "STOP")},
		want:      usage.Usage{Model: "gemini-3.8-flash", Input: 15, CachedInput: 5, Output: 17},
		requestID: "NxrUat6QM-XCytMPmIzNwQc",
	},
}

// framing is one way of writing a stream's events on the wire. The zero value
// is the plain one: LF line endings, "data: " with its space, nothing between
// events, every event's data on one line, and every event, the last too, ended
// by a blank line.
type framing struct {
	name string
	// crlf ends every line with CRLF in place of LF.
	crlf bool
	// noSpace writes "data:" and "event:" with no space after the colon.
	noSpace bool
	// comments puts a comment line (": ping") before every event but the first.
	// Before every second one (the third, the fifth...) a blank line follows it,
	// making it a keep-alive block of its own; before the others it is the
	// event's first line.
	comments bool
	// split writes every event's data on two lines, which the SSE spec joins
	// with "\n".
	split bool
	// cut takes that many line breaks off the end of the stream: one leaves the
	// last event with no blank line after it, two leave its last line with no
	// line break either.
	cut int
}

func (f framing) render(t *testing.T, events []event) string {
	t.Helper()
	eol, colon := "\n", ": "
	if f.crlf {
		eol = "\r\n"
	}
	if f.noSpace {
		colon = ":"
	}

	var b strings.Builder
	for i, e := range events {
		if f.comments && i > 0 {
			b.WriteString(": ping" + eol)
			if i%2 == 0 {
				b.WriteString(eol)
			}
		}
		if e.name != "" {
			b.WriteString("event" + colon + e.name + eol)
		}
		lines := []string{e.data}
		if f.split {
			lines = twoLines(t, e.data)
		}
		for _, l := range lines {
			b.WriteString("data" + colon + l + eol)
		}
		b.WriteString(eol)
	}
	return strings.TrimSuffix(b.String(), strings.Repeat(eol, f.cut))
}

// twoLines splits a JSON payload after its first comma, between two tokens, so
// that it is still JSON once an SSE parser joins the two lines with "\n".
func twoLines(t *testing.T, data string) []string {
	t.Helper()
	i := strings.Index(data, ",") + 1
	if i == 0 || !json.Valid([]byte(data[:i]+"\n"+data[i:])) {
		t.Fatalf("test bug: %s can't be split after its first comma", data)
	}
	return []string{data[:i], data[i:]}
}

// meterResponse passes body through usage.Meter as a 200 response with header,
// reads it to the end as the client does, and returns what the client received
// and the Result the callback got, which must have run once.
func meterResponse(t *testing.T, format provider.Format, header http.Header, body string, oneByte bool) (string, usage.Result) {
	t.Helper()
	var r io.Reader = strings.NewReader(body)
	if oneByte {
		// Every event split at every possible point.
		r = iotest.OneByteReader(r)
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(r),
		Request:    httptest.NewRequest(http.MethodPost, "/", nil),
	}
	var calls []usage.Result
	usage.Meter(resp, format, func(_ context.Context, res usage.Result) { calls = append(calls, res) })

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	_ = resp.Body.Close()
	if len(calls) != 1 {
		t.Fatalf("callback ran %d times, want once", len(calls))
	}
	return string(got), calls[0]
}

// eachReader runs f once for each way the client's bytes can arrive: in whole
// reads, and one byte at a time.
func eachReader(t *testing.T, f func(t *testing.T, oneByte bool)) {
	t.Helper()
	for _, oneByte := range []bool{false, true} {
		name := "whole reads"
		if oneByte {
			name = "one byte at a time"
		}
		t.Run(name, func(t *testing.T) { f(t, oneByte) })
	}
}

// plain meters the stream written plainly, which is what the stream means,
// and checks the Result is that: the usage want says, complete and streamed.
func (s sseStream) plain(t *testing.T) usage.Result {
	t.Helper()
	_, res := meterResponse(t, s.format, s.header, framing{}.render(t, s.events), false)
	if res.Usage != s.want || res.ProviderRequestID != s.requestID || res.Status != http.StatusOK || !res.Streamed || !res.Complete {
		t.Fatalf("the plain stream's result = %+v; want usage %+v, provider request ID %q, status 200, streamed and complete",
			res, s.want, s.requestID)
	}
	return res
}

// TestMeterSSEFraming: server-sent events may be framed in ways the gateway's
// parser has to read alike. Each framing, under each kind of read, passes the
// client the upstream's exact bytes and yields the same Result as the same
// stream written plainly.
func TestMeterSSEFraming(t *testing.T) {
	framings := []framing{
		{name: "plain"},
		{name: "CRLF line endings", crlf: true},
		{name: "data with no space after the colon", noSpace: true},
		{name: "comment lines between events", comments: true},
		{name: "events' data on two lines", split: true},
		// The gateway parses a last event with no blank line after it, which a
		// browser's EventSource would drop.
		{name: "last event with no blank line after it", cut: 1},
		{name: "last event with no line break at all", cut: 2},
		{name: "all of them at once", crlf: true, noSpace: true, comments: true, split: true, cut: 2},
	}
	for _, s := range sseStreams {
		t.Run(s.name, func(t *testing.T) {
			plain := s.plain(t)
			for _, f := range framings {
				t.Run(f.name, func(t *testing.T) {
					wire := f.render(t, s.events)
					eachReader(t, func(t *testing.T, oneByte bool) {
						got, res := meterResponse(t, s.format, s.header, wire, oneByte)
						if got != wire {
							t.Errorf("client received %q, want the upstream's bytes %q", got, wire)
						}
						if !reflect.DeepEqual(res, plain) {
							t.Errorf("result = %+v, want the plain stream's %+v; wire %q", res, plain, wire)
						}
					})
				})
			}
		})
	}
}

// TestMeterSkipsInvalidJSONEvents: a data line that isn't JSON between valid
// events is skipped, in every format, and the events after it still count.
func TestMeterSkipsInvalidJSONEvents(t *testing.T) {
	garbage := []struct{ name, data string }{
		{"cut-off object", `{"truncated":`},
		{"plain text", `not json at all`},
		{"cut-off array", `[1,2,`},
	}
	for _, s := range sseStreams {
		t.Run(s.name, func(t *testing.T) {
			plain := s.plain(t)
			for _, g := range garbage {
				t.Run(g.name, func(t *testing.T) {
					// One bad event between each pair of good ones.
					var events []event
					for i, e := range s.events {
						if i > 0 {
							events = append(events, event{data: g.data})
						}
						events = append(events, e)
					}
					wire := framing{}.render(t, events)
					eachReader(t, func(t *testing.T, oneByte bool) {
						got, res := meterResponse(t, s.format, s.header, wire, oneByte)
						if got != wire {
							t.Errorf("client received %q, want the upstream's bytes %q", got, wire)
						}
						if !reflect.DeepEqual(res, plain) {
							t.Errorf("result = %+v, want the stream without its bad events, %+v; wire %q", res, plain, wire)
						}
					})
				})
			}
		})
	}
}

// TestMeterAnthropicBodyWithoutUsage: an Anthropic body that isn't JSON, or
// that carries no usage, yields no usage, and does not panic.
func TestMeterAnthropicBodyWithoutUsage(t *testing.T) {
	cases := []struct{ name, body string }{
		{"not JSON", `this is not json`},
		{"empty", ``},
		{"cut off", `{"id":"msg_1","type":"message","usage":{"input_tokens":15,"output_tok`},
		{"no usage field", `{"id":"msg_1","type":"message","model":"claude-sonnet-4","content":[{"type":"text","text":"hi"}]}`},
		{"null usage", `{"id":"msg_1","type":"message","model":"claude-sonnet-4","usage":null}`},
		{"an error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
		{"an array", `[{"type":"message","usage":{"input_tokens":15,"output_tokens":10}}]`},
	}
	header := http.Header{"Content-Type": {"application/json"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, res := meterResponse(t, provider.FormatAnthropic, header, tc.body, false)
			if got != tc.body {
				t.Errorf("client received %q, want the upstream's bytes %q", got, tc.body)
			}
			if res.Tokens() != 0 {
				t.Errorf("usage = %+v, want no tokens", res.Usage)
			}
		})
	}
}
