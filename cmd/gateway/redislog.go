package main

import (
	"context"
	"fmt"
	"log/slog"
)

// redisLogger sends go-redis's own log lines to logger, which would
// otherwise print them to stderr as plain text, such as "failed to dial" while
// Redis is down. Each line becomes one record at Warn, with go-redis's
// formatted text as its message and the field component=go-redis.
//
// Go note: redis.SetLogger takes go-redis's internal.Logging interface. This
// module can't import that package (only code inside go-redis can import an
// internal package), and needn't: a type satisfies an interface by having its
// methods, with no declaration of the fact, so anything with go-redis's
// Printf can be passed. run() passing it to SetLogger is what makes the
// compiler check that it does.
type redisLogger struct{ logger *slog.Logger }

// Printf is go-redis's logging method. WarnContext hands ctx, the one go-redis
// was working under, on to the logger's handler, which can read values from it.
func (l redisLogger) Printf(ctx context.Context, format string, v ...any) {
	l.logger.WarnContext(ctx, fmt.Sprintf(format, v...), "component", "go-redis")
}
