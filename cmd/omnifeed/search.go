package main

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/kinorai/omnifeed/internal/config"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/httpx/redislimit"
	"github.com/kinorai/omnifeed/internal/observability"
	"github.com/kinorai/omnifeed/internal/search/failover"
	"github.com/kinorai/omnifeed/internal/search/searxng"
	"github.com/kinorai/omnifeed/internal/search/tinyfish"
)

// tinyfishConcurrency caps TinyFish searches in flight per process. TinyFish
// limits requests per minute, not concurrency, so this only keeps one fan-out
// from spending the whole minute's quota in a single burst.
const tinyfishConcurrency = 4

// searchDeps is what every searcher constructor draws on.
type searchDeps struct {
	cfg       config.Config
	rdb       redis.UniversalClient
	metrics   *observability.Metrics
	logger    *slog.Logger
	transport http.RoundTripper
	tracing   bool
}

// buildSearcher assembles OMNIFEED_SEARCHERS into one domain.Searcher: the
// configured searchers in order, each next one the failover backup of the
// chain before it. It returns nil when none is configured, which turns search
// off. The SearXNG searcher is also returned on its own (nil when absent): its
// engine metrics are initialised in the background at startup.
func buildSearcher(d searchDeps) (domain.Searcher, *searxng.Searcher) {
	var chain []domain.Searcher
	var searxngSearcher *searxng.Searcher
	for _, name := range d.cfg.Searchers {
		switch name {
		case "searxng":
			if d.cfg.SearXNGURL == "" {
				d.logger.Info("searcher not configured, skipped", "searcher", name, "needs", "OMNIFEED_SEARXNG_URL")
				continue
			}
			searxngSearcher = newSearXNGSearcher(d)
			chain = append(chain, searxngSearcher)
		case "tinyfish":
			if d.cfg.TinyFishAPIKey == "" {
				d.logger.Info("searcher not configured, skipped", "searcher", name, "needs", "OMNIFEED_TINYFISH_API_KEY")
				continue
			}
			chain = append(chain, newTinyFishSearcher(d))
		}
	}
	switch len(chain) {
	case 0:
		d.logger.Info("search tool disabled (OMNIFEED_SEARXNG_URL and OMNIFEED_TINYFISH_API_KEY not set)")
		return nil, nil
	case 1:
		return chain[0], searxngSearcher
	}

	names := make([]string, len(chain))
	for i, s := range chain {
		names[i] = s.Name()
	}
	d.logger.Info("search failover chain", "searchers", strings.Join(names, " > "),
		"min_results", d.cfg.SearchFallbackMinResults,
		"primary_budget", d.cfg.SearchPrimaryBudget,
		"critical_engines", strings.Join(d.cfg.SearXNGCriticalEngines, ","))

	// Folded from the end so the first searcher is the outermost primary:
	// [a b c] becomes failover(a, failover(b, c)).
	searcher := chain[len(chain)-1]
	for i := len(chain) - 2; i >= 0; i-- {
		searcher = failover.New(failover.Config{
			Primary:       chain[i],
			Backup:        searcher,
			Policy:        failover.Policy{MinResults: d.cfg.SearchFallbackMinResults},
			PrimaryBudget: d.cfg.SearchPrimaryBudget,
			Logger:        d.logger,
			Metrics:       d.metrics,
		})
	}
	return searcher, searxngSearcher
}

func newSearXNGSearcher(d searchDeps) *searxng.Searcher {
	cfg := d.cfg
	pacer := searxngLimiter(cfg, d.rdb, d.metrics, d.logger)
	if d.tracing && pacer != nil {
		pacer = httpx.TraceLimiter("searxng", pacer)
	}
	client := httpx.New(&http.Client{Timeout: cfg.SearXNGTimeout, Transport: d.transport})
	client.OnAttempt = d.metrics.ObserveAttempt
	client.OnUpstream = d.metrics.ObserveUpstream
	client.OnRetryAfter = retryAfterHook(pacer, d.metrics)
	if cfg.SearchAudit != "off" {
		// Announced at startup because both modes log every query string
		// and "full" adds every result URL. An operator who did not intend
		// that should find out from the first log line, not from the log
		// store. Config.Load has already guaranteed the level lets INFO
		// through, so this cannot be swallowed.
		d.logger.Info("search audit log enabled", "mode", cfg.SearchAudit,
			"logs_queries", true, "logs_result_urls", cfg.SearchAudit == "full")
	}
	return searxng.New(searxng.Config{
		Endpoint:        cfg.SearXNGURL,
		Client:          client,
		Logger:          d.logger,
		Metrics:         d.metrics,
		SiteEngines:     cfg.SearXNGSiteEngines,
		CriticalEngines: cfg.SearXNGCriticalEngines,
		Limiter:         pacer,
		MaxWait:         cfg.SearXNGMaxWait,
		Audit:           cfg.SearchAudit,
	})
}

func newTinyFishSearcher(d searchDeps) *tinyfish.Searcher {
	cfg := d.cfg
	pacer := tinyfishLimiter(cfg, d.rdb, d.metrics, d.logger)
	if d.tracing && pacer != nil {
		pacer = httpx.TraceLimiter("tinyfish", pacer)
	}
	client := httpx.New(&http.Client{Timeout: cfg.TinyFishTimeout, Transport: d.transport})
	client.OnAttempt = d.metrics.ObserveAttempt
	client.OnUpstream = d.metrics.ObserveUpstream
	client.OnRetryAfter = retryAfterHook(pacer, d.metrics)
	return tinyfish.New(tinyfish.Config{
		Endpoint: cfg.TinyFishURL,
		APIKey:   cfg.TinyFishAPIKey,
		Client:   client,
		Logger:   d.logger,
		Limiter:  pacer,
		MaxWait:  cfg.TinyFishMaxWait,
	})
}

// tinyfishLimiter paces TinyFish under its per-key requests-per-minute limit,
// shared across replicas when Redis is configured — the key is one budget
// however many pods spend it. Nil when OMNIFEED_TINYFISH_QUOTA is 0.
func tinyfishLimiter(cfg config.Config, rdb redis.UniversalClient, metrics *observability.Metrics, logger *slog.Logger) pacer {
	if cfg.TinyFishQuota <= 0 {
		logger.Info("tinyfish pacing disabled (OMNIFEED_TINYFISH_QUOTA=0)")
		return nil
	}
	logger.Info("tinyfish pacing enabled",
		"quota", cfg.TinyFishQuota, "window", cfg.TinyFishQuotaWindow, "shared", rdb != nil)
	return buildLimiter("tinyfish", rdb,
		httpx.NewDomainQuotaLimiter(tinyfishConcurrency, 0, cfg.TinyFishQuota, cfg.TinyFishQuotaWindow),
		redislimit.Config{
			Prefix:        cfg.RedisKeyPrefix,
			MaxConcurrent: tinyfishConcurrency,
			Quota:         cfg.TinyFishQuota,
			Window:        cfg.TinyFishQuotaWindow,
		},
		metrics, logger)
}
