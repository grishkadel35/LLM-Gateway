package usage

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"sync"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// Result is what Meter reports for one response.
type Result struct {
	Usage
	// Status is the HTTP status the provider returned, which the client
	// receives unchanged.
	Status int
	// Streamed reports whether the response was streamed: a server-sent
	// event stream, or a stream the client asked for in another shape (such
	// as Gemini's default JSON array).
	Streamed bool
	// Complete is false when the body was closed before EOF, typically
	// because the client disconnected mid-stream: Usage then holds only what
	// had been seen.
	Complete bool
	// ProviderRequestID is the provider's own ID for the request, which its
	// support asks for. "" if it sent none.
	ProviderRequestID string
}

// Callback receives a response's Result. ctx is the request's context, which
// carries the tenant and request ID. It runs on the proxy's goroutine, so it
// must not block.
type Callback func(ctx context.Context, r Result)

// maxBufferedBody bounds how much of a non-streaming body is kept for parsing
// at EOF. Past it the bytes still reach the client, but usage is not read.
const maxBufferedBody = 32 << 20

// Meter wraps resp.Body so that the response's usage is read as its bytes
// pass through to the client, untouched and unbuffered. done runs exactly
// once: at EOF, or when the body is closed first.
//
// A text/event-stream body is parsed event by event. Anything else is parsed
// once complete, which also covers JSON streamed without SSE, such as
// Gemini's default JSON-array stream.
//
// When the response doesn't name its model (error responses rarely do), the
// model the request asked for is reported instead.
func Meter(resp *http.Response, format provider.Format, done Callback) {
	providerID := resp.Header.Get("X-Request-Id") // OpenAI, Groq
	if providerID == "" {
		providerID = resp.Header.Get("Request-Id") // Anthropic
	}

	ctx := resp.Request.Context()
	sse := isEventStream(resp)
	req, _ := RequestFrom(ctx)
	if req == nil {
		req = &Request{}
	}

	resp.Body = &meteredBody{
		body:   resp.Body,
		parser: newParser(format),
		done:   done,
		ctx:    ctx,
		sse:    sse,
		req:    req,
		result: Result{
			Status:            resp.StatusCode,
			Streamed:          sse || req.Stream,
			ProviderRequestID: providerID,
		},
	}
}

type meteredBody struct {
	body   io.ReadCloser
	parser parser // nil: the format has no parser, report no usage
	done   Callback
	ctx    context.Context
	sse    bool     // parse event by event, rather than the whole body at EOF
	req    *Request // what ReadBody saw of the request
	result Result

	// buf holds an unfinished SSE line, or the whole body so far for
	// non-streaming responses. data accumulates the current SSE event.
	buf      []byte
	data     []byte
	overflow bool

	once sync.Once
}

func (m *meteredBody) Read(p []byte) (int, error) {
	n, err := m.body.Read(p)
	if n > 0 && m.parser != nil {
		m.feed(p[:n])
	}
	if err == io.EOF {
		m.finish(true)
	}
	return n, err
}

func (m *meteredBody) Close() error {
	err := m.body.Close()
	m.finish(false)
	return err
}

// feed takes the next bytes of the body.
func (m *meteredBody) feed(b []byte) {
	if m.overflow {
		return
	}
	if len(m.buf)+len(b) > maxBufferedBody {
		m.overflow, m.buf, m.data = true, nil, nil
		return
	}
	m.buf = append(m.buf, b...)
	if !m.sse {
		return
	}

	// Handle every complete line; keep the unfinished tail for next time.
	for {
		i := bytes.IndexByte(m.buf, '\n')
		if i < 0 {
			break
		}
		m.line(m.buf[:i])
		m.buf = m.buf[i+1:]
	}
	m.buf = append([]byte(nil), m.buf...)
}

// line handles one SSE line. Only data lines matter: every provider repeats
// the event type inside the JSON payload, so "event:" lines can be ignored.
func (m *meteredBody) line(l []byte) {
	l = bytes.TrimSuffix(l, []byte("\r"))
	switch {
	case len(l) == 0:
		m.dispatch()
	case bytes.HasPrefix(l, []byte("data:")):
		v := bytes.TrimPrefix(l[len("data:"):], []byte(" "))
		if len(m.data) > 0 {
			m.data = append(m.data, '\n')
		}
		m.data = append(m.data, v...)
	}
}

// dispatch hands the accumulated event data to the parser.
func (m *meteredBody) dispatch() {
	if len(m.data) > 0 {
		m.parser.event(m.data)
		m.data = m.data[:0]
	}
}

// finish reports the result, once.
func (m *meteredBody) finish(complete bool) {
	m.once.Do(func() {
		if m.parser != nil && !m.overflow {
			if m.sse {
				// A last event the stream didn't terminate with a blank line.
				if len(m.buf) > 0 {
					m.line(m.buf)
				}
				m.dispatch()
			} else {
				m.parser.body(m.buf)
			}
			m.result.Usage = m.parser.usage()
		}
		if m.result.Model == "" {
			m.result.Model = m.req.Model
		}
		m.buf, m.data = nil, nil
		m.result.Complete = complete
		m.done(m.ctx, m.result)
	})
}

// isEventStream reports whether resp is a server-sent event stream.
func isEventStream(resp *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && mediaType == "text/event-stream"
}
