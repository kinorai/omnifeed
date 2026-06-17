// Package searchapi exposes the web-search use case over plain HTTP/JSON, so
// non-MCP clients (scripts, RAG pipelines, n8n, ...) can run the same
// query → result-URLs search the `web_search` MCP tool provides — no MCP client
// required. It is mounted only when a Searcher is configured.
//
//	POST /search
//	Headers: Authorization: Bearer <api_key>
//	Body:    {"query": "...", "limit": 10, "time_range": "week", "language": "en"}
//	Resp:    [{"title","url","snippet","engine","published_date"}, ...]
package searchapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/kinorai/omnifeed/internal/auth"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
)

const (
	maxBodySize        = 1 << 16 // 64KB — search requests are tiny
	defaultSearchLimit = 10
)

// Server is the REST search endpoint.
type Server struct {
	searcher   domain.Searcher
	auth       auth.Authenticator
	logger     *slog.Logger
	metrics    *observability.Metrics
	maxResults int
}

// Config configures the Server.
type Config struct {
	Searcher      domain.Searcher
	Authenticator auth.Authenticator
	Logger        *slog.Logger
	Metrics       *observability.Metrics
	MaxResults    int
}

// New constructs the server.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxResults <= 0 {
		cfg.MaxResults = 25
	}
	return &Server{
		searcher:   cfg.Searcher,
		auth:       cfg.Authenticator,
		logger:     cfg.Logger,
		metrics:    cfg.Metrics,
		maxResults: cfg.MaxResults,
	}
}

// Register attaches /search to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/search", s.search)
}

type searchRequest struct {
	Query     string `json:"query"`
	Limit     int    `json:"limit"`
	TimeRange string `json:"time_range"`
	Language  string `json:"language"`
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if _, err := s.auth.Authenticate(r); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid or missing API key")
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxBodySize)
	defer func() { _ = body.Close() }()

	var req searchRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}

	opts := domain.SearchOptions{Limit: defaultSearchLimit}
	if req.Limit >= 1 {
		opts.Limit = req.Limit
	}
	if opts.Limit > s.maxResults {
		opts.Limit = s.maxResults
	}
	switch req.TimeRange {
	case "", "day", "week", "month", "year":
		opts.TimeRange = req.TimeRange
	default:
		writeError(w, http.StatusBadRequest, "time_range must be one of: day, week, month, year")
		return
	}
	opts.Language = req.Language

	start := time.Now()
	results, err := s.searcher.Search(r.Context(), req.Query, opts)
	if s.metrics != nil {
		s.metrics.ObserveSearch(s.searcher.Name(), observability.StatusOf(err), observability.Reason(err), time.Since(start))
	}
	if err != nil {
		s.logger.Warn("search failed", "query", req.Query, "reason", observability.Reason(err), "err", err)
		writeError(w, http.StatusBadGateway, "search upstream failed (see server logs for details)")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(results)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
