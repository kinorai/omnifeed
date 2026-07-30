// Package config loads all runtime configuration from OMNIFEED_-prefixed
// environment variables. Every knob the binary respects is declared here in
// one place so operators have a single source of truth.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	// HTTP loader (Open WebUI contract).
	ListenAddr string

	// MCP transports.
	MCPListenAddr string
	MCPStdio      bool

	// Observability.
	MetricsAddr string
	LogLevel    string
	LogFormat   string
	EnablePprof bool

	// Auth.
	APIKey      string
	AllowNoAuth bool

	// Upstream crawl4ai.
	Crawl4AIURL            string
	Crawl4AITimeout        time.Duration
	Crawl4AIKeepLinks      bool    // render hyperlink anchor text + keep external links in markdown
	Crawl4AIPruneThreshold float64 // PruningContentFilter cutoff for the generic engine (0–1; higher strips more boilerplate)
	Crawl4AIWaitUntil      string  // page-ready signal for the generic engine: domcontentloaded (default) | load | networkidle | commit
	Crawl4AIToken          string  // bearer token sent to crawl4ai (its CRAWL4AI_API_TOKEN); empty = no Authorization header

	// Upstream SearXNG (optional). Empty disables the `search` MCP tool.
	SearXNGURL     string
	SearXNGTimeout time.Duration
	// SearXNGDegradedRetryDelay is the wait before the single retry of a
	// degraded search (200 + zero results + unresponsive engines). 0 disables
	// the retry.
	SearXNGDegradedRetryDelay time.Duration

	// Search tool limits.
	SearchMaxResults int

	// Reddit engine defaults.
	RedditTimeout     time.Duration
	RedditMaxRounds   int
	RedditFormat      string
	RedditFetchLimit  int    // Reddit `limit` query param: max comments in initial fetch
	RedditDepth       int    // Reddit `depth` query param: max comment-tree nesting
	RedditSort        string // Reddit `sort` query param: comment sort order
	RedditMaxComments int    // hard cap on total comments emitted (0 = unlimited)
	RedditMaxTopLevel int    // hard cap on top-level comment threads (0 = unlimited)
	RedditKeepCreated bool   // include the per-comment `created` timestamp
	RedditKeepDepth   bool   // include the per-comment `depth` field

	// Limits and rate control.
	MaxURLsPerRequest    int
	PerDomainConcurrency int
	PerDomainDelay       time.Duration
	BlockPrivateIPs      bool
}

// Load reads OMNIFEED_* env vars and returns a populated Config, or an error if a
// required variable is malformed. Defaults are documented inline.
func Load() (Config, error) {
	c := Config{
		ListenAddr:        env("OMNIFEED_LISTEN_ADDR", ":8080"),
		MCPListenAddr:     env("OMNIFEED_MCP_LISTEN_ADDR", ":8081"),
		MetricsAddr:       env("OMNIFEED_METRICS_ADDR", ":9090"),
		LogLevel:          env("OMNIFEED_LOG_LEVEL", "info"),
		LogFormat:         env("OMNIFEED_LOG_FORMAT", "json"),
		APIKey:            os.Getenv("OMNIFEED_API_KEY"),
		Crawl4AIURL:       env("OMNIFEED_CRAWL4AI_URL", ""),
		Crawl4AIWaitUntil: env("OMNIFEED_CRAWL4AI_WAIT_UNTIL", "domcontentloaded"),
		Crawl4AIToken:     env("OMNIFEED_CRAWL4AI_TOKEN", ""),
		SearXNGURL:        env("OMNIFEED_SEARXNG_URL", ""),
		RedditFormat:      env("OMNIFEED_REDDIT_FORMAT", "toon"),
		RedditSort:        env("OMNIFEED_REDDIT_SORT", domain.DefaultRedditSort),
	}

	var err error
	if c.MCPStdio, err = envBool("OMNIFEED_MCP_STDIO", false); err != nil {
		return c, err
	}
	if c.EnablePprof, err = envBool("OMNIFEED_ENABLE_PPROF", false); err != nil {
		return c, err
	}
	if c.AllowNoAuth, err = envBool("OMNIFEED_DEV_NO_AUTH", false); err != nil {
		return c, err
	}
	if c.BlockPrivateIPs, err = envBool("OMNIFEED_BLOCK_PRIVATE_IPS", true); err != nil {
		return c, err
	}
	if c.Crawl4AITimeout, err = envDuration("OMNIFEED_CRAWL4AI_TIMEOUT", 90*time.Second); err != nil {
		return c, err
	}
	if c.Crawl4AIKeepLinks, err = envBool("OMNIFEED_CRAWL4AI_KEEP_LINKS", true); err != nil {
		return c, err
	}
	if c.Crawl4AIPruneThreshold, err = envFloat("OMNIFEED_CRAWL4AI_PRUNE_THRESHOLD", 0.48); err != nil {
		return c, err
	}
	if c.SearXNGTimeout, err = envDuration("OMNIFEED_SEARXNG_TIMEOUT", 15*time.Second); err != nil {
		return c, err
	}
	if c.SearXNGDegradedRetryDelay, err = envDuration("OMNIFEED_SEARXNG_DEGRADED_RETRY_DELAY", 2*time.Second); err != nil {
		return c, err
	}
	if c.SearchMaxResults, err = envInt("OMNIFEED_SEARCH_MAX_RESULTS", 25); err != nil {
		return c, err
	}
	if c.RedditTimeout, err = envDuration("OMNIFEED_REDDIT_TIMEOUT", 4*time.Minute); err != nil {
		return c, err
	}
	if c.RedditMaxRounds, err = envInt("OMNIFEED_REDDIT_MAX_ROUNDS", 3); err != nil {
		return c, err
	}
	if c.RedditFetchLimit, err = envInt("OMNIFEED_REDDIT_FETCH_LIMIT", domain.DefaultRedditFetchLimit); err != nil {
		return c, err
	}
	if c.RedditDepth, err = envInt("OMNIFEED_REDDIT_DEPTH", domain.DefaultRedditDepth); err != nil {
		return c, err
	}
	if c.RedditMaxComments, err = envInt("OMNIFEED_REDDIT_MAX_COMMENTS", 0); err != nil {
		return c, err
	}
	if c.RedditMaxTopLevel, err = envInt("OMNIFEED_REDDIT_MAX_TOP_LEVEL", 0); err != nil {
		return c, err
	}
	if c.RedditKeepCreated, err = envBool("OMNIFEED_REDDIT_KEEP_CREATED", true); err != nil {
		return c, err
	}
	if c.RedditKeepDepth, err = envBool("OMNIFEED_REDDIT_KEEP_DEPTH", false); err != nil {
		return c, err
	}
	if c.MaxURLsPerRequest, err = envInt("OMNIFEED_MAX_URLS_PER_REQUEST", 30); err != nil {
		return c, err
	}
	if c.PerDomainConcurrency, err = envInt("OMNIFEED_PER_DOMAIN_CONCURRENCY", 2); err != nil {
		return c, err
	}
	if c.PerDomainDelay, err = envDuration("OMNIFEED_PER_DOMAIN_DELAY", 1500*time.Millisecond); err != nil {
		return c, err
	}

	c.RedditFormat = strings.ToLower(c.RedditFormat)
	if c.RedditFormat != "toon" && c.RedditFormat != "json" {
		return c, fmt.Errorf("OMNIFEED_REDDIT_FORMAT must be 'toon' or 'json', got %q", c.RedditFormat)
	}

	c.RedditSort = strings.ToLower(c.RedditSort)
	if !domain.ValidRedditSort(c.RedditSort) {
		return c, fmt.Errorf("OMNIFEED_REDDIT_SORT must be one of %s, got %q", strings.Join(domain.ValidRedditSorts, ", "), c.RedditSort)
	}
	if c.RedditFetchLimit < 1 {
		return c, fmt.Errorf("OMNIFEED_REDDIT_FETCH_LIMIT must be >= 1, got %d", c.RedditFetchLimit)
	}
	if c.RedditDepth < 1 {
		return c, fmt.Errorf("OMNIFEED_REDDIT_DEPTH must be >= 1, got %d", c.RedditDepth)
	}
	if c.RedditMaxComments < 0 {
		return c, fmt.Errorf("OMNIFEED_REDDIT_MAX_COMMENTS must be >= 0 (0 = unlimited), got %d", c.RedditMaxComments)
	}
	if c.RedditMaxTopLevel < 0 {
		return c, fmt.Errorf("OMNIFEED_REDDIT_MAX_TOP_LEVEL must be >= 0 (0 = unlimited), got %d", c.RedditMaxTopLevel)
	}

	if c.SearXNGDegradedRetryDelay < 0 {
		return c, fmt.Errorf("OMNIFEED_SEARXNG_DEGRADED_RETRY_DELAY must be >= 0 (0 disables the retry), got %v", c.SearXNGDegradedRetryDelay)
	}

	if c.SearchMaxResults < 1 || c.SearchMaxResults > 100 {
		return c, fmt.Errorf("OMNIFEED_SEARCH_MAX_RESULTS must be between 1 and 100, got %d", c.SearchMaxResults)
	}

	if c.Crawl4AIPruneThreshold < 0 || c.Crawl4AIPruneThreshold > 1 {
		return c, fmt.Errorf("OMNIFEED_CRAWL4AI_PRUNE_THRESHOLD must be between 0 and 1, got %v", c.Crawl4AIPruneThreshold)
	}

	switch c.Crawl4AIWaitUntil {
	case "domcontentloaded", "load", "networkidle", "commit":
	default:
		return c, fmt.Errorf("OMNIFEED_CRAWL4AI_WAIT_UNTIL must be one of domcontentloaded, load, networkidle, commit, got %q", c.Crawl4AIWaitUntil)
	}

	// crawl4ai is required for ALL engines now: the Reddit engine fetches
	// through crawl4ai's headless browser (Reddit blocks non-browser clients),
	// and the generic fallback obviously needs it too. Fail fast rather than
	// reporting healthy while every crawl errors.
	if c.Crawl4AIURL == "" {
		return c, fmt.Errorf("OMNIFEED_CRAWL4AI_URL is required: every engine (Reddit and the generic fallback) fetches through crawl4ai")
	}

	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("env %s: %w", key, err)
	}
	return b, nil
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("env %s: %w", key, err)
	}
	return n, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("env %s: %w", key, err)
	}
	return f, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("env %s: %w", key, err)
	}
	return d, nil
}
