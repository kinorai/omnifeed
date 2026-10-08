package searxng

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kinorai/omnifeed/internal/httpx"
)

// Bounds of the background /config fetch (see InitEngineMetrics). SearXNG may
// come up after omnifeed — same deploy, slower start — so the fetch retries
// with a doubling backoff, and gives up after initBudget: past that, the lazy
// path (an engine's series is minted the first time a response names it) is
// all there is.
const (
	initBackoff    = time.Second
	initMaxBackoff = 30 * time.Second
	initBudget     = 5 * time.Minute
)

// engineErrorTypes is every value normalizeEngineError can return, plus
// "unknown" (countUnresponsive's label for a missing error type). It is the
// closed vocabulary of the `error` label, so an engine's unresponsive series
// can be minted at zero for all of them.
var engineErrorTypes = []string{
	"captcha", "too_many_requests", "access_denied", "timeout", "suspended", "error", "unknown",
}

// initEngineMetrics mints every per-engine series for the given engines at
// zero. Prometheus' increase()/rate() cannot count a counter's FIRST sample,
// so a series that is born at 20 on a pod's first search reads as no increase
// at all — after every restart, a pod's first results per engine were
// invisible, and an "engine went silent" alert fired on a healthy engine.
// A series that already exists at 0 makes the first real increment count.
func (s *Searcher) initEngineMetrics(engines []string) {
	if s.metrics == nil {
		return
	}
	for _, engine := range engines {
		if engine = strings.TrimSpace(engine); engine != "" {
			s.metrics.InitSearxngEngine(engine, engineErrorTypes)
		}
	}
}

// configResponse is the slice of SearXNG's GET /config this package reads.
// Engines is a pointer so a response without the key is told apart from an
// instance with no engines.
type configResponse struct {
	Engines *[]struct {
		Name    string `json:"name"`
		Enabled *bool  `json:"enabled"`
	} `json:"engines"`
}

// errConfigUnusable marks a /config answer retrying will not change — a body
// this package cannot read, or a 4xx other than 429 — so it ends the fetch at
// once.
var errConfigUnusable = errors.New("unusable /config answer")

// InitEngineMetrics fetches the instance's enabled engines from GET /config and
// mints their per-engine series at zero (see initEngineMetrics). It is meant
// to run in its own goroutine after startup: it never touches readiness,
// retries with backoff while SearXNG is not answering yet, and returns after
// success, after the retry budget, or when ctx is done. A failure is logged
// once at WARN; nothing else depends on it — engines still get their series
// lazily, the first time a search response names them.
func (s *Searcher) InitEngineMetrics(ctx context.Context) {
	if s.metrics == nil {
		return
	}
	deadline := time.Now().Add(s.initBudget)
	backoff := s.initBackoff
	for attempt := 1; ; attempt++ {
		engines, err := s.enabledEngines(ctx)
		if err == nil {
			s.initEngineMetrics(engines)
			s.logger.Info("searxng engine metrics initialized at zero",
				"engines", len(engines), "attempts", attempt)
			return
		}
		if ctx.Err() != nil {
			s.logger.Debug("searxng engine metrics init canceled", "error", ctx.Err())
			return
		}
		if errors.Is(err, errConfigUnusable) || !time.Now().Add(backoff).Before(deadline) {
			s.logger.Warn("searxng /config unavailable: per-engine metrics will be created on first use only",
				"error", err, "attempts", attempt)
			return
		}
		s.logger.Debug("searxng /config not ready, retrying",
			"error", err, "attempt", attempt, "backoff", backoff)
		select {
		case <-ctx.Done():
			s.logger.Debug("searxng engine metrics init canceled", "error", ctx.Err())
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, s.initMaxBackoff)
	}
}

// enabledEngines makes one GET /config attempt and returns the names of the
// engines the instance has enabled. The retry loop is InitEngineMetrics'
// own, so the client is asked for a single attempt.
func (s *Searcher) enabledEngines(ctx context.Context) ([]string, error) {
	resp, err := s.configClient.DoRetry(ctx, http.MethodGet, s.configURL, nil, nil,
		httpx.RetryConfig{MaxAttempts: 1})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read /config: %w", err)
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: status %d", errConfigUnusable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng /config returned %d", resp.StatusCode)
	}
	var cr configResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, fmt.Errorf("%w: %v", errConfigUnusable, err)
	}
	if cr.Engines == nil {
		return nil, fmt.Errorf("%w: no engines key", errConfigUnusable)
	}
	var engines []string
	for _, e := range *cr.Engines {
		if e.Name != "" && e.Enabled != nil && *e.Enabled {
			engines = append(engines, e.Name)
		}
	}
	return engines, nil
}
