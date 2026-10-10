package usage

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// forwarded is what the handler after ReadBody saw.
type forwarded struct {
	reached       bool
	body          string
	contentLength int64
	getBody       string
	req           *Request
}

func serveBody(t *testing.T, format provider.Format, maxBytes int64, path, body string) (*httptest.ResponseRecorder, forwarded) {
	t.Helper()

	var got forwarded
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.reached = true
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading forwarded body: %v", err)
		}
		got.body = string(b)
		got.contentLength = r.ContentLength
		if r.GetBody != nil {
			rc, err := r.GetBody()
			if err != nil {
				t.Fatalf("GetBody(): %v", err)
			}
			b, _ := io.ReadAll(rc)
			got.getBody = string(b)
		}
		got.req, _ = RequestFrom(r.Context())
	})

	rec := httptest.NewRecorder()
	ReadBody(format, maxBytes)(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec, got
}

const limit = 1 << 20

// TestReadBodyPassesThroughUntouched: unless the gateway has a reason to
// rewrite a body, the provider receives the client's exact bytes: field
// order, whitespace, unknown fields, and integers too big for float64.
func TestReadBodyPassesThroughUntouched(t *testing.T) {
	cases := []struct {
		name   string
		format provider.Format
		path   string
		body   string
	}{
		{"openai non-streaming", provider.FormatOpenAI, "/v1/chat/completions",
			`{ "seed": 12345678901234567890, "model":"gpt-4o", "future_field": {"a": [1, 2]}, "messages":[] }`},
		{"openai streaming, include_usage already on", provider.FormatOpenAI, "/v1/chat/completions",
			`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"seed":12345678901234567890}`},
		{"anthropic streaming", provider.FormatAnthropic, "/v1/messages",
			`{"model":"claude-sonnet-4","stream":true,"max_tokens":12345678901234567890}`},
		{"gemini streaming", provider.FormatGemini, "/v1beta/models/gemini-3.8-flash:streamGenerateContent",
			`{"contents":[{"parts":[{"text":"hi"}]}]}`},
		{"malformed JSON", provider.FormatOpenAI, "/v1/chat/completions", `{"model":"gpt-4o","stream":tru`},
		{"empty body", provider.FormatOpenAI, "/v1/chat/completions", ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, got := serveBody(t, tc.format, limit, tc.path, tc.body)

			if !got.reached {
				t.Fatalf("not forwarded: status %d, body %s", rec.Code, rec.Body)
			}
			if got.body != tc.body {
				t.Errorf("forwarded body = %q, want the client's bytes %q", got.body, tc.body)
			}
			if got.contentLength != int64(len(tc.body)) {
				t.Errorf("ContentLength = %d, want %d", got.contentLength, len(tc.body))
			}
			if got.getBody != tc.body {
				t.Errorf("GetBody() = %q, want %q", got.getBody, tc.body)
			}
		})
	}
}

// TestReadBodyInjectsIncludeUsage: OpenAI streams carry usage only when
// stream_options.include_usage is set, so it is forced on for every
// OpenAI-format streaming request, whatever the client sent.
func TestReadBodyInjectsIncludeUsage(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no stream_options", `{"model":"gpt-4o","stream":true,"seed":12345678901234567890}`},
		{"include_usage false", `{"model":"gpt-4o","stream":true,"seed":12345678901234567890,"stream_options":{"include_usage":false,"include_obfuscation":false}}`},
		{"stream_options null", `{"model":"gpt-4o","stream":true,"seed":12345678901234567890,"stream_options":null}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := serveBody(t, provider.FormatOpenAI, limit, "/v1/chat/completions", tc.body)

			var sent map[string]json.RawMessage
			if err := json.Unmarshal([]byte(got.body), &sent); err != nil {
				t.Fatalf("forwarded body %q is not JSON: %v", got.body, err)
			}
			var opts map[string]json.RawMessage
			if err := json.Unmarshal(sent["stream_options"], &opts); err != nil {
				t.Fatalf("stream_options = %s: %v", sent["stream_options"], err)
			}
			if string(opts["include_usage"]) != "true" {
				t.Errorf("include_usage = %s, want true", opts["include_usage"])
			}
			if strings.Contains(tc.body, "include_obfuscation") && string(opts["include_obfuscation"]) != "false" {
				t.Errorf("client's other stream_options were lost: %s", sent["stream_options"])
			}
			// Untouched fields keep their exact bytes, big integers included.
			if string(sent["seed"]) != "12345678901234567890" || string(sent["model"]) != `"gpt-4o"` {
				t.Errorf("seed = %s, model = %s; want them unchanged", sent["seed"], sent["model"])
			}
			if got.contentLength != int64(len(got.body)) || got.getBody != got.body {
				t.Errorf("ContentLength = %d, GetBody = %q; want %d and the rewritten body", got.contentLength, got.getBody, len(got.body))
			}
		})
	}
}

// TestReadBodyLeavesInvalidStreamOptions: a stream_options that isn't an
// object is the client's error to get from the provider, not something to
// silently repair.
func TestReadBodyLeavesInvalidStreamOptions(t *testing.T) {
	body := `{"model":"gpt-4o","stream":true,"stream_options":"yes"}`
	_, got := serveBody(t, provider.FormatOpenAI, limit, "/v1/chat/completions", body)
	if got.body != body {
		t.Errorf("forwarded body = %q, want it unchanged", got.body)
	}
}

func TestReadBodyRejectsOversizedBody(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[]}`

	rec, got := serveBody(t, provider.FormatOpenAI, int64(len(body))-1, "/v1/chat/completions", body)
	if got.reached {
		t.Error("oversized body was forwarded")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if !strings.Contains(rec.Body.String(), "request_too_large") {
		t.Errorf("body = %s, want error type request_too_large", rec.Body)
	}

	// Exactly at the limit is fine.
	if _, got := serveBody(t, provider.FormatOpenAI, int64(len(body)), "/v1/chat/completions", body); !got.reached {
		t.Error("body exactly at the limit was refused")
	}
}

// TestReadBodyExposesModelAndStream: later steps (pricing, limits) need the
// model and whether the response will stream, wherever the format keeps them.
func TestReadBodyExposesModelAndStream(t *testing.T) {
	cases := []struct {
		name       string
		format     provider.Format
		path, body string
		model      string
		stream     bool
	}{
		{"openai", provider.FormatOpenAI, "/v1/chat/completions", `{"model":"gpt-4o","stream":true}`, "gpt-4o", true},
		{"openai non-streaming", provider.FormatOpenAI, "/v1/chat/completions", `{"model":"gpt-4o"}`, "gpt-4o", false},
		{"anthropic", provider.FormatAnthropic, "/v1/messages", `{"model":"claude-sonnet-4","stream":false}`, "claude-sonnet-4", false},
		// Gemini keeps both in the URL, not the body.
		{"gemini", provider.FormatGemini, "/v1beta/models/gemini-3.8-flash:generateContent", `{}`, "gemini-3.8-flash", false},
		{"gemini streaming", provider.FormatGemini, "/v1beta/models/gemini-3.8-flash:streamGenerateContent", `{}`, "gemini-3.8-flash", true},
		{"malformed", provider.FormatOpenAI, "/v1/chat/completions", `{"model":`, "", false},
		{"wrong types", provider.FormatOpenAI, "/v1/chat/completions", `{"model":42,"stream":"yes"}`, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := serveBody(t, tc.format, limit, tc.path, tc.body)
			if got.req == nil {
				t.Fatal("RequestFrom() found nothing")
			}
			if got.req.Model != tc.model || got.req.Stream != tc.stream {
				t.Errorf("Model, Stream = %q, %v; want %q, %v", got.req.Model, got.req.Stream, tc.model, tc.stream)
			}
		})
	}
}

// TestReadBodyRecordsPathAndTime: the usage row's endpoint is the path after
// the provider prefix, and its latency and created_at count from arrival.
func TestReadBodyRecordsPathAndTime(t *testing.T) {
	before := time.Now()
	_, got := serveBody(t, provider.FormatOpenAI, limit, "/v1/chat/completions", `{"model":"gpt-4o"}`)

	if got.req.Path != "/v1/chat/completions" {
		t.Errorf("Path = %q, want /v1/chat/completions", got.req.Path)
	}
	if got.req.Received.Before(before) || got.req.Received.After(time.Now()) {
		t.Errorf("Received = %v, want the time ReadBody ran", got.req.Received)
	}
}

// TestReadBodyEstimatesRequest: rate limiting needs a request's input size and
// output limit before the provider answers, wherever each format keeps them.
// Estimating never rewrites the body.
func TestReadBodyEstimatesRequest(t *testing.T) {
	openAI := `{"model":"gpt-4o","max_tokens":100,"n":2,"messages":[{"role":"user","content":"hi"}]}`
	anthropic := `{"model":"claude-sonnet-4","max_tokens":512,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}`
	gemini := `{"contents":[{"parts":[{"text":"hi"}]}],"generation_config":{"max_output_tokens":300,"candidate_count":2}}`
	malformed := `{"model":"gpt-4o","max_tokens":100`

	cases := []struct {
		name       string
		format     provider.Format
		path, body string
		input      int64
		outputCap  int64
	}{
		{"openai", provider.FormatOpenAI, "/v1/chat/completions", openAI, ceilQuarter(len(openAI)), 200},
		// The image's payload is left out of the /4, and the part costs a flat
		// 1,600 tokens.
		{"anthropic with an image", provider.FormatAnthropic, "/v1/messages", anthropic,
			1600 + ceilQuarter(len(anthropic)-len(`{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}`)), 512},
		{"gemini", provider.FormatGemini, "/v1beta/models/gemini-3.8-flash:generateContent", gemini, ceilQuarter(len(gemini)), 600},
		{"malformed", provider.FormatOpenAI, "/v1/chat/completions", malformed, ceilQuarter(len(malformed)), 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := serveBody(t, tc.format, limit, tc.path, tc.body)
			if got.req == nil {
				t.Fatal("RequestFrom() found nothing")
			}
			if got.req.InputEstimate != tc.input || got.req.OutputCap != tc.outputCap {
				t.Errorf("InputEstimate, OutputCap = %d, %d; want %d, %d", got.req.InputEstimate, got.req.OutputCap, tc.input, tc.outputCap)
			}
			if got.body != tc.body {
				t.Errorf("forwarded body = %q, want the client's bytes %q", got.body, tc.body)
			}
		})
	}
}

// TestReadBodyEstimatesClientBytes: forcing include_usage on adds bytes to the
// forwarded body but nothing to the prompt, so the estimate counts what the
// client sent.
func TestReadBodyEstimatesClientBytes(t *testing.T) {
	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	_, got := serveBody(t, provider.FormatOpenAI, limit, "/v1/chat/completions", body)

	if ceilQuarter(len(got.body)) == ceilQuarter(len(body)) {
		t.Fatalf("forwarded body is %d bytes against the client's %d; want the include_usage rewrite to make a difference here", len(got.body), len(body))
	}
	if want := ceilQuarter(len(body)); got.req.InputEstimate != want {
		t.Errorf("InputEstimate = %d, want %d, counted from the client's %d bytes", got.req.InputEstimate, want, len(body))
	}
}
