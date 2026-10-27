package observability

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// PrintfLogger routes a library's Printf-style logger into slog, so its lines
// share the app's format (JSON in production) and carry a level. go-redis is
// the case: without redis.SetLogger it prints "redis: 2026/10/26 ... pool.go:"
// text lines to stderr, which log collectors store with no level. Its
// Printf(ctx, format, v...) signature matches go-redis's Logging interface,
// so this package does not import go-redis.
type PrintfLogger struct {
	logger *slog.Logger
}

// NewPrintfLogger logs through l, tagged with component.
func NewPrintfLogger(l *slog.Logger, component string) *PrintfLogger {
	return &PrintfLogger{logger: l.With("component", component)}
}

// Printf logs at warn. go-redis passes no level: most calls are errors it
// retries or recovers from (dial failures, pool and push-notification errors,
// recovered panics), a few are state changes (sentinel failover, ring shards).
func (p *PrintfLogger) Printf(ctx context.Context, format string, v ...any) {
	p.logger.WarnContext(ctx, strings.TrimSuffix(fmt.Sprintf(format, v...), "\n"))
}
