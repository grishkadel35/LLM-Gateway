package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLoggingPreservesResponse is the important one: middleware must be
// invisible to the client. Status, body and headers all pass through unchanged.
func TestLoggingPreservesResponse(t *testing.T) {
	const wantBody = `{"choices":[{"text":"hello"}]}`

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom", "kept")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, wantBody)
	})

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	rec := httptest.NewRecorder()
	Logging(logger)(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/completions", nil))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusCreated)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	if got := res.Header.Get("X-Custom"); got != "kept" {
		t.Errorf("X-Custom = %q, want %q", got, "kept")
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(body) != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
}

// TestLoggingEmitsStructuredFields decodes the emitted log line and checks the
// fields the task calls for: method, path, status, duration.
func TestLoggingEmitsStructuredFields(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	req := httptest.NewRequest(http.MethodDelete, "/v1/files/abc", nil)
	Logging(logger)(inner).ServeHTTP(httptest.NewRecorder(), req)

	// Go note: map[string]any is the generic "any JSON object" type — `any` is
	// an alias for interface{}. Numbers decode as float64.
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log output %q is not valid JSON: %v", logs.String(), err)
	}

	if got := entry["method"]; got != http.MethodDelete {
		t.Errorf("logged method = %v, want %v", got, http.MethodDelete)
	}
	if got := entry["path"]; got != "/v1/files/abc" {
		t.Errorf("logged path = %v, want %v", got, "/v1/files/abc")
	}
	if got := entry["status"]; got != float64(http.StatusTeapot) {
		t.Errorf("logged status = %v, want %v", got, http.StatusTeapot)
	}
	if _, ok := entry["duration_ms"].(float64); !ok {
		t.Errorf("logged duration_ms = %v, want a number", entry["duration_ms"])
	}
}

// TestLoggingDefaultsToStatus200 covers a handler that writes a body without
// ever calling WriteHeader — net/http implies 200, and so must our recorder.
func TestLoggingDefaultsToStatus200(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	Logging(logger)(inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log output %q is not valid JSON: %v", logs.String(), err)
	}
	if got := entry["status"]; got != float64(http.StatusOK) {
		t.Errorf("logged status = %v, want %v", got, http.StatusOK)
	}
	if got := entry["bytes"]; got != float64(2) {
		t.Errorf("logged bytes = %v, want 2", got)
	}
}

// TestLoggingSupportsFlush guards the streaming path. Server-sent events from an
// LLM must reach the client as they arrive; if the wrapper hides Flush, the
// whole stream buffers until the response ends. http.NewResponseController finds
// Flush through the Unwrap method on responseRecorder.
func TestLoggingSupportsFlush(t *testing.T) {
	flushed := false

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: chunk\n\n")

		rc := http.NewResponseController(w)
		if err := rc.Flush(); err != nil {
			t.Errorf("Flush() through middleware failed: %v", err)
			return
		}
		flushed = true
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	Logging(logger)(inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))

	if !flushed {
		t.Error("handler could not flush through the logging middleware")
	}
}
