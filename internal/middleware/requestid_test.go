package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestIDOnEveryResponse: the ID must be on the response however the
// handler finishes, since 401s and 502s are what clients most need to quote.
func TestRequestIDOnEveryResponse(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"explicit status", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}},
		{"body without WriteHeader", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "ok")
		}},
		{"nothing written", func(w http.ResponseWriter, r *http.Request) {}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = RequestIDFrom(r.Context())
				tc.handler(w, r)
			})

			rec := httptest.NewRecorder()
			RequestID(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

			got := rec.Result().Header.Values("X-Request-ID")
			if len(got) != 1 || !strings.HasPrefix(got[0], "req_") {
				t.Fatalf("X-Request-ID = %q, want one req_ ID", got)
			}
			if seen != got[0] {
				t.Errorf("RequestIDFrom() = %q, client got %q", seen, got[0])
			}
		})
	}
}

// TestRequestIDIgnoresClientID: a client-sent ID can't be trusted as unique,
// so it never becomes the gateway's ID.
func TestRequestIDIgnoresClientID(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	ids := map[string]bool{}
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Request-ID", "client-chosen")
		rec := httptest.NewRecorder()
		RequestID(inner).ServeHTTP(rec, req)
		ids[rec.Header().Get("X-Request-ID")] = true
	}

	if ids["client-chosen"] {
		t.Error("client's X-Request-ID was used as the gateway's")
	}
	if len(ids) != 2 {
		t.Errorf("two requests got IDs %v, want two distinct IDs", ids)
	}
}

// TestRequestIDReplacesUpstreamID: OpenAI and Groq send their own
// x-request-id, which ReverseProxy adds to the response headers. The client
// must still see exactly one ID, the gateway's.
func TestRequestIDReplacesUpstreamID(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Request-Id", "req_from_openai")
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	RequestID(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

	got := rec.Result().Header.Values("X-Request-ID")
	if len(got) != 1 || got[0] == "req_from_openai" {
		t.Errorf("X-Request-ID = %q, want only the gateway's ID", got)
	}
}

// TestRequestIDSupportsFlush: streamed responses must still flush through the
// wrapper.
func TestRequestIDSupportsFlush(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: chunk\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush() through RequestID failed: %v", err)
		}
	})

	rec := httptest.NewRecorder()
	RequestID(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !rec.Flushed {
		t.Error("response was not flushed")
	}
}

// TestLoggingRecordsRequestIDs: the log line carries the gateway's ID, and the
// client's own one when it sent one.
func TestLoggingRecordsRequestIDs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Request-ID", "client-chosen")
	rec := httptest.NewRecorder()
	RequestID(Logging(logger)(inner)).ServeHTTP(rec, req)

	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log output %q is not valid JSON: %v", logs.String(), err)
	}
	if got, want := entry["request_id"], rec.Header().Get("X-Request-ID"); got != want {
		t.Errorf("logged request_id = %v, want %v", got, want)
	}
	if got := entry["client_request_id"]; got != "client-chosen" {
		t.Errorf("logged client_request_id = %v, want %q", got, "client-chosen")
	}
}
