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
	"strings"
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
	"github.com/kinorai/omnifeed/internal/engine/reddit/redisstate"
	"github.com/kinorai/omnifeed/internal/engine/twitter"
	"github.com/kinorai/omnifeed/internal/fetchcache"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/httpx/redislimit"
	"github.com/kinorai/omnifeed/internal/observability"
	"github.com/kinorai/omnifeed/internal/transport/mcp"
	"github.com/kinorai/omnifeed/internal/transport/mcp/tools"
	"github.com/kinorai/omnifeed/internal/transport/openwebui"
	"github.com/kinorai/omnifeed/internal/transport/searchapi"
	"github.com/kinorai/omnifeed/internal/version"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
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
	// --- Tracing (only when OTEL_EXPORTER_OTLP_ENDPOINT is set) ---

	shutdownTracing, tracing, err := observability.SetupTracing(context.Background(), observability.TracingConfig{
		Endpoint:    cfg.OTelEndpoint,
		ServiceName: cfg.OTelServiceName,
		Version:     version.Version,
	})
	if err != nil {
		return err
	}
	// Deferred first, so it runs last: after the servers drained, flush the
	// spans they produced.
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			logger.Warn("tracing shutdown: spans may be lost", "err", err)
		}
	}()
	if tracing {
		logger.Info("tracing enabled", "service_name", cfg.OTelServiceName)
	}

	// --- HTTP client with retry, shared by both engines ---

	// One transport for every outbound client: a widened per-host idle pool so
	// concurrent calls to the same upstream reuse connections instead of
	// re-handshaking TLS (DefaultTransport keeps only 2 idle conns per host).
	// With tracing on, it is wrapped once so every upstream call (crawl4ai,
	// SearXNG, the API engines) is a client span and carries traceparent.
	var transport http.RoundTripper = httpx.NewTransport()
	if tracing {
		transport = otelhttp.NewTransport(transport)
	}
	httpClient := httpx.New(&http.Client{Timeout: cfg.Crawl4AITimeout, Transport: transport})
	metrics := observability.NewMetrics()
	metrics.SetBuildInfo(version.Version)
	// Count every HTTP attempt the crawl client makes (first try + retries) so
	// retry waste shows up in metrics, not just reconstructed from logs, and
	// time every upstream round-trip per attempt. Hooks must be set BEFORE the
	// adapters below copy the client via WithUpstream.
	httpClient.OnAttempt = metrics.ObserveAttempt
	httpClient.OnUpstream = metrics.ObserveUpstream

	// --- Rate limiting (shared through Redis when configured) ---

	rdb, closeRedis, err := redisClient(cfg, logger)
	if err != nil {
		return err
	}
	if closeRedis != nil {
		defer closeRedis()
	}
	limiter := buildLimiter("domain", rdb,
		httpx.NewDomainLimiter(cfg.PerDomainConcurrency, cfg.PerDomainDelay),
		redislimit.Config{
			Prefix:        cfg.RedisKeyPrefix,
			MaxConcurrent: cfg.PerDomainConcurrency,
			MinDelay:      cfg.PerDomainDelay,
		},
		metrics, logger)
	if tracing {
		limiter = httpx.TraceLimiter("domain", limiter)
	}
	httpClient.OnRetryAfter = retryAfterHook(limiter, metrics)

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

	// The Reddit engine tries a chain of methods: a credential over plain HTTP
	// first, the browser last — see reddit.Fetcher and docs/adr/0001.
	crawl4aiBrowser := browsercrawl4ai.New(httpClient, cfg.Crawl4AIURL, cfg.Crawl4AIToken)
	// Reddit's back-off holds only the method that got it (see reddit.Chain),
	// never the crawl limiter: a hold there would stall a working credential.
	redditFetcher := reddit.NewFetcher(reddit.FetcherConfig{
		Browser:       crawl4aiBrowser,
		ClientID:      cfg.RedditClientID,
		SessionCookie: cfg.RedditSession,
		UserAgent:     "omnifeed/" + version.Version,
		HTTP: &http.Client{
			Timeout:   time.Minute,
			Transport: transport,
			// Share links resolve from the Location header, not by following.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Methods:  cfg.RedditMethods,
		Cooldown: cfg.RedditMethodCooldown,
		OnBudget: func(method string, remaining float64) {
			metrics.RedditRemaining.WithLabelValues(method).Set(remaining)
		},
		OnInPage: func(kind string, n int) {
			metrics.RedditInPageRequests.WithLabelValues(kind).Add(float64(n))
		},
		OnOutcome: func(method, outcome string) {
			metrics.RedditMethodCalls.WithLabelValues(method, outcome).Inc()
			if outcome == "ok" || outcome == "incomplete" {
				metrics.RedditMethodLastSuccess.WithLabelValues(method).SetToCurrentTime()
			}
		},
		OnState: func(method string, up bool, why string, hold time.Duration) {
			v := 0.0
			if up {
				v = 1
			}
			metrics.RedditMethodUp.WithLabelValues(method).Set(v)
			switch {
			case up:
				logger.Info("reddit: method working again", "method", method)
			case why == "backoff":
				logger.Info("reddit: method held for Reddit's back-off", "method", method, "hold", hold)
			case why == "held":
				logger.Info("reddit: method on hold, skipped", "method", method, "hold", hold.Round(time.Second))
			default:
				logger.Warn("reddit: method failing, skipped", "method", method, "hold", hold)
			}
		},
		Quota:  traceLimiter(tracing, "reddit", redditQuota(cfg, rdb, metrics, logger)),
		Shared: redditShared(rdb, cfg.RedisKeyPrefix),
		// Same metrics and log lines as the rate limiters' Redis backend,
		// under their own op and scope labels.
		OnSharedError: func(op string, err error) {
			metrics.ObserveRatelimitBackendError("reddit_method_" + op)
			logger.Warn("reddit method state backend operation failed", "op", op, "err", err)
		},
		OnSharedDown: func(down bool) {
			metrics.SetRatelimitDegraded("reddit_methods", down)
			if down {
				logger.Warn("reddit method state degraded to per-pod (redis unreachable)")
				return
			}
			logger.Info("reddit method state shared again (redis reachable)")
		},
	})
	if len(redditFetcher.Methods()) == 0 {
		return fmt.Errorf("OMNIFEED_REDDIT_METHODS %q leaves no usable Reddit method: client_id needs OMNIFEED_REDDIT_CLIENT_ID, session needs OMNIFEED_REDDIT_SESSION", strings.Join(cfg.RedditMethods, ","))
	}
	if rdb != nil {
		metrics.SetRatelimitDegraded("reddit_methods", false)
	}
	for _, m := range redditFetcher.Methods() {
		metrics.RedditMethodUp.WithLabelValues(m).Set(1)
		metrics.RedditMethodLastSuccess.WithLabelValues(m)
	}
	// At 0 from the start, so increase() sees the first page's requests.
	for _, kind := range []string{"listing", "thread"} {
		metrics.RedditInPageRequests.WithLabelValues(kind)
	}
	// A hold another replica recorded still applies to this pod: report it
	// now rather than at the first call.
	loadCtx, cancelLoad := context.WithTimeout(context.Background(), cfg.RedisTimeout)
	redditFetcher.LoadSharedHolds(loadCtx)
	cancelLoad()
	logger.Info("reddit: method chain", "methods", strings.Join(redditFetcher.Methods(), ","), "cooldown", cfg.RedditMethodCooldown)
	if cfg.RedditSession != "" {
		if exp, ok := reddit.SessionExpiry(cfg.RedditSession); ok {
			metrics.RedditSessionExpiry.Set(float64(exp.Unix()))
			if left := time.Until(exp); left < 14*24*time.Hour {
				logger.Warn("reddit: session cookie expires soon, log in again and refresh OMNIFEED_REDDIT_SESSION", "expires", exp.Format(time.DateOnly), "days_left", int(left.Hours()/24))
			}
		}
	}
	redditEngine := reddit.New(reddit.Config{
		Fetcher:     redditFetcher,
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
		UserAgent:        cfg.Crawl4AIUserAgent,
		Stealth:          cfg.Crawl4AIStealth,
		ChallengeWait:    cfg.Crawl4AIChallengeWait,
		MinProseChars:    cfg.Crawl4AIMinProseChars,
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
	// the quota to 5000/h and enables discussions (GraphQL needs a token).
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
		Register(blueskyEngine)

	// x.com walls headless browsers and twitter.com links fail in crawl4ai, so
	// post URLs read public JSON mirrors directly (FxTwitter, then the embed
	// syndication endpoint and vxTwitter), with crawl4ai on the x.com URL last.
	// Needs outbound access to api.fxtwitter.com (or the configured FxEmbed),
	// cdn.syndication.twimg.com, api.vxtwitter.com and t.co.
	threadEngines := []domain.Engine{redditEngine, hackerNewsEngine, gitHubEngine, discourseEngine, blueskyEngine}
	if cfg.TwitterEnabled {
		twitterEngine := twitter.New(twitter.Config{
			Client:          httpClient,
			Limiter:         limiter,
			Generic:         crawl4aiEngine,
			FxTwitterURL:    cfg.TwitterFxTwitterURL,
			MaxReplies:      cfg.TwitterMaxReplies,
			BlockPrivateIPs: cfg.BlockPrivateIPs,
			Logger:          logger,
		})
		registry.Register(twitterEngine)
		// A post and its replies change like a thread: the threads TTL.
		threadEngines = append(threadEngines, twitterEngine)
	}

	registry.
		Fallback(crawl4aiEngine).
		BlockPrivateIPs(cfg.BlockPrivateIPs).
		Logger(logger).
		Metrics(metrics)

	// --- fetch_url response cache (decorates the registry for every transport) ---

	var dispatcher engine.Dispatcher = registry
	if cfg.CacheEnabled {
		dispatcher = fetchCache(cfg, registry, rdb, metrics, logger, threadEngines...)
	} else {
		logger.Info("fetch cache disabled (OMNIFEED_CACHE_ENABLED=false)")
	}

	// --- Searcher (optional — search tool is exposed only when configured) ---

	searcher, searxngSearcher := buildSearcher(searchDeps{
		cfg:       cfg,
		rdb:       rdb,
		metrics:   metrics,
		logger:    logger,
		transport: transport,
		tracing:   tracing,
	})

	// --- MCP tools (shared by the stdio and HTTP transports) ---

	mcpTools := []mcp.Tool{
		tools.FetchURL(dispatcher, redditDefaults, metrics, cfg.FetchMaxChars),
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
			Tools:   mcpTools,
			Logger:  logger,
			Metrics: metrics,
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
		Registry:          dispatcher,
		Authenticator:     authn,
		Logger:            logger,
		Metrics:           metrics,
		MaxURLsPerRequest: cfg.MaxURLsPerRequest,
		MaxResponseBytes:  cfg.CrawlMaxResponseMB << 20,
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
		Metrics:       metrics,
	})
	mcpMux := http.NewServeMux()
	mcpServer.Register(mcpMux)

	// --- Observability HTTP server (separate listener for /metrics + health) ---

	obsMux := http.NewServeMux()
	// Readiness is NOT gated on crawl4ai or SearXNG: an outage of either fails
	// the calls that need it (classified, retryable), while the cache, the
	// Reddit quota and the API engines keep serving. Pulling every pod out of
	// the Service for it turned a partial outage into a total one. Their state
	// is exported as omnifeed_dependency_up instead, for alerting.
	health := observability.NewHealth(5 * time.Second)
	health.Register(obsMux)
	deps := &observability.DependencyChecker{
		Checks:  dependencyChecks(cfg, rdb, searxngSearcher != nil),
		Metrics: metrics,
		Logger:  logger,
	}
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
	// Outermost first: the request id (so every response, a recovered panic's
	// 500 included, carries X-Request-Id), then RED metrics + panic recovery.
	serve := func(server string, h http.Handler) http.Handler {
		h = observability.RequestID(metrics.HTTPMiddleware(server, h))
		if tracing {
			h = tracedHandler(server, h)
		}
		return h
	}
	servers := []serverSpec{
		{name: "loader", addr: cfg.ListenAddr, handler: serve(observability.ServerLoader, originGuard(mainMux)), writeTimeout: 300 * time.Second},
		{name: "mcp", addr: cfg.MCPListenAddr, handler: serve(observability.ServerMCP, originGuard(mcpMux)), writeTimeout: 300 * time.Second},
		{name: "observability", addr: cfg.MetricsAddr, handler: metrics.HTTPMiddleware(observability.ServerObs, obsMux), writeTimeout: 30 * time.Second},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Mint every enabled SearXNG engine's series at zero, in the background:
	// SearXNG may still be starting, and readiness must not wait on metrics.
	if searxngSearcher != nil {
		go func() {
			defer observability.Recover(observability.PanicBackground, nil)
			searxngSearcher.InitEngineMetrics(ctx)
		}()
	}
	go func() {
		defer observability.Recover(observability.PanicBackground, nil)
		deps.Run(ctx)
	}()

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
			// net/http's own errors (TLS handshakes, a panicking handler it
			// recovered, a bad Content-Length…) go through slog too, so they
			// are JSON lines like everything else instead of bare text.
			ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
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

// tracedHandler wraps an API server's handler in otelhttp: a server span per
// request, continuing the caller's W3C traceparent when it sent one. Span names
// use the bounded route, never the raw path; health and metrics probes are not
// traced (the observability server is not wrapped at all).
func tracedHandler(server string, h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, server,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + observability.RouteOf(r.URL.Path)
		}),
		otelhttp.WithFilter(func(r *http.Request) bool {
			switch observability.RouteOf(r.URL.Path) {
			case "/livez", "/readyz", "/metrics":
				return false
			}
			return true
		}),
	)
}

// traceLimiter wraps a possibly-nil limiter in a limiter.wait span when
// tracing is on. A nil limiter stays a nil INTERFACE (callers test == nil).
func traceLimiter(tracing bool, scope string, l httpx.Limiter) httpx.Limiter {
	if !tracing || l == nil {
		return l
	}
	return httpx.TraceLimiter(scope, l)
}

// pacer is a limiter as main.go wires it: the httpx.Limiter every engine sees,
// plus the Penalize method all three implementations carry and the interface
// deliberately does not — engines never penalize, only the retry client that
// sees the 429 does.
type pacer interface {
	httpx.Limiter
	Penalize(rawURL string, d time.Duration)
}

// retryAfterCap bounds how long one Retry-After header may hold a host back. A
// hostile or misconfigured upstream must not be able to starve a domain for a
// day with one number.
const retryAfterCap = 5 * time.Minute

// retryAfterHook feeds an upstream's Retry-After back into the limiter that
// paces THAT client, so the answer outlives the request that got it: without
// this, the retry loop honors the header once (capped at seconds) and the next
// caller walks into the same wall.
//
// Each client penalizes its own scope only — the crawl client the "domain"
// limiter, the searxng client the "searxng" pacer. Both key on the URL's host,
// and a limiter only ever reads keys for the hosts it paces, so a cross-scope
// penalty would write a key nothing reads.
func retryAfterHook(p pacer, metrics *observability.Metrics) func(upstream, rawURL string, wait time.Duration) {
	return func(upstream, rawURL string, wait time.Duration) {
		// crawl4ai is a proxy, so the two URLs are different hosts: the request
		// URL is the crawl4ai endpoint, the URL the limiter paced is the target
		// site. Penalizing here would hold back a host no limiter ever reads,
		// and the crawled site's own 429 never reaches us anyway. No penalty
		// happened, so the counter must not claim one either.
		if upstream == "crawl4ai" {
			return
		}
		if p != nil {
			p.Penalize(rawURL, min(wait, retryAfterCap))
		}
		metrics.ObserveRatelimitPenalty(upstream)
	}
}

// redditShared shares the Reddit method chain's holds between replicas when
// Redis is configured. It returns a nil interface otherwise, so the chain
// keeps its holds in memory.
func redditShared(rdb redis.UniversalClient, prefix string) reddit.SharedState {
	if rdb == nil {
		return nil
	}
	return redisstate.New(rdb, prefix)
}

// redditQuota builds the per-request Reddit quota limiter, or returns nil when
// OMNIFEED_REDDIT_QUOTA is 0 — opt-in, so an unconfigured deployment behaves
// exactly as before. It is its own "reddit" scope rather than a setting on the
// crawl limiter: that one admits a whole crawl (up to 40 morechildren rounds)
// as one request, while Reddit counts every request. No delay and no cluster
// concurrency of its own — the crawl limiter already shapes the gaps and the
// concurrency; this only bounds the count per window.
//
// The returned value is a nil interface, never a typed nil pointer, when off:
// the fetcher checks Quota == nil.
func redditQuota(cfg config.Config, rdb redis.UniversalClient, metrics *observability.Metrics, logger *slog.Logger) httpx.Limiter {
	if cfg.RedditQuota <= 0 {
		return nil
	}
	logger.Info("reddit request quota enabled",
		"quota", cfg.RedditQuota, "window", cfg.RedditQuotaWindow, "shared", rdb != nil)
	// Concurrency matches the crawl limiter's: every Reddit request is made
	// while a crawl-limiter slot for www.reddit.com is held, so no more than
	// that many can be in flight anyway; a lower cap here would only queue
	// requests behind each other inside an already-admitted crawl.
	return buildLimiter("reddit", rdb,
		httpx.NewDomainQuotaLimiter(cfg.PerDomainConcurrency, 0, cfg.RedditQuota, cfg.RedditQuotaWindow),
		redislimit.Config{
			Prefix:        cfg.RedisKeyPrefix,
			MaxConcurrent: cfg.PerDomainConcurrency,
			Quota:         cfg.RedditQuota,
			Window:        cfg.RedditQuotaWindow,
		},
		metrics, logger)
}

// searxngLimiter builds the pacing limiter for search queries, or returns nil
// when neither control is configured — pacing is opt-in, so an unconfigured
// deployment behaves exactly as before. Unconfigured stays nil even with Redis
// on: a shared backend has nothing to share when no limit is set.
//
// OMNIFEED_SEARXNG_CONCURRENCY is the third control and the only one that
// bounds requests in FLIGHT. Without it a nonzero delay serializes admissions
// across the whole cluster, so the deployment runs one search at a time however
// many replicas it has — see internal/httpx/redislimit's package doc.
func searxngLimiter(cfg config.Config, rdb redis.UniversalClient, metrics *observability.Metrics, logger *slog.Logger) pacer {
	// Concurrency belongs in this gate: it is an independent control, so setting
	// it alone is a reasonable request. Omitting it here returned a nil limiter
	// and ran searches completely unpaced while logging "pacing disabled".
	if cfg.SearXNGDelay <= 0 && cfg.SearXNGQuota <= 0 && cfg.SearXNGConcurrency <= 0 {
		logger.Info("searxng pacing disabled (OMNIFEED_SEARXNG_DELAY, _QUOTA and _CONCURRENCY unset)")
		return nil
	}
	// A cluster cap needs shared state to be a cluster cap. Without Redis the
	// number below is per pod, so replicas × it is what the engines actually see.
	// Warned rather than rejected: pacing still works, it is just not shared.
	// The cap itself is honoured (the local limiter below is built send-spaced
	// for this case), so a single instance needs no Redis at all — the warning
	// is about the arithmetic on replica two.
	if cfg.SearXNGConcurrency > 0 && rdb == nil {
		logger.Warn("OMNIFEED_SEARXNG_CONCURRENCY is set but OMNIFEED_REDIS_URL is not: "+
			"the cap is per-process, not cluster-wide, so the engines see instances × this. "+
			"Correct for a single instance; set OMNIFEED_REDIS_URL to share it across replicas",
			"concurrency", cfg.SearXNGConcurrency)
	}
	logger.Info("searxng pacing enabled",
		"delay", cfg.SearXNGDelay,
		"quota", cfg.SearXNGQuota,
		"window", cfg.SearXNGQuotaWindow,
		"cluster_concurrency", cfg.SearXNGConcurrency)
	return buildLimiter("searxng", rdb,
		searxngLocalLimiter(cfg, rdb),
		redislimit.Config{
			Prefix: cfg.RedisKeyPrefix,
			// Per-pod, and left at 1 on purpose: redislimit's semFor raises the
			// local semaphore to ClusterConcurrency, so the cluster cap is what
			// binds while Redis is healthy.
			MaxConcurrent:      1,
			MinDelay:           cfg.SearXNGDelay,
			Quota:              cfg.SearXNGQuota,
			Window:             cfg.SearXNGQuotaWindow,
			ClusterConcurrency: cfg.SearXNGConcurrency,
			LeaseTTL:           searxngLeaseTTL(cfg.SearXNGTimeout),
		},
		metrics, logger)
}

// searxngLocalLimiter builds the in-process searxng limiter. It is the whole
// limiter when Redis is off, and the degraded-mode fallback when Redis is on —
// two different jobs, which is why the concurrency cap applies to only one of
// them.
//
// Redis OFF: the process IS the deployment, so OMNIFEED_SEARXNG_CONCURRENCY is
// exactly honoured. It needs the send-spaced limiter: the completion-spaced one
// measures its delay from lastSend, the zero time on a cold slot, so every
// waiter computes wait=0 and N goroutines are admitted at once (measured: 8 in
// 188µs). Send-spaced admits them minDelay apart, which is the load shape the
// cap was measured safe at.
//
// Redis ON: this is what a Redis outage falls back to, and it stays at
// concurrency 1 deliberately. Tracking the cluster cap here would make a blip
// emit the sharpest burst the deployment can produce — 16 simultaneous queries
// on 2 replicas, since neither replica can see the other's — where the shape
// measured safe was 16 in flight admitted 250ms apart. Degraded mode gets the
// old, slow, known-safe behaviour.
func searxngLocalLimiter(cfg config.Config, rdb redis.UniversalClient) *httpx.DomainLimiter {
	if rdb == nil && cfg.SearXNGConcurrency > 0 {
		return httpx.NewSpacedDomainQuotaLimiter(cfg.SearXNGConcurrency,
			cfg.SearXNGDelay, cfg.SearXNGQuota, cfg.SearXNGQuotaWindow)
	}
	return httpx.NewDomainQuotaLimiter(1, cfg.SearXNGDelay, cfg.SearXNGQuota, cfg.SearXNGQuotaWindow)
}

// searxngLeaseTTL sizes the in-flight lease to the longest a single Acquire can
// hold its slot. That is NOT the client timeout: the lease is booked before
// DoRetry and released after it returns, and DoRetry re-issues the request up to
// RetryConfig's default MaxAttempts with up to MaxDelay of backoff between
// tries. Sizing it to one timeout lets the lease expire under a still-live
// retrying request, whereupon the next acquire purges it and admits an extra
// caller — the cap is exceeded silently, and on this pool the cost of that is a
// 20-minute engine suspension.
//
// Both constants mirror httpx.RetryConfig.defaults(). They are duplicated rather
// than exported because a lease that is too LONG only delays crash recovery,
// while one that is too short breaks the cap: this must round up, and drifting
// out of date rounds it up further.
func searxngLeaseTTL(clientTimeout time.Duration) time.Duration {
	const (
		maxAttempts   = 3
		maxBackoff    = 4 * time.Second
		recoverySlack = 10 * time.Second
	)
	return time.Duration(maxAttempts)*clientTimeout +
		time.Duration(maxAttempts-1)*maxBackoff +
		recoverySlack
}

// buildLimiter wires one pacing scope ("domain" for crawling, "searxng" for
// queries). Without Redis the in-process limiter IS the limiter, exactly as
// before. With Redis it becomes the fail-open fallback behind a limiter whose
// state every replica shares, so one deployment enforces one limit instead of
// one per pod.
//
// rcfg carries the pacing settings for the scope; the client and the scope are
// filled in here so a caller cannot pair one scope's name with another's keys.
// OnWait is set on both CONCRETE limiters before either is wrapped: the Limiter
// interface has no such hook, and whichever backend serves an acquire is the one
// that measures its wait.
func buildLimiter(scope string, rdb redis.UniversalClient, local *httpx.DomainLimiter,
	rcfg redislimit.Config, metrics *observability.Metrics, logger *slog.Logger) pacer {
	local.OnWait = metrics.ObserveLimiterWait
	if rdb == nil {
		return local
	}

	// Pre-register the degraded gauge at 0 for this scope. OnDegraded fires on
	// transitions only, so without this write the series would not exist until
	// the first outage — a healthy deployment and a missing metric would look
	// the same to an alert. Only scopes that really have a FallbackLimiter get
	// the series: with Redis off a permanent 0 would claim a shared backend
	// that is not there.
	metrics.SetRatelimitDegraded(scope, false)

	rcfg.Client, rcfg.Scope = rdb, scope
	primary := redislimit.New(rcfg)
	primary.OnWait = metrics.ObserveLimiterWait
	primary.OnError = metrics.ObserveRatelimitBackendError
	// The counter alone cannot be triaged: release and penalize are
	// fire-and-forget, so their errors reach nothing else. WARN because pacing
	// accuracy is degraded but retrieval is not.
	primary.OnErrorDetail = func(op string, err error) {
		logger.Warn("rate limiter backend operation failed",
			"scope", scope, "op", op, "err", err)
	}

	return &httpx.FallbackLimiter{
		Primary:  primary,
		Fallback: local,
		OnDegraded: func(down bool) {
			metrics.SetRatelimitDegraded(scope, down)
			if down {
				logger.Warn("rate limiting degraded to per-pod pacing (redis unreachable)", "scope", scope)
				return
			}
			logger.Info("rate limiting shared again (redis reachable)", "scope", scope)
		},
	}
}

// redisClient builds the one Redis client both limiter scopes share, or
// (nil, nil, nil) when OMNIFEED_REDIS_URL is unset — the zero-config path, where
// pacing stays in process. A set but unparseable URL is a fatal config error: an
// operator who asked for shared pacing must not silently get per-pod pacing.
func redisClient(cfg config.Config, logger *slog.Logger) (redis.UniversalClient, func(), error) {
	if cfg.RedisURL == "" {
		logger.Info("rate limiting backend", "backend", "in-process")
		return nil, nil, nil
	}
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, nil, fmt.Errorf("OMNIFEED_REDIS_URL is not a valid redis URL: %w", err)
	}
	// One operation's budget, applied to every call the limiters make. Retries
	// are OFF (-1, since 0 means go-redis' default of 3): with them a dead Redis
	// costs ~1.7s per acquire instead of the ~250ms the fail-open cooldown is
	// sized for, and the fallback already covers the failure.
	opts.DialTimeout = cfg.RedisTimeout
	opts.ReadTimeout = cfg.RedisTimeout
	opts.WriteTimeout = cfg.RedisTimeout
	opts.MaxRetries = -1
	client := redis.NewClient(opts)
	logger.Info("rate limiting backend", "backend", "redis",
		"addr", opts.Addr, "db", opts.DB, "key_prefix", cfg.RedisKeyPrefix, "timeout", cfg.RedisTimeout)

	// Startup probe, never a readiness gate: pacing degrades to per-pod when
	// Redis is down, retrieval does not stop.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("redis unreachable at startup — pacing is per-pod until it answers", "err", err)
	} else {
		logger.Info("redis reachable — rate limiting state is shared across replicas")
	}
	return client, func() { _ = client.Close() }, nil
}

// dependencyChecks builds the periodic omnifeed_dependency_up probes: crawl4ai
// always (it is mandatory), SearXNG when search is configured, Redis when
// OMNIFEED_REDIS_URL is set. The probes use their own plain client, not the
// shared outbound one: no retries, no upstream metrics, and no trace per probe.
func dependencyChecks(cfg config.Config, rdb redis.UniversalClient, searxng bool) map[string]observability.DependencyCheck {
	client := &http.Client{Timeout: 5 * time.Second}
	checks := map[string]observability.DependencyCheck{
		"crawl4ai": upstreamReachable(client, cfg.Crawl4AIURL),
	}
	if searxng {
		checks["searxng"] = upstreamReachable(client, cfg.SearXNGURL+"/healthz")
	}
	if rdb != nil {
		checks["redis"] = func(ctx context.Context) error { return rdb.Ping(ctx).Err() }
	}
	return checks
}

// upstreamReachable reports failure only when the endpoint cannot be reached.
// Any HTTP status counts as up — even 405 means the server is listening.
func upstreamReachable(client *http.Client, endpoint string) observability.DependencyCheck {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
}

// fetchCache wraps the registry in the fetch_url response cache. Redis when
// OMNIFEED_REDIS_URL is set (the same client the limiters use, so one replica's
// fetch serves them all), else an in-process LRU of OMNIFEED_CACHE_MAX_BYTES.
// With Redis, a startup probe (and any later permission error) falls back to
// that LRU when the Redis user may not write the cache prefix.
// threadEngines get OMNIFEED_CACHE_TTL_THREADS; everything else (the generic
// page fallback) gets OMNIFEED_CACHE_TTL_PAGES.
func fetchCache(cfg config.Config, registry *engine.Registry, rdb redis.UniversalClient,
	metrics *observability.Metrics, logger *slog.Logger, threadEngines ...domain.Engine) *fetchcache.Cache {
	engineTTL := make(map[string]time.Duration, len(threadEngines))
	for _, e := range threadEngines {
		engineTTL[e.Name()] = cfg.CacheTTLThreads
	}

	metrics.SetCacheBytes(0)
	memory := fetchcache.NewMemory(fetchcache.MemoryConfig{
		MaxBytes:     cfg.CacheMaxBytes,
		MaxItemBytes: cfg.CacheMaxItemBytes,
		OnSize:       metrics.SetCacheBytes,
	})
	var backend fetchcache.Backend = memory
	backendName := fetchcache.BackendMemory
	if rdb != nil {
		// Probe before serving: a Redis user whose ACL does not cover the
		// cache prefix refuses every GET and SET, and the cache then silently
		// stores nothing. The Failover falls back to the in-process LRU above.
		failover := fetchcache.NewFailover(fetchcache.FailoverConfig{
			Redis: fetchcache.NewRedis(fetchcache.RedisConfig{
				Client:       rdb,
				Prefix:       cfg.CacheKeyPrefix,
				MaxItemBytes: cfg.CacheMaxItemBytes,
				OnError:      metrics.ObserveCacheBackendError,
			}),
			Memory:    memory,
			Prefix:    cfg.CacheKeyPrefix,
			Logger:    logger,
			OnBackend: metrics.SetCacheBackend,
		})
		failover.Probe(context.Background(), cfg.RedisTimeout)
		backend, backendName = failover, failover.Backend()
	} else {
		metrics.SetCacheBackend(fetchcache.BackendMemory)
	}
	logger.Info("fetch cache enabled", "backend", backendName,
		"ttl_threads", cfg.CacheTTLThreads, "ttl_pages", cfg.CacheTTLPages,
		"max_bytes", cfg.CacheMaxBytes, "max_item_bytes", cfg.CacheMaxItemBytes)
	return fetchcache.New(fetchcache.Config{
		Inner:      registry,
		Backend:    backend,
		DefaultTTL: cfg.CacheTTLPages,
		EngineTTL:  engineTTL,
		Metrics:    metrics,
		Logger:     logger,
	})
}
