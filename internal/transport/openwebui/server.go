// Package openwebui implements the HTTP transport that speaks Open WebUI's
// external-loader contract.
//
// Contract (from Open WebUI source):
//
//	POST <loader_url>/crawl
//	Headers: Authorization: Bearer <api_key>
//	Body:    {"urls": ["..."]}
//	Resp:    [{"page_content": "...", "metadata": {...}}, ...]
package openwebui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/kinorai/crawl4ai-reddit-proxy/internal/auth"
	"github.com/kinorai/crawl4ai-reddit-proxy/internal/domain"
	"github.com/kinorai/crawl4ai-reddit-proxy/internal/engine"
	"github.com/kinorai/crawl4ai-reddit-proxy/internal/engine/reddit"
	"github.com/kinorai/crawl4ai-reddit-proxy/internal/httpx"
	"github.com/kinorai/crawl4ai-reddit-proxy/internal/observability"
)

const maxBodySize = 1 << 20 // 1MB request body cap

// Server is the HTTP loader endpoint.
type Server struct {
	registry       *engine.Registry
	auth           auth.Authenticator
	logger         *slog.Logger
	metrics        *observability.Metrics
	maxURLsPerReq  int
	blockPrivate   bool
	redditDefaults reddit.Options
}

// Config configures the Server.
type Config struct {
	Registry          *engine.Registry
	Authenticator     auth.Authenticator
	Logger            *slog.Logger
	Metrics           *observability.Metrics
	MaxURLsPerRequest int
	BlockPrivateIPs   bool
	RedditDefaults    reddit.Options
}

// New constructs the server.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxURLsPerRequest <= 0 {
		cfg.MaxURLsPerRequest = 30
	}
	return &Server{
		registry:       cfg.Registry,
		auth:           cfg.Authenticator,
		logger:         cfg.Logger,
		metrics:        cfg.Metrics,
		maxURLsPerReq:  cfg.MaxURLsPerRequest,
		blockPrivate:   cfg.BlockPrivateIPs,
		redditDefaults: cfg.RedditDefaults,
	}
}

// Register attaches /crawl to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/crawl", s.crawl)
}

// --- Open WebUI wire types ---

type loaderRequest struct {
	URLs []string `json:"urls"`
}

type loaderDocument struct {
	PageContent string            `json:"page_content"`
	Metadata    map[string]string `json:"metadata"`
}

func (s *Server) crawl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	tenant, err := s.auth.Authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid or missing API key")
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxBodySize)
	defer func() { _ = body.Close() }()

	var req loaderRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.URLs) == 0 {
		writeError(w, http.StatusBadRequest, "urls array is empty")
		return
	}
	if len(req.URLs) > s.maxURLsPerReq {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("too many URLs (%d), max %d", len(req.URLs), s.maxURLsPerReq))
		return
	}

	for _, u := range req.URLs {
		if err := httpx.ValidateURL(u, s.blockPrivate); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid URL %q: %v", u, err))
			return
		}
	}

	opts := s.buildEngineOptions(r)

	results := make([]loaderDocument, len(req.URLs))
	var wg sync.WaitGroup
	for i, u := range req.URLs {
		wg.Add(1)
		go func(idx int, rawURL string) {
			defer wg.Done()
			results[idx] = s.crawlOne(r.Context(), rawURL, opts, tenant)
		}(i, u)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(results)
}

func (s *Server) crawlOne(ctx context.Context, rawURL string, opts domain.EngineOptions, tenant auth.TenantID) loaderDocument {
	start := time.Now()
	eng := s.registry.Resolve(rawURL)
	engName := "unknown"
	if eng != nil {
		engName = eng.Name()
	}

	doc, err := s.registry.Crawl(ctx, rawURL, opts)
	status := "ok"
	if err != nil {
		status = "error"
		s.logger.Warn("crawl failed", "url", rawURL, "engine", engName, "err", err)
		if s.metrics != nil {
			s.metrics.Observe(engName, string(tenant), status, time.Since(start))
		}
		return loaderDocument{
			PageContent: fmt.Sprintf("Error crawling URL: %v", err),
			Metadata:    map[string]string{"source": rawURL, "error": "true"},
		}
	}
	if s.metrics != nil {
		s.metrics.Observe(engName, string(tenant), status, time.Since(start))
	}
	return loaderDocument{PageContent: doc.PageContent, Metadata: doc.Metadata}
}

// buildEngineOptions translates query-string knobs into a domain.EngineOptions.
func (s *Server) buildEngineOptions(r *http.Request) domain.EngineOptions {
	q := r.URL.Query()
	opts := domain.EngineOptions{
		RedditFormat:      s.redditDefaults.Format,
		RedditKeepDepth:   s.redditDefaults.KeepDepth,
		RedditKeepCreated: s.redditDefaults.KeepCreated,
		RedditMaxRounds:   s.redditDefaults.MaxRounds,
	}
	if f := q.Get("format"); f == "json" || f == "toon" {
		opts.RedditFormat = f
	}
	if q.Get("depth") == "1" {
		opts.RedditKeepDepth = true
	}
	if q.Get("nocreated") == "1" {
		opts.RedditKeepCreated = false
	}
	if ex := q.Get("expand"); ex != "" {
		// Cap is enforced inside the reddit engine.
		switch ex {
		case "full", "all", "max":
			opts.RedditMaxRounds = reddit.MaxExpansionRounds
		default:
			// Engine will validate.
			opts.RedditMaxRounds = parseIntDefault(ex, opts.RedditMaxRounds)
		}
	}
	return opts
}

func parseIntDefault(s string, fallback int) int {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ErrServer is returned when the Server itself fails (vs a single URL).
var ErrServer = errors.New("openwebui server error")
