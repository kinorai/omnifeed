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

	// Logs go to stderr, never stdout: in stdio MCP mode stdout carries the
	// JSON-RPC stream, so anything else on stdout would corrupt the protocol.
	var handler slog.Handler
	if strings.EqualFold(format, "text") {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}
