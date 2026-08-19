// Command omnifeed is the entry point. It loads OMNIFEED_*
// environment variables, wires the engine registry, searcher, and MCP tools
// into the transports, and runs everything until SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/kinorai/omnifeed/internal/auth"
	browsercrawl4ai "github.com/kinorai/omnifeed/internal/browser/crawl4ai"
	"github.com/kinorai/omnifeed/internal/config"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/engine/bluesky"
	"github.com/kinorai/omnifeed/internal/engine/crawl4ai"
	"github.com/kinorai/omnifeed/internal/engine/discourse"
	"github.com/kinorai/omnifeed/internal/engine/github"
	"github.com/kinorai/omnifeed/internal/engine/hackernews"
	"github.com/kinorai/omnifeed/internal/engine/reddit"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/observability"
	"github.com/kinorai/omnifeed/internal/search/searxng"
	"github.com/kinorai/omnifeed/internal/transport/mcp"
	"github.com/kinorai/omnifeed/internal/transport/mcp/tools"
	"github.com/kinorai/omnifeed/internal/transport/openwebui"
	"github.com/kinorai/omnifeed/internal/transport/searchapi"
	"github.com/kinorai/omnifeed/internal/version"
)

func main() {
	var mcpStdio bool
	flag.BoolVar(&mcpStdio, "mcp-stdio", false, "Run MCP over stdio (alternative to OMNIFEED_MCP_STDIO=true)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	if mcpStdio {
		cfg.MCPStdio = true
	}

	logger := observability.NewLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	if err := run(cfg, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, logger *slog.Logger) error {
	// --- HTTP client with retry, shared by both engines ---

	// One transport for every outbound client: a widened per-host idle pool so
	// concurrent calls to the same upstream reuse connections instead of
	// re-handshaking TLS (DefaultTransport keeps only 2 idle conns per host).
	transport := httpx.NewTransport()
	httpClient := httpx.New(&http.Client{Timeout: cfg.Crawl4AITimeout, Transport: transport})
	limiter := httpx.NewDomainLimiter(cfg.PerDomainConcurrency, cfg.PerDomainDelay)
	metrics := observability.NewMetrics()
	// Count every HTTP attempt the crawl client makes (first try + retries) so
	// retry waste shows up in metrics, not just reconstructed from logs, and
	// time every upstream round-trip per attempt. Hooks must be set BEFORE the
	// adapters below copy the client via WithUpstream.
	httpClient.OnAttempt = metrics.ObserveAttempt
	httpClient.OnUpstream = metrics.ObserveUpstream
	// Surface time spent queued behind the per-domain politeness limiter.
	limiter.OnWait = metrics.ObserveLimiterWait

	// --- Engines ---

	redditDefaults := reddit.Options{
		KeepDepth:   cfg.RedditKeepDepth,
		KeepCreated: cfg.RedditKeepCreated,
		MaxRounds:   cfg.RedditMaxRounds,
		Format:      cfg.RedditFormat,
		FetchLimit:  cfg.RedditFetchLimit,
		Depth:       cfg.RedditDepth,
		Sort:        cfg.RedditSort,
		MaxComments: cfg.RedditMaxComments,
		MaxTopLevel: cfg.RedditMaxTopLevel,
	}

	// The Reddit engine fetches through a real browser because Reddit's edge
	// blocks non-browser HTTP clients — see reddit.Fetcher.
	crawl4aiBrowser := browsercrawl4ai.New(httpClient, cfg.Crawl4AIURL, cfg.Crawl4AIToken)
	redditEngine := reddit.New(reddit.Config{
		Fetcher: reddit.NewFetcher(reddit.FetcherConfig{
			Browser: crawl4aiBrowser,
		}),
		Limiter:     limiter,
		Timeout:     cfg.RedditTimeout,
		DefaultOpts: redditDefaults,
		Logger:      logger,
		Metrics:     metrics,
	})
	crawl4aiEngine := crawl4ai.New(crawl4ai.Config{
		Endpoint:       cfg.Crawl4AIURL,
		Token:          cfg.Crawl4AIToken,
		Client:         httpClient,
		Limiter:        limiter,
		KeepLinks:      cfg.Crawl4AIKeepLinks,
		PruneThreshold: cfg.Crawl4AIPruneThreshold,
		WaitUntil:      cfg.Crawl4AIWaitUntil,

		ExcludedSelector: cfg.Crawl4AIExcludedSelector,
		TargetElements:   cfg.Crawl4AITargetElements,
		ScanFullPage:     cfg.Crawl4AIScanFullPage,
		ScrollDelay:      cfg.Crawl4AIScrollDelay,
		DelayBeforeHTML:  cfg.Crawl4AIDelayBeforeHTML,
		RemoveOverlays:   cfg.Crawl4AIRemoveOverlays,
		BlockPrivateIPs:  cfg.BlockPrivateIPs,
	})

	// The Hacker News, GitHub, Discourse and Bluesky engines read their public
	// JSON APIs directly (none is bot-walled, unlike Reddit), so — exceptionally
	// — they do NOT go through crawl4ai. They need outbound access to
	// hn.algolia.com, api.github.com, the configured Discourse hosts, and
	// public.api.bsky.app respectively.
	hackerNewsEngine := hackernews.New(hackernews.Config{
		Client:  httpClient,
		Limiter: limiter,
		Logger:  logger,
	})
	// Anonymous GitHub access works (60 req/h/IP); OMNIFEED_GITHUB_TOKEN raises
	// the quota to 5000/h.
	gitHubEngine := github.New(github.Config{
		Client:  httpClient,
		Limiter: limiter,
		Token:   cfg.GitHubToken,
		Logger:  logger,
	})
	// Discourse runs on arbitrary self-hosted domains, so the engine claims only
	// the hosts OMNIFEED_DISCOURSE_HOSTS lists; unlisted forums fall through to
	// the browser fallback. An empty list makes it claim nothing.
	discourseEngine := discourse.New(discourse.Config{
		Client:  httpClient,
		Limiter: limiter,
		Hosts:   cfg.DiscourseHosts,
		Logger:  logger,
	})

	// bsky.app is a client-side SPA the browser path renders as an empty shell;
	// the AT Protocol AppView behind it is public and keyless.
	blueskyEngine := bluesky.New(bluesky.Config{
		Client:  httpClient,
		Limiter: limiter,
		Logger:  logger,
	})

	registry := engine.New().
		Register(redditEngine).
		Register(hackerNewsEngine).
		Register(gitHubEngine).
		Register(discourseEngine).
		Register(blueskyEngine).
		Fallback(crawl4aiEngine).
		BlockPrivateIPs(cfg.BlockPrivateIPs).
		Logger(logger).
		Metrics(metrics)

	// --- Searcher (optional — search tool is exposed only when configured) ---

	var searcher domain.Searcher
	var searxngClient *httpx.Client
	if cfg.SearXNGURL != "" {
		searxngClient = httpx.New(&http.Client{Timeout: cfg.SearXNGTimeout, Transport: transport})
		searxngClient.OnAttempt = metrics.ObserveAttempt
		searxngClient.OnUpstream = metrics.ObserveUpstream
		searcher = searxng.New(searxng.Config{
			Endpoint:    cfg.SearXNGURL,
			Client:      searxngClient,
			Logger:      logger,
			Metrics:     metrics,
			SiteEngines: cfg.SearXNGSiteEngines,
			Limiter:     searxngLimiter(cfg, metrics, logger),
			Audit:       cfg.SearchAudit,
		})
		if cfg.SearchAudit != "off" {
			// Announced at startup because both modes log every query string
			// and "full" adds every result URL. An operator who did not intend
			// that should find out from the first log line, not from the log
			// store. Config.Load has already guaranteed the level lets INFO
			// through, so this cannot be swallowed.
			logger.Info("search audit log enabled", "mode", cfg.SearchAudit,
				"logs_queries", true, "logs_result_urls", cfg.SearchAudit == "full")
		}
	} else {
		logger.Info("search tool disabled (OMNIFEED_SEARXNG_URL not set)")
	}

	// --- MCP tools (shared by the stdio and HTTP transports) ---

	mcpTools := []mcp.Tool{
		tools.FetchURL(registry, redditDefaults, metrics, cfg.FetchMaxChars),
	}
	if searcher != nil {
		mcpTools = append(mcpTools, tools.WebSearch(searcher, cfg.SearchMaxResults, metrics))
	}

	// --- MCP stdio mode short-circuits everything else ---

	if cfg.MCPStdio {
		logger.Info("starting MCP server on stdio", "version", version.Version)
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		s := mcp.New(mcp.Config{
			Tools:  mcpTools,
			Logger: logger,
		})
		return s.ServeStdio(ctx, os.Stdin, os.Stdout)
	}

	// --- Auth (HTTP transports only; stdio inherits its parent process's
	// trust). Fail closed: refuse to start the internet-facing transports
	// without a token rather than silently allowing every request. The
	// Cloudflare tunnel bypasses the nginx RFC1918 whitelist, so an empty key
	// would mean a fully unauthenticated public proxy. OMNIFEED_DEV_NO_AUTH=true is
	// the explicit local-development opt-out.
	var authn auth.Authenticator
	switch {
	case cfg.APIKey != "":
		if cfg.AllowNoAuth {
			logger.Warn("OMNIFEED_DEV_NO_AUTH=true ignored because OMNIFEED_API_KEY is set — authentication is enabled")
		}
		authn = auth.NewSharedBearer(cfg.APIKey)
		logger.Info("api key authentication enabled")
	case cfg.AllowNoAuth:
		authn = auth.AlwaysAllow{}
		logger.Warn("OMNIFEED_API_KEY not set and OMNIFEED_DEV_NO_AUTH=true — HTTP transports are UNAUTHENTICATED")
	default:
		return fmt.Errorf("OMNIFEED_API_KEY is not set: refusing to start the HTTP transports unauthenticated. " +
			"Set OMNIFEED_API_KEY=<token> to enable bearer-token auth, or OMNIFEED_DEV_NO_AUTH=true to run without auth (local/dev only)")
	}

	// --- HTTP server (Open WebUI loader + MCP HTTP) ---

	loaderServer := openwebui.New(openwebui.Config{
		Registry:          registry,
		Authenticator:     authn,
		Logger:            logger,
		Metrics:           metrics,
		MaxURLsPerRequest: cfg.MaxURLsPerRequest,
		RedditDefaults:    redditDefaults,
	})

	mainMux := http.NewServeMux()
	loaderServer.Register(mainMux)

	// REST search on the same port as /crawl, mounted only when a Searcher is
	// configured — mirrors the web_search MCP tool so non-MCP clients can search
	// over plain HTTP.
	if searcher != nil {
		searchapi.New(searchapi.Config{
			Searcher:      searcher,
			Authenticator: authn,
			Logger:        logger,
			Metrics:       metrics,
			MaxResults:    cfg.SearchMaxResults,
		}).Register(mainMux)
	}

	// --- MCP HTTP server (separate listener) ---

	mcpServer := mcp.New(mcp.Config{
		Tools:         mcpTools,
		Authenticator: authn,
		Logger:        logger,
	})
	mcpMux := http.NewServeMux()
	mcpServer.Register(mcpMux)

	// --- Observability HTTP server (separate listener for /metrics + health) ---

	obsMux := http.NewServeMux()
	readyChecks := []observability.ReadyCheck{
		upstreamReady("crawl4ai", httpClient, cfg.Crawl4AIURL),
	}
	if searcher != nil {
		readyChecks = append(readyChecks,
			upstreamReady("searxng", searxngClient, cfg.SearXNGURL+"/healthz"))
	}
	health := observability.NewHealth(5*time.Second, readyChecks...)
	health.Register(obsMux)
	metrics.RegisterMetrics(obsMux)
	if cfg.EnablePprof {
		observability.RegisterPprof(obsMux)
		logger.Warn("pprof enabled — DO NOT expose to the public internet")
	}

	// --- Run all three servers ---

	// Every request-serving mux sits behind the Origin guard (DNS-rebinding
	// protection, a Streamable HTTP transport MUST). It wraps at this level —
	// not inside one transport — so the loader and search APIs get the same
	// guard, and so a future transport mounted here inherits it for free.
	originGuard := auth.OriginGuard(cfg.AllowedOrigins)
	servers := []serverSpec{
		{name: "loader", addr: cfg.ListenAddr, handler: originGuard(mainMux), writeTimeout: 300 * time.Second},
		{name: "mcp", addr: cfg.MCPListenAddr, handler: originGuard(mcpMux), writeTimeout: 300 * time.Second},
		{name: "observability", addr: cfg.MetricsAddr, handler: obsMux, writeTimeout: 30 * time.Second},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("starting servers",
		"version", version.Version,
		"loader", cfg.ListenAddr,
		"mcp", cfg.MCPListenAddr,
		"observability", cfg.MetricsAddr,
		"crawl4ai_url", cfg.Crawl4AIURL,
		"searxng_url", cfg.SearXNGURL,
	)

	return runServers(ctx, logger, health, servers)
}

type serverSpec struct {
	name         string
	addr         string
	handler      http.Handler
	writeTimeout time.Duration
}

func runServers(ctx context.Context, logger *slog.Logger, health *observability.Health, specs []serverSpec) error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(specs))
	servers := make([]*http.Server, len(specs))

	for i, sp := range specs {
		if sp.addr == "" {
			logger.Info("server disabled", "name", sp.name)
			continue
		}
		srv := &http.Server{
			Addr:              sp.addr,
			Handler:           sp.handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      sp.writeTimeout,
			IdleTimeout:       120 * time.Second,
		}
		servers[i] = srv
		wg.Add(1)
		go func(name string, s *http.Server) {
			defer wg.Done()
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s: %w", name, err)
			}
		}(sp.name, srv)
	}

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining...")
	case err := <-errCh:
		logger.Error("server error, shutting down", "err", err)
	}

	health.MarkShuttingDown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		if s != nil {
			_ = s.Shutdown(shutdownCtx)
		}
	}
	wg.Wait()
	logger.Info("shutdown complete")
	return nil
}

// searxngLimiter builds the pacing limiter for search queries, or returns nil
// when neither control is configured — pacing is opt-in, so an unconfigured
// deployment behaves exactly as before.
//
// Concurrency is fixed at 1 rather than exposed as a knob: the point is to
// space requests, and a second query in flight would step straight over the
// delay the first one is serving.
func searxngLimiter(cfg config.Config, metrics *observability.Metrics, logger *slog.Logger) *httpx.DomainLimiter {
	if cfg.SearXNGDelay <= 0 && cfg.SearXNGQuota <= 0 {
		logger.Info("searxng pacing disabled (OMNIFEED_SEARXNG_DELAY and OMNIFEED_SEARXNG_QUOTA unset)")
		return nil
	}
	logger.Info("searxng pacing enabled",
		"delay", cfg.SearXNGDelay,
		"quota", cfg.SearXNGQuota,
		"window", cfg.SearXNGQuotaWindow)
	l := httpx.NewDomainQuotaLimiter(1, cfg.SearXNGDelay, cfg.SearXNGQuota, cfg.SearXNGQuotaWindow)
	l.OnWait = metrics.ObserveLimiterWait
	return l
}

// upstreamReady is a readiness check: GET the endpoint and report failure if
// it isn't reachable. Any reachable status counts as up — even 405 means the
// server is listening.
func upstreamReady(name string, client *httpx.Client, endpoint string) observability.ReadyCheck {
	return func(ctx context.Context) error {
		if endpoint == "" {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		resp, err := client.HTTP.Do(req)
		if err != nil {
			return fmt.Errorf("%s unreachable: %w", name, err)
		}
		_ = resp.Body.Close()
		return nil
	}
}
