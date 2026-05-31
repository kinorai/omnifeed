// Package observability wires structured logging, Prometheus metrics, and
// Kubernetes-style health endpoints.
package observability

import (
	"log/slog"
	"os"
	"strings"
)

// NewLogger returns a slog.Logger configured per the given level and format.
// Level: debug | info | warn | error (default info).
// Format: json | text (default json).
func NewLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	if strings.EqualFold(format, "text") {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
