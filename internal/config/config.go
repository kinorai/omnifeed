// Package config loads all runtime configuration from OMNIFEED_-prefixed
// environment variables. Every knob the binary respects is declared here in
// one place so operators have a single source of truth.
package config

import (
	"fmt"
	"net/url"
	"os"
	"slices"
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
	// AllowedOrigins lists browser Origin values (scheme://host[:port])
	// allowed to call the HTTP APIs cross-origin, on top of the always-allowed
	// loopback origins. Requests without an Origin header (native clients)
	// are never affected. Empty = loopback-only.
	AllowedOrigins []string

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

	// SearXNGSiteEngines names the engines that run when the caller passes a
	// `site` filter. SearXNG hands `site:` to the engines instead of applying it
	// itself, and an engine that does not implement the operator either ignores
	// it (answering with unrelated pages) or returns nothing at all — so on a
	// mixed pool a site-scoped search comes back thin and noisy. Empty (the
	// default) queries the whole pool; set it to the subset that honours
	// `site:`, e.g. OMNIFEED_SEARXNG_SITE_ENGINES=privacywall,google cse
	SearXNGSiteEngines []string

	// SearXNGDelay and SearXNGQuota/SearXNGQuotaWindow pace queries to SearXNG.
	// They protect the ENGINES behind it, not SearXNG itself: one query fans out
	// to every enabled engine, so each engine sees exactly omnifeed's query
	// rate, and an engine that judges that rate bot-like suspends itself or
	// serves a CAPTCHA.
	//
	// Both controls are needed because engines enforce two different limits.
	// Measured on this deployment's pool (2026-08-17): the strictest engine kept
	// answering at a 3s spacing from a quiet start, but blocked after ~20
	// requests in ~85s — it counts requests in a window. A delay alone run
	// continuously would send 30 per 90s and trip it, so the delay shapes the
	// gap and the quota bounds the burst.
	//
	// Both default to off, which keeps the previous unpaced behaviour. Set them
	// to the values measured against your own pool.
	SearXNGDelay       time.Duration
	SearXNGQuota       int
	SearXNGQuotaWindow time.Duration

	// SearXNGConcurrency caps searches in flight across every replica. It exists
	// because SearXNGDelay alone cannot: a nonzero delay serializes admissions
	// cluster-wide, so without this the deployment runs one search at a time
	// however many replicas it has. See internal/httpx/redislimit's package doc.
	//
	// Measured on this deployment's pool (2026-08-24): 16 concurrent searches
	// ran clean from a cold start, and 24 blocked the strictest engine for the
	// 1200s its suspension lasts. Leave margin — the cost of overshooting is a
	// 20-minute engine outage, not a failed query.
	//
	// 0 keeps the pre-cap behaviour, where SearXNGDelay serializes.
	SearXNGConcurrency int

	// SearXNGMaxWait caps how long one query may sit in the pacing limiter
	// before it gives up with a timeout. It is the knob behind the
	// "context deadline exceeded" a fanned-out caller sees: the queue was
	// longer than this, not the upstream slower. Raise it to trade latency for
	// completions, lower it to push back on callers sooner. 0 uses the
	// searcher's own default.
	SearXNGMaxWait time.Duration

	// SearchAudit controls the per-search audit log: "off", "summary" or
	// "full". It is deliberately NOT a log level. A level answers "how bad is
	// this?", while this stream answers "what did the pool return?" — putting it
	// behind DEBUG would mean enabling every other component's debug output to
	// get it, and would invite sampling, which biases the per-engine statistics
	// it exists to produce. Both modes emit at INFO.
	//
	// off     nothing beyond the existing warnings (the default).
	// summary one line per search: query, filters, how many rows each engine
	//         contributed, and which engines were unresponsive.
	// full    adds one line per (engine, result) with that engine's own rank —
	//         the position table. ~10 lines per search.
	//
	// Both modes log the query text; only "full" adds the result URLs. That is
	// the most revealing data the service holds, so this is opt-in and its
	// retention is the operator's decision.
	//
	// Emitted at INFO through the shared logger, so anything other than "off"
	// requires OMNIFEED_LOG_LEVEL=debug or info — Load rejects the combination
	// rather than let a warn-level deployment discard the feed in silence.
	SearchAudit string

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

	// Distributed rate limiting (optional). RedisURL is the single opt-in
	// switch: unset keeps pacing entirely in process, exactly as before. Set,
	// the limiters share their state through Redis so every replica counts
	// against one limit instead of one limit each.
	RedisURL string
	// RedisKeyPrefix namespaces the limiter keys, so several deployments can
	// share one Redis instance without colliding.
	RedisKeyPrefix string
	// RedisTimeout is the budget for ONE Redis operation, not for one Acquire:
	// an Acquire legitimately sleeps minutes between attempts while it waits out
	// a quota window. On a breach the limiter fails open to in-process pacing
	// for a cooldown, so a dead Redis costs one timeout per cooldown.
	RedisTimeout time.Duration

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

// ValidSearchAudit is the complete set of OMNIFEED_SEARCH_AUDIT modes.
var ValidSearchAudit = []string{"off", "summary", "full"}

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

		SearXNGURL: env("OMNIFEED_SEARXNG_URL", ""),

		RedisURL:       env("OMNIFEED_REDIS_URL", ""),
		RedisKeyPrefix: env("OMNIFEED_REDIS_KEY_PREFIX", "omnifeed:ratelimit"),
		GitHubToken:    env("OMNIFEED_GITHUB_TOKEN", ""),
		RedditFormat:   env("OMNIFEED_REDDIT_FORMAT", "toon"),
		RedditSort:     env("OMNIFEED_REDDIT_SORT", domain.DefaultRedditSort),
	}

	// Tri-state: unset = the shipped default list, set = that list verbatim,
	// set-but-empty = the engine claims nothing.
	hosts := defaultDiscourseHosts
	if v := envPtr("OMNIFEED_DISCOURSE_HOSTS"); v != nil {
		hosts = *v
	}
	c.DiscourseHosts = splitHosts(hosts)

	// Origins are scheme://host[:port]; splitHosts' trim+lowercase is safe
	// on them because scheme and host are case-insensitive by RFC 3986.
	c.AllowedOrigins = splitHosts(env("OMNIFEED_ALLOWED_ORIGINS", ""))

	// SearXNG engine names are lowercase and may contain spaces ("google cse"),
	// which splitHosts' trim+lowercase preserves.
	c.SearXNGSiteEngines = splitHosts(env("OMNIFEED_SEARXNG_SITE_ENGINES", ""))

	c.SearchAudit = strings.ToLower(strings.TrimSpace(env("OMNIFEED_SEARCH_AUDIT", "off")))

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
	if c.SearXNGDelay, err = envDuration("OMNIFEED_SEARXNG_DELAY", 0); err != nil {
		return c, err
	}
	if c.SearXNGQuota, err = envInt("OMNIFEED_SEARXNG_QUOTA", 0); err != nil {
		return c, err
	}
	if c.SearXNGConcurrency, err = envInt("OMNIFEED_SEARXNG_CONCURRENCY", 0); err != nil {
		return c, err
	}
	if c.SearXNGMaxWait, err = envDuration("OMNIFEED_SEARXNG_MAX_WAIT", 0); err != nil {
		return c, err
	}
	if c.SearXNGQuotaWindow, err = envDuration("OMNIFEED_SEARXNG_QUOTA_WINDOW", 90*time.Second); err != nil {
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
	if c.RedisTimeout, err = envDuration("OMNIFEED_REDIS_TIMEOUT", 250*time.Millisecond); err != nil {
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
	if c.SearXNGDelay < 0 {
		return c, fmt.Errorf("OMNIFEED_SEARXNG_DELAY must be >= 0 (0 = no delay), got %s", c.SearXNGDelay)
	}
	if c.SearXNGMaxWait < 0 {
		return c, fmt.Errorf("OMNIFEED_SEARXNG_MAX_WAIT must be >= 0 (0 = default), got %s", c.SearXNGMaxWait)
	}
	if c.SearXNGConcurrency < 0 {
		return c, fmt.Errorf("OMNIFEED_SEARXNG_CONCURRENCY must be >= 0 (0 = serialize on delay), got %d", c.SearXNGConcurrency)
	}
	if c.SearXNGQuota < 0 {
		return c, fmt.Errorf("OMNIFEED_SEARXNG_QUOTA must be >= 0 (0 = no quota), got %d", c.SearXNGQuota)
	}
	// A quota without a window would divide by nothing: the pacing would silently
	// do nothing, which is the failure mode this whole change exists to remove.
	if c.SearXNGQuota > 0 && c.SearXNGQuotaWindow <= 0 {
		return c, fmt.Errorf("OMNIFEED_SEARXNG_QUOTA_WINDOW must be > 0 when OMNIFEED_SEARXNG_QUOTA is set, got %s", c.SearXNGQuotaWindow)
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

	if !slices.Contains(ValidSearchAudit, c.SearchAudit) {
		return c, fmt.Errorf("OMNIFEED_SEARCH_AUDIT must be one of %s, got %q",
			strings.Join(ValidSearchAudit, ", "), c.SearchAudit)
	}
	// The audit log is a data feed, not diagnostics — but it still travels
	// through the shared slog logger, which drops INFO records at warn/error.
	// Silently emitting nothing would be the worst outcome: the operator asked
	// for the feed, sees no error, and concludes the searcher is broken. Refuse
	// the combination instead of honouring half of it.
	if c.SearchAudit != "off" && !slices.Contains([]string{"debug", "info"}, strings.ToLower(c.LogLevel)) {
		return c, fmt.Errorf("OMNIFEED_SEARCH_AUDIT=%q needs OMNIFEED_LOG_LEVEL=debug or info (got %q): the audit log is emitted at INFO and a higher level discards it silently",
			c.SearchAudit, c.LogLevel)
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

	// The URL is only shape-checked here: config stays free of any Redis client,
	// and go-redis parses it for real at wiring time. A wrong scheme is the
	// mistake worth catching early — an http:// URL would otherwise fail deep
	// inside startup.
	if c.RedisURL != "" {
		u, err := url.Parse(c.RedisURL)
		if err != nil {
			return c, fmt.Errorf("OMNIFEED_REDIS_URL is not a valid URL: %w", err)
		}
		if u.Scheme != "redis" && u.Scheme != "rediss" {
			return c, fmt.Errorf("OMNIFEED_REDIS_URL scheme must be redis or rediss, got %q", u.Scheme)
		}
	}
	if c.RedisTimeout <= 0 {
		return c, fmt.Errorf("OMNIFEED_REDIS_TIMEOUT must be > 0, got %s", c.RedisTimeout)
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
