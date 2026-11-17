package mockprovider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// streamCases are the five stream shapes: each provider's, and Gemini's JSON
// array. count returns how many events (chunks) a body holds.
var streamCases = []struct {
	name    string
	path    string
	headers map[string]string
	body    string
	count   func(t *testing.T, body string) int
}{
	{"openai", "/v1/chat/completions", openAIHeaders, `{"stream":true}`, countSSE},
	{"groq", "/openai/v1/chat/completions", openAIHeaders, `{"stream":true}`, countSSE},
	{"anthropic", "/v1/messages", anthropicHeaders, `{"stream":true}`, countSSE},
	{"gemini-sse", "/v1beta/models/gemini-x:streamGenerateContent?alt=sse", geminiHeaders, `{}`, countSSE},
	{"gemini-array", "/v1beta/models/gemini-x:streamGenerateContent", geminiHeaders, `{}`,
		func(t *testing.T, body string) int {
			var chunks []geminiChunk
			mustUnmarshal(t, body, &chunks)
			return len(chunks)
		}},
}

func countSSE(t *testing.T, body string) int {
	return len(parseSSE(t, body))
}

// timedStream posts to a mock with opts and returns the whole body, plus when
// the first and the last bytes arrived, measured from just before the request.
func timedStream(t *testing.T, opts Options, path string, headers map[string]string, body string) (raw string, first, last time.Duration) {
	t.Helper()

	srv := httptest.NewServer(NewHandler(opts))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if sb.Len() == 0 {
				first = time.Since(start)
			}
			last = time.Since(start)
			sb.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return sb.String(), first, last
}

// TestChunkDelay checks each stream's first event comes straight away and its
// last no sooner than (events-1) delays later, and that the delay changes
// timing only: the bytes are those of an undelayed stream. Lower bounds are
// firm, since a timer never fires early; upper bounds are loose, for a loaded
// machine under -race.
func TestChunkDelay(t *testing.T) {
	const delay = 50 * time.Millisecond

	for _, tc := range streamCases {
		t.Run(tc.name, func(t *testing.T) {
			want, _, _ := timedStream(t, Options{}, tc.path, tc.headers, tc.body)
			got, first, last := timedStream(t, Options{ChunkDelay: delay}, tc.path, tc.headers, tc.body)

			if got != want {
				t.Errorf("delayed body differs from the undelayed one:\n got %q\nwant %q", got, want)
			}

			gaps := time.Duration(tc.count(t, got)-1) * delay
			if gaps < 5*delay {
				t.Fatalf("stream has only %d events, want a multi-event stream", tc.count(t, got))
			}
			if first >= gaps/2 {
				t.Errorf("first event after %v, want well before the stream's %v of delays", first, gaps)
			}
			if last < gaps {
				t.Errorf("last event after %v, want at least %v", last, gaps)
			}
			if last > gaps+2*time.Second {
				t.Errorf("last event after %v, want about %v", last, gaps)
			}
		})
	}
}

// TestChunkDelayNotOnNonStreaming checks a plain answer never waits.
func TestChunkDelayNotOnNonStreaming(t *testing.T) {
	const delay = 5 * time.Second

	for _, tc := range []struct {
		name    string
		path    string
		headers map[string]string
		body    string
	}{
		{"openai", "/v1/chat/completions", openAIHeaders, `{}`},
		{"groq", "/openai/v1/chat/completions", openAIHeaders, `{}`},
		{"anthropic", "/v1/messages", anthropicHeaders, `{}`},
		{"gemini", "/v1beta/models/gemini-x:generateContent", geminiHeaders, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, last := timedStream(t, Options{ChunkDelay: delay}, tc.path, tc.headers, tc.body)
			if last >= delay {
				t.Errorf("non-streaming answer took %v, want no chunk delay", last)
			}
		})
	}
}

// TestChunkDelayStopsWhenClientLeaves checks a client that cancels mid-stream
// ends the handler promptly, rather than leaving it sleeping through the rest.
func TestChunkDelayStopsWhenClientLeaves(t *testing.T) {
	for _, tc := range streamCases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(Options{ChunkDelay: time.Hour})
			done := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				h.ServeHTTP(w, r)
			}))
			defer srv.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()

			// The first event is out and the handler is now sleeping an hour.
			if _, err := res.Body.Read(make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
				t.Fatal("handler ended before the client left")
			default:
			}

			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("handler still running 5s after the client left")
			}
		})
	}
}
