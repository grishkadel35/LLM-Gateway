package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/concurrency"
	"github.com/grishkadel/llm-gateway/internal/provider"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// ollamaAnswering returns a func that starts a fake Ollama answering every
// request with status, and returns its URL.
func ollamaAnswering(status int) func(*testing.T) string {
	return func(t *testing.T) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if status == http.StatusOK {
				_, _ = io.WriteString(w, `{"model":"qwen3.5:9b","choices":[],"usage":{"prompt_tokens":19,"completion_tokens":1}}`)
				return
			}
			_, _ = io.WriteString(w, `{"error":{"message":"ollama failed"}}`)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
}

// ollamaUnreachable returns the URL of a fake Ollama that has shut down.
func ollamaUnreachable(t *testing.T) string {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

// ollamaHanging starts a fake Ollama that doesn't answer while the test runs,
// and returns its URL.
func ollamaHanging(t *testing.T) string {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	// Cleanups run last-registered first: the handler is released before
	// Close waits for it.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv.URL
}

// fakeGroq is the fallback target. It answers every request with its status
// and records the last request it got.
type fakeGroq struct {
	server *httptest.Server
	calls  int
	auth   string
	body   []byte
}

func newFakeGroq(t *testing.T, status int) *fakeGroq {
	t.Helper()
	f := &fakeGroq{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.auth = r.Header.Get("Authorization")
		f.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"model":"qwen/qwen3.8-27b","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// meteredCall is one usage callback, with the provider whose proxy made it.
type meteredCall struct {
	provider string
	result   usage.Result
}

// fallbackRig is ollama, at ollamaURL, falling back to groq for qwen3.5:9b,
// inside ReadBody, as router wires them. It records each usage callback and
// each fallback.
type fallbackRig struct {
	handler   http.Handler
	ollama    *httputil.ReverseProxy
	calls     []meteredCall
	fallbacks []string // "from to reason"
}

func newFallbackRig(t *testing.T, ollamaURL string, ollamaTimeout time.Duration, groq *fakeGroq) *fallbackRig {
	t.Helper()

	rig := &fallbackRig{}
	meter := func(name string) usage.Callback {
		return func(_ context.Context, r usage.Result) { rig.calls = append(rig.calls, meteredCall{name, r}) }
	}

	ollama := testProvider(t, ollamaURL)
	ollama.Name, ollama.Auth, ollama.Format, ollama.Timeout = "ollama", provider.AuthNone, provider.FormatOpenAI, ollamaTimeout
	target := testProvider(t, groq.server.URL)
	target.Name, target.Key, target.Format = "groq", "sk-groq", provider.FormatOpenAI

	rig.ollama = New(ollama, discardLogger(), meter("ollama"))
	h := Fallback("ollama", rig.ollama, "groq", New(target, discardLogger(), meter("groq")),
		map[string]string{"qwen3.5:9b": "qwen/qwen3.8-27b"},
		func(from, to, reason string) { rig.fallbacks = append(rig.fallbacks, from+" "+to+" "+reason) },
		discardLogger())
	rig.handler = usage.ReadBody(provider.FormatOpenAI, 1<<20)(h)
	return rig
}

// chatBody is a chat request body for model.
func chatBody(model string, stream bool) string {
	return `{"model":"` + model + `","stream":` + strconv.FormatBool(stream) + `,"temperature":0.2,"messages":[{"role":"user","content":"ping"}]}`
}

// chat is a tenant's chat request for model.
func chat(model string, stream bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody(model, stream)))
	req.Header.Set("Authorization", "Bearer gw_tenant-key")
	return req
}

// TestFallbackDecisions: what each outcome at ollama does to a request that
// can fall back, being non-streaming and for a mapped model.
func TestFallbackDecisions(t *testing.T) {
	cases := []struct {
		name             string
		ollama           func(*testing.T) string
		wantStatus       int
		wantOllamaStatus int    // the status in ollama's usage callback
		wantReason       string // "" if the request must not fall back
	}{
		{"answers 200", ollamaAnswering(http.StatusOK), http.StatusOK, http.StatusOK, ""},
		{"answers 400", ollamaAnswering(http.StatusBadRequest), http.StatusBadRequest, http.StatusBadRequest, ""},
		{"answers 404", ollamaAnswering(http.StatusNotFound), http.StatusNotFound, http.StatusNotFound, ""},
		{"answers 500", ollamaAnswering(http.StatusInternalServerError), http.StatusOK, http.StatusInternalServerError, reasonServerError},
		{"answers 503", ollamaAnswering(http.StatusServiceUnavailable), http.StatusOK, http.StatusServiceUnavailable, reasonServerError},
		{"unreachable", ollamaUnreachable, http.StatusOK, http.StatusBadGateway, reasonUnreachable},
		{"times out", ollamaHanging, http.StatusOK, http.StatusGatewayTimeout, reasonTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groq := newFakeGroq(t, http.StatusOK)
			rig := newFallbackRig(t, tc.ollama(t), 100*time.Millisecond, groq)

			rec := httptest.NewRecorder()
			rig.handler.ServeHTTP(rec, chat("qwen3.5:9b", false))

			fellBack := tc.wantReason != ""
			wantProvider, wantFallback, wantFallbacks := "ollama", "false", []string(nil)
			if fellBack {
				wantProvider, wantFallback, wantFallbacks = "groq", "true", []string{"ollama groq " + tc.wantReason}
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if got := rec.Header().Get(HeaderProvider); got != wantProvider {
				t.Errorf("%s = %q, want %q", HeaderProvider, got, wantProvider)
			}
			if got := rec.Header().Get(HeaderFallback); got != wantFallback {
				t.Errorf("%s = %q, want %q", HeaderFallback, got, wantFallback)
			}
			if !reflect.DeepEqual(rig.fallbacks, wantFallbacks) {
				t.Errorf("fallbacks = %q, want %q", rig.fallbacks, wantFallbacks)
			}
			if fellBack != (groq.calls == 1) {
				t.Errorf("groq got %d requests", groq.calls)
			}

			// Each attempt reports its own usage, ollama's first.
			if len(rig.calls) == 0 || rig.calls[0].provider != "ollama" || rig.calls[0].result.Status != tc.wantOllamaStatus {
				t.Fatalf("usage callbacks = %+v, want ollama's first, status %d", rig.calls, tc.wantOllamaStatus)
			}
			if tc.wantReason == reasonServerError && !rig.calls[0].result.Complete {
				t.Error("ollama's 5xx was not read to the end, so its usage is incomplete")
			}
			if !fellBack {
				if len(rig.calls) != 1 {
					t.Errorf("usage callbacks = %+v, want only ollama's", rig.calls)
				}
				return
			}
			if len(rig.calls) != 2 || rig.calls[1].provider != "groq" || rig.calls[1].result.Status != http.StatusOK ||
				rig.calls[1].result.Model != "qwen/qwen3.8-27b" {
				t.Errorf("usage callbacks = %+v, want ollama's, then groq's for qwen/qwen3.8-27b", rig.calls)
			}
		})
	}
}

// TestFallbackNeverFor: requests that never fall back, whatever ollama does.
// They still carry the headers saying ollama answered.
func TestFallbackNeverFor(t *testing.T) {
	cases := []struct {
		name       string
		ollama     func(*testing.T) string
		model      string
		stream     bool
		busy       bool // ollama's only concurrency slot is taken
		clientGone bool // the client gives up after 50ms
		wantStatus int
	}{
		{"streaming", ollamaAnswering(http.StatusInternalServerError), "qwen3.5:9b", true, false, false, http.StatusInternalServerError},
		{"unmapped model", ollamaAnswering(http.StatusInternalServerError), "llama3.2:1b", false, false, false, http.StatusInternalServerError},
		{"busy", ollamaAnswering(http.StatusOK), "qwen3.5:9b", false, true, false, http.StatusServiceUnavailable},
		{"client gone", ollamaHanging, "qwen3.5:9b", false, false, true, StatusClientClosedRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groq := newFakeGroq(t, http.StatusOK)
			url := tc.ollama(t)
			rig := newFallbackRig(t, url, 5*time.Second, groq)

			req := chat(tc.model, tc.stream)
			if tc.busy {
				rig.ollama.Transport = concurrency.Limit(rig.ollama.Transport, 1, 20*time.Millisecond, nil)
				// Another request holds the only slot until the test ends.
				out, err := http.NewRequest(http.MethodGet, url, nil)
				if err != nil {
					t.Fatal(err)
				}
				held, err := rig.ollama.Transport.RoundTrip(out)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { held.Body.Close() })
			}
			if tc.clientGone {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				time.AfterFunc(50*time.Millisecond, cancel)
				req = req.WithContext(ctx)
			}

			rec := httptest.NewRecorder()
			rig.handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if rec.Header().Get(HeaderProvider) != "ollama" || rec.Header().Get(HeaderFallback) != "false" {
				t.Errorf("%s = %q, %s = %q; want ollama, false", HeaderProvider, rec.Header().Get(HeaderProvider),
					HeaderFallback, rec.Header().Get(HeaderFallback))
			}
			if groq.calls != 0 || len(rig.fallbacks) != 0 {
				t.Errorf("fell back: groq got %d requests, fallbacks %q", groq.calls, rig.fallbacks)
			}
		})
	}
}

// TestFallbackRewritesOnlyTheModel: groq gets the client's request with only
// the model swapped, under groq's own key, and the client gets groq's answer.
func TestFallbackRewritesOnlyTheModel(t *testing.T) {
	groq := newFakeGroq(t, http.StatusOK)
	rig := newFallbackRig(t, ollamaAnswering(http.StatusInternalServerError)(t), 5*time.Second, groq)

	rec := httptest.NewRecorder()
	rig.handler.ServeHTTP(rec, chat("qwen3.5:9b", false))

	var got, want map[string]any
	if err := json.Unmarshal(groq.body, &got); err != nil {
		t.Fatalf("groq's body %s: %v", groq.body, err)
	}
	if err := json.Unmarshal([]byte(chatBody("qwen3.5:9b", false)), &want); err != nil {
		t.Fatal(err)
	}
	want["model"] = "qwen/qwen3.8-27b"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groq got %v, want %v", got, want)
	}
	if groq.auth != "Bearer sk-groq" {
		t.Errorf("groq's Authorization = %q, want its own key", groq.auth)
	}
	if rec.Code != http.StatusOK || !jsonHas(rec.Body.Bytes(), "model", "qwen/qwen3.8-27b") {
		t.Errorf("client got %d %s, want groq's answer", rec.Code, rec.Body)
	}
}

// TestFallbackTargetFails: when groq fails too, the client gets groq's
// failure, as from any provider. Were the retry armed, groq's proxy would
// swallow its own failure and the client would get an empty 200.
func TestFallbackTargetFails(t *testing.T) {
	cases := []struct {
		name       string
		groqStatus int  // what groq answers
		groqDown   bool // groq is unreachable instead
		wantStatus int
	}{
		{"groq answers 500", http.StatusInternalServerError, false, http.StatusInternalServerError},
		{"groq unreachable", http.StatusOK, true, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groq := newFakeGroq(t, tc.groqStatus)
			if tc.groqDown {
				groq.server.Close()
			}
			rig := newFallbackRig(t, ollamaAnswering(http.StatusInternalServerError)(t), 5*time.Second, groq)

			rec := httptest.NewRecorder()
			rig.handler.ServeHTTP(rec, chat("qwen3.5:9b", false))

			if rec.Code != tc.wantStatus || rec.Body.Len() == 0 {
				t.Errorf("status = %d, body %q; want %d with groq's error", rec.Code, rec.Body, tc.wantStatus)
			}
			if rec.Header().Get(HeaderProvider) != "groq" || rec.Header().Get(HeaderFallback) != "true" {
				t.Errorf("%s = %q, %s = %q; want groq, true", HeaderProvider, rec.Header().Get(HeaderProvider),
					HeaderFallback, rec.Header().Get(HeaderFallback))
			}
		})
	}
}

// jsonHas reports whether body is a JSON object whose field key is the string
// want.
func jsonHas(body []byte, key, want string) bool {
	var m map[string]any
	return json.Unmarshal(body, &m) == nil && m[key] == want
}
