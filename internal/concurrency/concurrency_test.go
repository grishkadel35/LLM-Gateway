package concurrency

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var errUnreachable = errors.New("connection refused")

// upstream is a fake inner transport. It answers every request with a 200,
// except a request for /unreachable, which fails as if nothing listened.
func upstream(calls *atomic.Int32) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Path == "/unreachable" {
			return nil, errUnreachable
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})
}

// closeRecorder is a request body that records whether it was closed.
type closeRecorder struct {
	io.Reader
	closed atomic.Bool
}

func (b *closeRecorder) Close() error {
	b.closed.Store(true)
	return nil
}

// newRequest returns a request for path whose context ends after timeout.
func newRequest(t *testing.T, path string, timeout time.Duration) (*http.Request, *closeRecorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	body := &closeRecorder{Reader: strings.NewReader(`{"model":"qwen3.5:9b"}`)}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://ollama.test"+path, body)
	if err != nil {
		t.Fatal(err)
	}
	return req, body
}

// wantOneFreeSlot fails the test unless l has exactly one slot free: one
// request gets it, and a second can't get another.
func wantOneFreeSlot(t *testing.T, l http.RoundTripper) {
	t.Helper()
	req, _ := newRequest(t, "/v1/chat/completions", 5*time.Second)
	resp, err := l.RoundTrip(req)
	if err != nil {
		t.Fatalf("no slot free: %v", err)
	}
	defer resp.Body.Close()

	req, _ = newRequest(t, "/v1/chat/completions", 20*time.Millisecond)
	if resp, err := l.RoundTrip(req); err == nil {
		resp.Body.Close()
		t.Fatal("a second slot was free: one was released twice")
	}
}

// TestLimitQueuesUntilTheBodyCloses: with one slot, a second request waits
// until the first one's response body is closed, then goes through.
func TestLimitQueuesUntilTheBodyCloses(t *testing.T) {
	var calls atomic.Int32
	l := Limit(upstream(&calls), 1, time.Minute, nil)

	req, _ := newRequest(t, "/v1/chat/completions", 5*time.Second)
	first, err := l.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	req, _ = newRequest(t, "/v1/chat/completions", 5*time.Second)
	go func() {
		resp, err := l.RoundTrip(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()

	select {
	case <-done:
		t.Fatal("the second request went through while the first held the only slot")
	case <-time.After(50 * time.Millisecond):
	}

	first.Body.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second request: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second request never got the freed slot")
	}
	if calls.Load() != 2 {
		t.Errorf("the upstream got %d requests, want 2", calls.Load())
	}
}

// TestLimitFreesTheSlot: however a request that got a slot ends, the slot is
// freed, and only once.
func TestLimitFreesTheSlot(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		closes  int // how often the response body is closed
		wantErr error
	}{
		{"body closed", "/v1/chat/completions", 1, nil},
		{"body closed twice", "/v1/chat/completions", 2, nil},
		{"upstream unreachable", "/unreachable", 0, errUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			l := Limit(upstream(&calls), 1, time.Minute, nil)

			req, _ := newRequest(t, tc.path, 5*time.Second)
			resp, err := l.RoundTrip(req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("RoundTrip() error = %v, want %v", err, tc.wantErr)
			}
			for range tc.closes {
				resp.Body.Close()
			}

			wantOneFreeSlot(t, l)
		})
	}
}

// TestLimitRefusesAWait: a request that can't get a slot fails without
// reaching the upstream, closes its body, has its wait observed, and leaves
// no slot taken.
func TestLimitRefusesAWait(t *testing.T) {
	cases := []struct {
		name         string
		queueTimeout time.Duration
		clientGone   time.Duration // when the client gives up; 0 if it waits
		wantErr      error
	}{
		{"queue timeout", 20 * time.Millisecond, 0, ErrBusy},
		{"client gone while waiting", time.Minute, 20 * time.Millisecond, context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var waits []time.Duration
			l := Limit(upstream(&calls), 1, tc.queueTimeout, func(d time.Duration) { waits = append(waits, d) })

			// Another request holds the only slot.
			req, _ := newRequest(t, "/v1/chat/completions", 5*time.Second)
			holder, err := l.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.clientGone > 0 {
				time.AfterFunc(tc.clientGone, cancel)
			}
			body := &closeRecorder{Reader: strings.NewReader("{}")}
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, "http://ollama.test/v1/chat/completions", body)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := l.RoundTrip(req); !errors.Is(err, tc.wantErr) {
				t.Errorf("RoundTrip() error = %v, want %v", err, tc.wantErr)
			}
			if !body.closed.Load() {
				t.Error("the request body was not closed")
			}
			if calls.Load() != 1 {
				t.Errorf("the upstream got %d requests, want only the holder's", calls.Load())
			}
			if len(waits) != 2 || waits[1] < 20*time.Millisecond {
				t.Errorf("waits = %v, want the holder's and then one of at least 20ms", waits)
			}

			holder.Body.Close()
			wantOneFreeSlot(t, l)
		})
	}
}
