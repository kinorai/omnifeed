package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/kinorai/omnifeed/internal/config"
	"github.com/kinorai/omnifeed/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"
)

// Each configured dependency gets a probe, an unreachable one reads 0, and
// any HTTP answer (even an error status) reads 1. Unconfigured dependencies
// get no series at all.
func TestDependencyChecks(t *testing.T) {
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // reachable: still "up"
	}))
	defer searx.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := config.Config{Crawl4AIURL: deadURL, SearXNGURL: searx.URL}
	checks := dependencyChecks(cfg, rdb, true)
	if len(checks) != 3 {
		t.Fatalf("checks = %d, want crawl4ai, searxng, redis", len(checks))
	}
	m := observability.NewMetrics()
	(&observability.DependencyChecker{Checks: checks, Metrics: m,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).CheckOnce(context.Background())
	for dep, want := range map[string]float64{"crawl4ai": 0, "searxng": 1, "redis": 1} {
		if got := gaugeValue(t, m.DependencyUp.WithLabelValues(dep)); got != want {
			t.Errorf("omnifeed_dependency_up{%s} = %v, want %v", dep, got, want)
		}
	}

	if got := dependencyChecks(cfg, nil, false); len(got) != 1 || got["crawl4ai"] == nil {
		t.Fatalf("without search and redis: checks = %v, want crawl4ai only", got)
	}
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var dm dto.Metric
	if err := g.Write(&dm); err != nil {
		t.Fatal(err)
	}
	return dm.GetGauge().GetValue()
}
