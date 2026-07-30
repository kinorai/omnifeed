// Package tools defines the MCP tools this proxy exposes — fetch_url and
// web_search. Each constructor binds a use case (the engine registry or a
// searcher) to an mcp.Tool, so the MCP server stays a pure JSON-RPC transport
// and adding a tool never touches the protocol plumbing.
package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/engine/reddit"
	"github.com/kinorai/omnifeed/internal/observability"
	"github.com/kinorai/omnifeed/internal/transport/mcp"
)

// mcpTenant labels MCP-originated calls in metrics; the MCP transport has a
// single shared bearer, so there is no finer-grained tenant to report.
const mcpTenant = "mcp"

// defaultSearchLimit is the result count when the caller omits `limit`.
const defaultSearchLimit = 10

// MaxFetchChars is the ceiling on fetch_url's `max_chars`, and the value
// declared to clients as `anthropic/maxResultSizeChars`. 500000 is the maximum
// that annotation accepts, so a caller can never ask for more text than the
// client is willing to render.
const MaxFetchChars = 500000

// FetchURL returns the `fetch_url` tool: URL → LLM-friendly content via the
// engine registry (Reddit engine for reddit.com, Hacker News engine for
// news.ycombinator.com, crawl4ai fallback for the rest).
// defaultMaxChars caps markdown content when the caller omits `max_chars`
// (0 = unlimited); it comes from OMNIFEED_FETCH_MAX_CHARS.
func FetchURL(reg *engine.Registry, defaults reddit.Options, metrics *observability.Metrics, defaultMaxChars int) mcp.Tool {
	return mcp.Tool{
		Name:        "fetch_url",
		Description: "Fetch any URL and return LLM-friendly content. You MUST use it for Reddit and Hacker News URLs.",
		// Read-only and open-world: fetches external pages without mutating
		// anything, so clients can auto-approve it.
		Annotations: map[string]any{
			"readOnlyHint":  true,
			"openWorldHint": true,
		},
		// Raise the client's per-tool text budget to this tool's own ceiling so a
		// large page is truncated by max_chars (with a resumable marker) rather
		// than silently spooled to a file by the client.
		Meta: map[string]any{
			"anthropic/maxResultSizeChars": MaxFetchChars,
		},
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
				"limit": map[string]any{
					"type":        "integer",
					"description": "Reddit-only: max comments to fetch in the initial tree (Reddit `limit`).",
				},
				"depth": map[string]any{
					"type":        "integer",
					"description": "Reddit-only: max nesting depth of the comment tree (Reddit `depth`).",
				},
				"sort": map[string]any{
					"type":        "string",
					"description": "Reddit-only: comment sort order ('confidence' = best).",
					"enum":        domain.ValidRedditSorts,
				},
				"max_comments": map[string]any{
					"type":        "integer",
					"description": "Reddit-only: hard cap on total comments after expansion (0 = unlimited).",
				},
				"max_top_level": map[string]any{
					"type":        "integer",
					"description": "Reddit-only: hard cap on top-level comment threads (0 = unlimited).",
				},
				"max_chars": map[string]any{
					"type": "integer",
					"description": "Markdown content only (ignored for Reddit/Hacker News TOON or JSON): max characters to return, up to " +
						strconv.Itoa(MaxFetchChars) + ". 0 or omitted uses the server default (" + strconv.Itoa(defaultMaxChars) +
						", 0 = unlimited). When content is cut, the reply ends with a marker giving the offset to resume from.",
					"minimum": 0,
				},
				"start_char": map[string]any{
					"type": "integer",
					"description": "Markdown content only: character offset to start from (default 0). Use the start_char value from a " +
						"truncation marker to read the next chunk of the same page.",
					"minimum": 0,
				},
			},
		},
		Handle: crawlHandler(reg, defaults, metrics, defaultMaxChars),
	}
}

func crawlHandler(reg *engine.Registry, defaults reddit.Options, metrics *observability.Metrics, defaultMaxChars int) func(context.Context, map[string]any) (mcp.ToolResult, error) {
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
			RedditFetchLimit:  defaults.FetchLimit,
			RedditDepth:       defaults.Depth,
			RedditSort:        defaults.Sort,
			RedditMaxComments: defaults.MaxComments,
			RedditMaxTopLevel: defaults.MaxTopLevel,
		}
		if f, isString := args["format"].(string); isString && (f == "toon" || f == "json") {
			opts.RedditFormat = f
		}
		if ex, isNumber := args["expand"].(float64); isNumber && ex >= 0 {
			opts.RedditMaxRounds = int(ex)
		}
		if l, isNumber := args["limit"].(float64); isNumber && l >= 1 {
			opts.RedditFetchLimit = int(l)
		}
		if d, isNumber := args["depth"].(float64); isNumber && d >= 1 {
			opts.RedditDepth = int(d)
		}
		if s, isString := args["sort"].(string); isString && domain.ValidRedditSort(s) {
			opts.RedditSort = s
		}
		if mc, isNumber := args["max_comments"].(float64); isNumber && mc >= 0 {
			opts.RedditMaxComments = int(mc)
		}
		if mt, isNumber := args["max_top_level"].(float64); isNumber && mt >= 0 {
			opts.RedditMaxTopLevel = int(mt)
		}

		start := time.Now()
		doc, err := reg.Crawl(ctx, rawURL, opts)
		observe(metrics, engineName(reg, rawURL), err, start)
		if err != nil {
			return mcp.ToolResult{}, err
		}
		return sized(doc, resolveMaxChars(args, defaultMaxChars), argInt(args, "start_char")), nil
	}
}

// resolveMaxChars picks the character cap for this call: a positive caller
// `max_chars` wins (clamped to MaxFetchChars), otherwise the server default.
func resolveMaxChars(args map[string]any, defaultMaxChars int) int {
	if n := argInt(args, "max_chars"); n > 0 {
		return min(n, MaxFetchChars)
	}
	return defaultMaxChars
}

func argInt(args map[string]any, key string) int {
	if n, isNumber := args[key].(float64); isNumber && n > 0 {
		return int(n)
	}
	return 0
}

// sized applies character-window truncation to markdown documents and mirrors
// the offsets into the result _meta so the caller can resume. Structured
// content (TOON/JSON) is returned whole — see domain.TruncatableContentType.
func sized(doc domain.Document, maxChars, startChar int) mcp.ToolResult {
	if !domain.TruncatableContentType(doc.Metadata[domain.ContentTypeKey]) || (maxChars <= 0 && startChar == 0) {
		return mcp.ToolResult{Text: doc.PageContent, Meta: doc.Metadata}
	}

	t := domain.TruncateContent(doc.PageContent, maxChars, startChar)
	if !t.Truncated && t.ReturnedChars == t.TotalChars {
		return mcp.ToolResult{Text: doc.PageContent, Meta: doc.Metadata}
	}

	meta := make(map[string]string, len(doc.Metadata)+4)
	for k, v := range doc.Metadata {
		meta[k] = v
	}
	meta["truncated"] = strconv.FormatBool(t.Truncated)
	meta["total_chars"] = strconv.Itoa(t.TotalChars)
	meta["returned_chars"] = strconv.Itoa(t.ReturnedChars)
	meta["next_start_char"] = strconv.Itoa(t.NextStartChar)
	return mcp.ToolResult{Text: t.Text, Meta: meta}
}

// WebSearch returns the `web_search` tool: query → result URLs via the configured
// Searcher (SearXNG). Results feed the `fetch_url` tool, which renders any
// returned URL — reddit.com hits come back as full TOON comment trees.
func WebSearch(searcher domain.Searcher, maxResults int, metrics *observability.Metrics) mcp.Tool {
	return mcp.Tool{
		Name:        "web_search",
		Description: "Search the whole web and return result URLs with titles and snippets. You MUST use it for Reddit and Hacker News. Follow up with `fetch_url` to read a result.",
		// Read-only and open-world (see fetch_url).
		Annotations: map[string]any{
			"readOnlyHint":  true,
			"openWorldHint": true,
		},
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
					"enum":        domain.ValidTimeRanges,
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
				if !domain.ValidTimeRange(tr) {
					return mcp.ToolResult{}, mcp.InvalidParams("time_range must be one of: day, week, month, year")
				}
				opts.TimeRange = tr
			}
			if lang, isString := args["language"].(string); isString {
				opts.Language = lang
			}

			start := time.Now()
			results, err := searcher.Search(ctx, query, opts)
			if metrics != nil {
				metrics.ObserveSearch(searcher.Name(), observability.StatusOf(err), observability.Reason(err), time.Since(start))
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
	metrics.Observe(engine, mcpTenant, observability.StatusOf(err), observability.Reason(err), time.Since(start))
}

func engineName(reg *engine.Registry, rawURL string) string {
	if e := reg.Resolve(rawURL); e != nil {
		return e.Name()
	}
	return "unknown"
}
