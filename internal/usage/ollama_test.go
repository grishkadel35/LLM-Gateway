package usage

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/iotest"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// TestOllamaFixtures meters real Ollama responses. The files in
// testdata/ollama are byte-for-byte what Ollama 0.40.2 sent for qwen3.5:9b
// through its OpenAI-compatible /v1/chat/completions, captured with curl on
// 2026-10-10. They show:
//   - usage arrives on every non-streaming reply, but on a stream only when
//     stream_options.include_usage is set, which ReadBody does for every
//     OpenAI-format stream: then a "choices": [] chunk carries it after the
//     finish chunk, before [DONE];
//   - cached_tokens is part of prompt_tokens, as at OpenAI: the repeated
//     request had 14 of its 19 prompt tokens in Ollama's cache;
//   - thinking (on for qwen3.5 unless reasoning_effort is "none") is counted
//     in completion_tokens.
func TestOllamaFixtures(t *testing.T) {
	cases := []struct {
		file        string
		status      int
		contentType string
		want        Usage
	}{
		{"chat.json", http.StatusOK, "application/json", Usage{Model: "qwen3.5:9b", Input: 19, Output: 1}},
		{"chat_cached.json", http.StatusOK, "application/json", Usage{Model: "qwen3.5:9b", Input: 5, CachedInput: 14, Output: 1}},
		{"chat_thinking.json", http.StatusOK, "application/json", Usage{Model: "qwen3.5:9b", Input: 3, CachedInput: 14, Output: 362}},
		{"chat_stream.sse", http.StatusOK, "text/event-stream", Usage{Model: "qwen3.5:9b", Input: 4, CachedInput: 15, Output: 1}},
		// Without include_usage there is no usage at all: the reason ReadBody
		// forces it on.
		{"chat_stream_no_usage.sse", http.StatusOK, "text/event-stream", Usage{Model: "qwen3.5:9b"}},
		{"error_unknown_model.json", http.StatusNotFound, "application/json", Usage{}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", "ollama", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			for _, oneByte := range []bool{false, true} {
				var r io.Reader = bytes.NewReader(body)
				if oneByte {
					// Every event split at every possible point.
					r = iotest.OneByteReader(r)
				}
				resp := response(tc.status, tc.contentType, r)
				calls := meter(resp, provider.FormatOpenAI)
				got, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()

				if !bytes.Equal(got, body) {
					t.Errorf("oneByte=%v: client received %q, want the fixture unchanged", oneByte, got)
				}
				if len(*calls) != 1 {
					t.Fatalf("oneByte=%v: callback ran %d times, want once", oneByte, len(*calls))
				}
				c := (*calls)[0]
				if c.Usage != tc.want || c.Status != tc.status || !c.Complete || c.Streamed != (tc.contentType == "text/event-stream") {
					t.Errorf("oneByte=%v: got usage %+v, status %d, complete %v, streamed %v; want %+v, %d, complete, streamed %v",
						oneByte, c.Usage, c.Status, c.Complete, c.Streamed, tc.want, tc.status, tc.contentType == "text/event-stream")
				}
			}
		})
	}
}
