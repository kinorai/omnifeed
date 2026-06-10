// Command search-crawl-reddit-proxy is the entry point. It loads SCRM_*
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

	"github.com/kinorai/search-crawl-reddit-proxy/internal/auth"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/config"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/domain"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/engine"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/engine/crawl4ai"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/engine/reddit"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/httpx"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/observability"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/search/searxng"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/transport/mcp"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/transport/mcp/tools"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/transport/openwebui"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/version"
)

func main() {
	var mcpStdio bool
	flag.BoolVar(&mcpStdio, "mcp-stdio", false, "Run MCP over stdio (alternative to SCRM_MCP_STDIO=true)")
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

	httpClient := httpx.New(&http.Client{Timeout: cfg.Crawl4AITimeout})
	limiter := httpx.NewDomainLimiter(cfg.PerDomainConcurrency, cfg.PerDomainDelay)

	// --- Engines ---

	redditDefaults := reddit.Options{
		KeepDepth:   false,
		KeepCreated: true,
		MaxRounds:   cfg.RedditMaxRounds,
		Format:      cfg.RedditFormat,
	}

	// The Reddit engine fetches through crawl4ai (a real browser) because
	// Reddit's edge blocks non-browser HTTP clients — see reddit.Fetcher.
	redditEngine := reddit.New(reddit.Config{
		Fetcher:     reddit.NewFetcher(httpClient, cfg.Crawl4AIURL),
		Limiter:     limiter,
		Timeout:     cfg.RedditTimeout,
		DefaultOpts: redditDefaults,
		Logger:      logger,
	})
	crawl4aiEngine := crawl4ai.New(crawl4ai.Config{
		Endpoint: cfg.Crawl4AIURL,
		Client:   httpClient,
		Limiter:  limiter,
	})

	registry := engine.New().
		Register(redditEngine).
		Fallback(crawl4aiEngine).
		BlockPrivateIPs(cfg.BlockPrivateIPs)

	// --- Searcher (optional — search tool is exposed only when configured) ---

	var searcher domain.Searcher
	var searxngClient *httpx.Client
	if cfg.SearXNGURL != "" {
		searxngClient = httpx.New(&http.Client{Timeout: cfg.SearXNGTimeout})
		searcher = searxng.New(searxng.Config{
			Endpoint: cfg.SearXNGURL,
			Client:   searxngClient,
		})
	} else {
		logger.Info("search tool disabled (SCRM_SEARXNG_URL not set)")
	}

	// --- Metrics ---

	metrics := observability.NewMetrics()

	// --- MCP tools (shared by the stdio and HTTP transports) ---

	mcpTools := []mcp.Tool{
		tools.Crawl(registry, redditDefaults, metrics),
		tools.RedditGetPost(registry, redditDefaults, metrics),
	}
	if searcher != nil {
		mcpTools = append(mcpTools, tools.Search(searcher, cfg.SearchMaxResults, metrics))
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
	// would mean a fully unauthenticated public proxy. SCRM_DEV_NO_AUTH=true is
	// the explicit local-development opt-out.
	var authn auth.Authenticator
	switch {
	case cfg.APIKey != "":
		if cfg.AllowNoAuth {
			logger.Warn("SCRM_DEV_NO_AUTH=true ignored because SCRM_API_KEY is set — authentication is enabled")
		}
		authn = auth.NewSharedBearer(cfg.APIKey)
		logger.Info("api key authentication enabled")
	case cfg.AllowNoAuth:
		authn = auth.AlwaysAllow{}
		logger.Warn("SCRM_API_KEY not set and SCRM_DEV_NO_AUTH=true — HTTP transports are UNAUTHENTICATED")
	default:
		return fmt.Errorf("SCRM_API_KEY is not set: refusing to start the HTTP transports unauthenticated. " +
			"Set SCRM_API_KEY=<token> to enable bearer-token auth, or SCRM_DEV_NO_AUTH=true to run without auth (local/dev only)")
	}

	// --- HTTP server (Open WebUI loader + MCP HTTP) ---

	loaderServer := openwebui.New(openwebui.Config{
		Registry:          registry,
		Authenticator:     authn,
		Logger:            logger,
		Metrics:           metrics,
		MaxURLsPerRequest: cfg.MaxURLsPerRequest,
		BlockPrivateIPs:   cfg.BlockPrivateIPs,
		RedditDefaults:    redditDefaults,
	})

	mainMux := http.NewServeMux()
	loaderServer.Register(mainMux)

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

	servers := []serverSpec{
		{name: "loader", addr: cfg.ListenAddr, handler: mainMux, writeTimeout: 300 * time.Second},
		{name: "mcp", addr: cfg.MCPListenAddr, handler: mcpMux, writeTimeout: 300 * time.Second},
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
