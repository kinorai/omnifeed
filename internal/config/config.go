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
	// Crawl4AIExcludedSelector overrides the generic engine's chrome selector
	// list. Empty = the engine's conservative default; to effectively exclude
	// nothing, set a selector that matches nothing.
	Crawl4AIExcludedSelector string
	// Crawl4AITargetElements is a comma-separated CSS selector list. Empty (the
	// default) keeps the feature off; non-empty restricts extraction to matching
	// containers, which can yield no content at all on pages without them.
	Crawl4AITargetElements string
	// Crawl4AIScanFullPage makes the generic engine scroll the full page (in
	// Crawl4AIScrollDelay steps) before extraction, so lazy-loaded content
	// renders — at a multi-second cost on long pages. Off by default (crawl4ai's
	// own default): most agent fetches want the main content, not the infinite
	// scroll tail.
	Crawl4AIScanFullPage bool
	Crawl4AIScrollDelay  float64 // seconds between scroll steps when scanning the full page
	// Crawl4AIDelayBeforeHTML is the unconditional settle (seconds) crawl4ai
	// sleeps after the page-ready signal before extracting HTML — paid on every
	// crawl whether or not the page needs it. crawl4ai's own default (0.1).
	Crawl4AIDelayBeforeHTML float64
	// Crawl4AIRemoveOverlays sends crawl4ai's remove_overlay_elements. Its
	// geometry heuristic (delete any large absolute/fixed element) silently
	// empties pages whose content sits in such containers — Wikipedia and
	// several news fronts return only their <title>. Off by default;
	// remove_consent_popups stays on regardless and covers cookie modals.
	Crawl4AIRemoveOverlays bool

	// Upstream SearXNG (optional). Empty disables the `search` MCP tool.
	SearXNGURL     string
	SearXNGTimeout time.Duration

	// Search tool limits.
	SearchMaxResults int

	// FetchMaxChars is the default character cap on markdown content returned by
	// the fetch_url MCP tool (0 = unlimited). A caller-supplied max_chars wins.
	FetchMaxChars int

	// GitHubToken authenticates the GitHub engine's REST calls. Empty = anonymous
	// (60 requests/hour/IP); a token raises it to 5000/hour.
	GitHubToken string

	// DiscourseHosts is the exact-hostname allowlist the Discourse engine claims
	// topic URLs on. Discourse is self-hosted on arbitrary domains and Matches is
	// a pure predicate (it can't probe), so the list has to be explicit. Empty
	// (the variable set to "") means the engine claims nothing and every forum
	// goes to the generic browser fallback.
	DiscourseHosts []string

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

// defaultDiscourseHosts is the shipped Discourse allowlist: the large public
// forums an agent is most likely to be pointed at. Operators replace it with
// their own list via OMNIFEED_DISCOURSE_HOSTS.
const defaultDiscourseHosts = "meta.discourse.org,discuss.python.org,users.rust-lang.org,internals.rust-lang.org,discuss.pytorch.org"

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

		Crawl4AIExcludedSelector: env("OMNIFEED_CRAWL4AI_EXCLUDED_SELECTOR", ""),
		Crawl4AITargetElements:   env("OMNIFEED_CRAWL4AI_TARGET_ELEMENTS", ""),

		SearXNGURL:   env("OMNIFEED_SEARXNG_URL", ""),
		GitHubToken:  env("OMNIFEED_GITHUB_TOKEN", ""),
		RedditFormat: env("OMNIFEED_REDDIT_FORMAT", "toon"),
		RedditSort:   env("OMNIFEED_REDDIT_SORT", domain.DefaultRedditSort),
	}

	// Tri-state: unset = the shipped default list, set = that list verbatim,
	// set-but-empty = the engine claims nothing.
	hosts := defaultDiscourseHosts
	if v := envPtr("OMNIFEED_DISCOURSE_HOSTS"); v != nil {
		hosts = *v
	}
	c.DiscourseHosts = splitHosts(hosts)

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
	if c.Crawl4AIScanFullPage, err = envBool("OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE", false); err != nil {
		return c, err
	}
	if c.Crawl4AIScrollDelay, err = envFloat("OMNIFEED_CRAWL4AI_SCROLL_DELAY", 0.5); err != nil {
		return c, err
	}
	if c.Crawl4AIDelayBeforeHTML, err = envFloat("OMNIFEED_CRAWL4AI_DELAY_BEFORE_HTML", 0.1); err != nil {
		return c, err
	}
	if c.Crawl4AIRemoveOverlays, err = envBool("OMNIFEED_CRAWL4AI_REMOVE_OVERLAYS", false); err != nil {
		return c, err
	}
	if c.SearXNGTimeout, err = envDuration("OMNIFEED_SEARXNG_TIMEOUT", 15*time.Second); err != nil {
		return c, err
	}
	if c.SearchMaxResults, err = envInt("OMNIFEED_SEARCH_MAX_RESULTS", 25); err != nil {
		return c, err
	}
	if c.FetchMaxChars, err = envInt("OMNIFEED_FETCH_MAX_CHARS", 120000); err != nil {
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

	if c.SearchMaxResults < 1 || c.SearchMaxResults > 100 {
		return c, fmt.Errorf("OMNIFEED_SEARCH_MAX_RESULTS must be between 1 and 100, got %d", c.SearchMaxResults)
	}

	if c.FetchMaxChars < 0 {
		return c, fmt.Errorf("OMNIFEED_FETCH_MAX_CHARS must be >= 0 (0 = unlimited), got %d", c.FetchMaxChars)
	}

	if c.Crawl4AIPruneThreshold < 0 || c.Crawl4AIPruneThreshold > 1 {
		return c, fmt.Errorf("OMNIFEED_CRAWL4AI_PRUNE_THRESHOLD must be between 0 and 1, got %v", c.Crawl4AIPruneThreshold)
	}

	// 60s is crawl4ai's page_timeout clamp — a settle or scroll step longer than
	// the whole page budget is a misconfiguration, not a preference.
	if c.Crawl4AIDelayBeforeHTML < 0 || c.Crawl4AIDelayBeforeHTML > 60 {
		return c, fmt.Errorf("OMNIFEED_CRAWL4AI_DELAY_BEFORE_HTML must be between 0 and 60 seconds, got %v", c.Crawl4AIDelayBeforeHTML)
	}
	if c.Crawl4AIScrollDelay < 0 || c.Crawl4AIScrollDelay > 60 {
		return c, fmt.Errorf("OMNIFEED_CRAWL4AI_SCROLL_DELAY must be between 0 and 60 seconds, got %v", c.Crawl4AIScrollDelay)
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

// splitHosts parses a comma-separated hostname list, trimming whitespace,
// lowercasing, and dropping empty entries. Returns nil for an empty list.
func splitHosts(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envPtr returns nil when key is not present in the environment, and a pointer
// to its (possibly empty) value when it is — the only way to tell "operator said
// nothing, use the default" from "operator explicitly set it to empty".
func envPtr(key string) *string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	return &v
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
