package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/config"
	"github.com/kinorai/omnifeed/internal/observability"
)

// searxngLimiter returns the httpx.Limiter interface, so an unconfigured
// deployment must get a LITERAL nil back. A nil *DomainLimiter stored in the
// interface is non-nil, and every call site tests `limiter != nil` to decide
// whether pacing is on — a typed nil would turn pacing "off" into a nil-pointer
// panic on the first query.
func TestSearxngLimiterUnconfiguredIsNil(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := observability.NewMetrics()

	tests := []struct {
		name    string
		cfg     config.Config
		wantNil bool
	}{
		{
			name:    "neither control set",
			cfg:     config.Config{},
			wantNil: true,
		},
		{
			name:    "delay set",
			cfg:     config.Config{SearXNGDelay: time.Second},
			wantNil: false,
		},
		{
			name:    "quota set",
			cfg:     config.Config{SearXNGQuota: 14, SearXNGQuotaWindow: 90 * time.Second},
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := searxngLimiter(tt.cfg, metrics, logger)
			if (got == nil) != tt.wantNil {
				t.Fatalf("searxngLimiter() == nil is %v, want %v (got %#v)", got == nil, tt.wantNil, got)
			}
		})
	}
}
