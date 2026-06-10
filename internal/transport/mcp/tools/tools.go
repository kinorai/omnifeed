// Package tools defines the MCP tools this proxy exposes — crawl,
// reddit_get_post, and search. Each constructor binds a use case (the engine
// registry or a searcher) to an mcp.Tool, so the MCP server stays a pure
// JSON-RPC transport and adding a tool never touches the protocol plumbing.
package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/kinorai/search-crawl-reddit-proxy/internal/domain"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/engine"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/engine/reddit"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/observability"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/transport/mcp"
)

// mcpTenant labels MCP-originated calls in metrics; the MCP transport has a
// single shared bearer, so there is no finer-grained tenant to report.
const mcpTenant = "mcp"

// defaultSearchLimit is the result count when the caller omits `limit`.
const defaultSearchLimit = 10

// Crawl returns the `crawl` tool: URL → LLM-friendly content via the engine
// registry (Reddit engine for reddit.com, crawl4ai fallback for the rest).
func Crawl(reg *engine.Registry, defaults reddit.Options, metrics *observability.Metrics) mcp.Tool {
	return mcp.Tool{
		Name:        "crawl",
		Description: "Fetch a URL and return LLM-friendly content. Reddit URLs return TOON-encoded comment trees with full /api/morechildren expansion. Other URLs are routed through the upstream crawl4ai instance and returned as filtered markdown.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"url"},
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "Absolute http(s) URL to crawl.",
				},
				"format": map[string]any{
					"type":        "string",
					"description": "Reddit-only: 'toon' (default, token-efficient) or 'json'.",
					"enum":        []string{"toon", "json"},
				},
				"expand": map[string]any{
					"type":        "integer",
					"description": "Reddit-only: number of /api/morechildren expansion rounds (0-40).",
				},
			},
		},
		Handle: crawlHandler(reg, defaults, metrics),
	}
}

// RedditGetPost returns the `reddit_get_post` tool. It shares the crawl
// handler — the registry routes reddit.com URLs to the Reddit engine — and
// exists as a separate tool so Reddit-focused clients get a dedicated,
// discoverable entry point.
func RedditGetPost(reg *engine.Registry, defaults reddit.Options, metrics *observability.Metrics) mcp.Tool {
	return mcp.Tool{
		Name:        "reddit_get_post",
		Description: "Reddit-only: fetch a thread by permalink and return the full comment tree as TOON. Equivalent to `crawl` on a reddit.com URL.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"url"},
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "Reddit post URL or /r/.../comments/... permalink.",
				},
				"expand": map[string]any{
					"type":        "integer",
					"description": "Number of expansion rounds (default 3, max 40).",
				},
			},
		},
		Handle: crawlHandler(reg, defaults, metrics),
	}
}

func crawlHandler(reg *engine.Registry, defaults reddit.Options, metrics *observability.Metrics) func(context.Context, map[string]any) (mcp.ToolResult, error) {
	return func(ctx context.Context, args map[string]any) (mcp.ToolResult, error) {
		rawURL, _ := args["url"].(string)
		if rawURL == "" {
			return mcp.ToolResult{}, mcp.InvalidParams("missing required argument: url")
		}

		opts := domain.EngineOptions{
			RedditFormat:      defaults.Format,
			RedditKeepDepth:   defaults.KeepDepth,
			RedditKeepCreated: defaults.KeepCreated,
			RedditMaxRounds:   defaults.MaxRounds,
		}
		if f, isString := args["format"].(string); isString && (f == "toon" || f == "json") {
			opts.RedditFormat = f
		}
		if ex, isNumber := args["expand"].(float64); isNumber && ex >= 0 {
			opts.RedditMaxRounds = int(ex)
		}

		start := time.Now()
		doc, err := reg.Crawl(ctx, rawURL, opts)
		observe(metrics, engineName(reg, rawURL), err, start)
		if err != nil {
			return mcp.ToolResult{}, err
		}
		return mcp.ToolResult{Text: doc.PageContent, Meta: doc.Metadata}, nil
	}
}

// Search returns the `search` tool: query → result URLs via the configured
// Searcher (SearXNG). Results feed the `crawl` tool, which renders any
// returned URL — reddit.com hits come back as full TOON comment trees.
func Search(searcher domain.Searcher, maxResults int, metrics *observability.Metrics) mcp.Tool {
	return mcp.Tool{
		Name:        "search",
		Description: "Search the web through the self-hosted SearXNG instance and return result URLs with titles and snippets as JSON. Reddit threads surface via the general engines (Google/Bing/DDG). Follow up with `crawl` on any returned URL to read it.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"query"},
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Search query.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max results to return (1-" + strconv.Itoa(maxResults) + ", default " + strconv.Itoa(defaultSearchLimit) + ").",
				},
				"time_range": map[string]any{
					"type":        "string",
					"description": "Restrict results to a recency window.",
					"enum":        []string{"day", "week", "month", "year"},
				},
				"language": map[string]any{
					"type":        "string",
					"description": "Result language code, e.g. 'en' or 'fr'. Default: upstream setting.",
				},
			},
		},
		Handle: func(ctx context.Context, args map[string]any) (mcp.ToolResult, error) {
			query, _ := args["query"].(string)
			if query == "" {
				return mcp.ToolResult{}, mcp.InvalidParams("missing required argument: query")
			}

			opts := domain.SearchOptions{Limit: defaultSearchLimit}
			if l, isNumber := args["limit"].(float64); isNumber && l >= 1 {
				opts.Limit = int(l)
			}
			if opts.Limit > maxResults {
				opts.Limit = maxResults
			}
			if tr, isString := args["time_range"].(string); isString && tr != "" {
				switch tr {
				case "day", "week", "month", "year":
					opts.TimeRange = tr
				default:
					return mcp.ToolResult{}, mcp.InvalidParams("time_range must be one of: day, week, month, year")
				}
			}
			if lang, isString := args["language"].(string); isString {
				opts.Language = lang
			}

			start := time.Now()
			results, err := searcher.Search(ctx, query, opts)
			if metrics != nil {
				metrics.ObserveSearch(searcher.Name(), statusOf(err), time.Since(start))
			}
			if err != nil {
				return mcp.ToolResult{}, err
			}

			text, err := json.Marshal(results)
			if err != nil {
				return mcp.ToolResult{}, err
			}
			return mcp.ToolResult{
				Text: string(text),
				Meta: map[string]string{
					"query":    query,
					"count":    strconv.Itoa(len(results)),
					"searcher": searcher.Name(),
				},
			}, nil
		},
	}
}

func observe(metrics *observability.Metrics, engine string, err error, start time.Time) {
	if metrics == nil {
		return
	}
	metrics.Observe(engine, mcpTenant, statusOf(err), time.Since(start))
}

func engineName(reg *engine.Registry, rawURL string) string {
	if e := reg.Resolve(rawURL); e != nil {
		return e.Name()
	}
	return "unknown"
}

func statusOf(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
