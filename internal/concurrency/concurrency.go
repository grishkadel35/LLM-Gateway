// Package concurrency caps how many requests a provider has in flight, for an
// upstream that can only serve a few at once, such as a local Ollama.
package concurrency

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// ErrBusy is the error for a request that waited its whole queue timeout
// without a slot coming free.
var ErrBusy = errors.New("no concurrency slot came free within the queue timeout")

// Limit wraps next so that at most n requests are in flight through it at
// once. A request beyond that waits for a slot, up to queueTimeout, and then
// fails with ErrBusy. observe, if not nil, receives every wait, granted or
// not.
//
// It is a RoundTripper, not handler middleware, so a slot covers exactly the
// exchange with the upstream: it is taken just before the request is sent and
// freed when the response body is closed, which for a stream is when the
// stream ends. The wait doesn't count against next's response-header timeout,
// which starts once the request is sent.
func Limit(next http.RoundTripper, n int, queueTimeout time.Duration, observe func(wait time.Duration)) http.RoundTripper {
	return &limiter{
		next:    next,
		slots:   semaphore.NewWeighted(int64(n)),
		timeout: queueTimeout,
		observe: observe,
	}
}

// limiter is the RoundTripper Limit returns.
//
// Go note: the slots are a semaphore.Weighted rather than a buffered channel.
// Weighted serves its waiters in the order they arrived, and an Acquire whose
// context ends returns holding nothing. A select on a channel promises no
// order among the goroutines waiting on it.
type limiter struct {
	next    http.RoundTripper
	slots   *semaphore.Weighted
	timeout time.Duration
	observe func(time.Duration)
}

func (l *limiter) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(req.Context(), l.timeout)
	err := l.slots.Acquire(ctx, 1)
	cancel()
	if l.observe != nil {
		l.observe(time.Since(start))
	}
	if err != nil {
		// A RoundTripper closes the request body, even when it fails.
		if req.Body != nil {
			req.Body.Close()
		}
		// The client gave up while waiting: that is the error, not a busy
		// provider.
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		return nil, ErrBusy
	}

	resp, err := l.next.RoundTrip(req)
	if err != nil {
		l.slots.Release(1)
		return nil, err
	}
	resp.Body = &slotBody{ReadCloser: resp.Body, release: func() { l.slots.Release(1) }}
	return resp, nil
}

// slotBody frees its request's slot when the response body is closed.
//
// The release lives in Close because Close is what always runs, panics
// included: ReverseProxy closes the body after copying it, before handing a
// ModifyResponse error to its ErrorHandler, and in a defer when it aborts a
// stream with panic(http.ErrAbortHandler). sync.Once makes a second Close
// harmless, where a second Release would panic.
type slotBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *slotBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
