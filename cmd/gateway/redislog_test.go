package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type ctxKey struct{}

// ctxHandler adds the value the context holds under ctxKey to each record it
// handles, as a handler that reads a request ID or a trace from the context
// would.
type ctxHandler struct{ slog.Handler }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		r.AddAttrs(slog.String("from_context", v))
	}
	return h.Handler.Handle(ctx, r)
}

// TestRedisLoggerWritesOneWarnLine: each message go-redis logs becomes exactly
// one JSON object, at WARN, with go-redis's formatted text as its message, the
// field component=go-redis, and go-redis's context passed on to the handler.
func TestRedisLoggerWritesOneWarnLine(t *testing.T) {
	cases := []struct {
		name, format string
		args         []any
		want         string
	}{
		{"a failed dial", "redis: connection pool: failed to dial after %d attempts: %v",
			[]any{5, errors.New("dial tcp 127.0.0.1:1: connect: connection refused")},
			"redis: connection pool: failed to dial after 5 attempts: dial tcp 127.0.0.1:1: connect: connection refused"},
		{"no arguments", "putConn called with nil connection", nil, "putConn called with nil connection"},
		{"quotes and newlines", "redis: bad reply %q\n%s", []any{`say "hi"`, "one\ntwo"}, "redis: bad reply \"say \\\"hi\\\"\"\none\ntwo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := redisLogger{slog.New(ctxHandler{slog.NewJSONHandler(&buf, nil)})}
			l.Printf(context.WithValue(context.Background(), ctxKey{}, "req_1"), tc.format, tc.args...)

			out := buf.String()
			if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
				t.Fatalf("log = %q, want exactly one line", out)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("the line is not one JSON object: %v: %q", err, out)
			}
			want := map[string]any{"level": "WARN", "msg": tc.want, "component": "go-redis", "from_context": "req_1"}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
			// Nothing else but the time.
			if len(got) != len(want)+1 {
				t.Errorf("the line has fields %v, want only time and %v", got, want)
			}
		})
	}
}
