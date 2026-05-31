// Package mcp implements a minimal Model Context Protocol server.
//
// The server speaks JSON-RPC 2.0. It supports stdio transport (one JSON
// message per line on stdin/stdout) and an HTTP+SSE transport for remote
// clients (Cursor remote, hosted MCP).
//
// Exposed tools:
//
//	crawl(url string) → page_content + metadata (uses the engine registry)
//	reddit_get_post(url string) → TOON-encoded Reddit thread
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/kinorai/crawl4ai-reddit-proxy/internal/auth"
	"github.com/kinorai/crawl4ai-reddit-proxy/internal/domain"
	"github.com/kinorai/crawl4ai-reddit-proxy/internal/engine"
	"github.com/kinorai/crawl4ai-reddit-proxy/internal/engine/reddit"
	"github.com/kinorai/crawl4ai-reddit-proxy/internal/version"
)

// ProtocolVersion is the MCP version this server speaks.
const ProtocolVersion = "2024-11-05"

// Server is a JSON-RPC 2.0 MCP server.
type Server struct {
	registry       *engine.Registry
	auth           auth.Authenticator
	logger         *slog.Logger
	redditDefaults reddit.Options
}

// Config configures the Server.
//
// Authenticator gates the HTTP transport (POST /mcp and GET /mcp/sse). The
// stdio transport is unaffected because it runs as a local subprocess and
// inherits trust from its parent. If nil, auth.AlwaysAllow is used.
type Config struct {
	Registry       *engine.Registry
	Authenticator  auth.Authenticator
	Logger         *slog.Logger
	RedditDefaults reddit.Options
}

// New constructs the server.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Authenticator == nil {
		cfg.Authenticator = auth.AlwaysAllow{}
	}
	return &Server{
		registry:       cfg.Registry,
		auth:           cfg.Authenticator,
		logger:         cfg.Logger,
		redditDefaults: cfg.RedditDefaults,
	}
}

// --- JSON-RPC 2.0 wire types ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// ServeStdio reads JSON-RPC messages from in, dispatches them, and writes
// responses to out. Notifications (id absent) produce no response.
//
// Blocks until ctx is canceled or in returns EOF.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 16MB max line

	writeMu := sync.Mutex{}
	writeResp := func(resp rpcResponse) {
		writeMu.Lock()
		defer writeMu.Unlock()
		b, _ := json.Marshal(resp)
		_, _ = out.Write(b)
		_, _ = out.Write([]byte("\n"))
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			writeResp(errorResp(nil, codeParseError, err.Error()))
			continue
		}
		if req.JSONRPC != "2.0" {
			writeResp(errorResp(req.ID, codeInvalidRequest, "jsonrpc must be 2.0"))
			continue
		}

		// Notifications (no ID) get no response.
		isNotification := len(req.ID) == 0

		resp := s.dispatch(ctx, req)
		if !isNotification {
			writeResp(resp)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "initialized", "notifications/initialized":
		// Notification — no response needed, but we still return a valid struct.
		return rpcResponse{JSONRPC: "2.0", ID: req.ID}
	case "ping":
		return ok(req.ID, struct{}{})
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	default:
		return errorResp(req.ID, codeMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *Server) handleInitialize(req rpcRequest) rpcResponse {
	return ok(req.ID, map[string]any{
		"protocolVersion": ProtocolVersion,
		"serverInfo": map[string]string{
			"name":    "crawl4ai-reddit-proxy",
			"version": version.Version,
		},
		"capabilities": map[string]any{
			"tools": map[string]any{
				"listChanged": false,
			},
		},
	})
}

type toolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (s *Server) handleToolsList(req rpcRequest) rpcResponse {
	tools := []toolSchema{
		{
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
		},
		{
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
		},
	}
	return ok(req.ID, map[string]any{"tools": tools})
}

type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func (s *Server) handleToolsCall(ctx context.Context, req rpcRequest) rpcResponse {
	var p toolCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errorResp(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}

	url, _ := p.Arguments["url"].(string)
	if url == "" {
		return errorResp(req.ID, codeInvalidParams, "missing required argument: url")
	}

	opts := domain.EngineOptions{
		RedditFormat:      s.redditDefaults.Format,
		RedditKeepDepth:   s.redditDefaults.KeepDepth,
		RedditKeepCreated: s.redditDefaults.KeepCreated,
		RedditMaxRounds:   s.redditDefaults.MaxRounds,
	}
	if f, ok := p.Arguments["format"].(string); ok && (f == "toon" || f == "json") {
		opts.RedditFormat = f
	}
	if ex, ok := p.Arguments["expand"].(float64); ok && ex >= 0 {
		opts.RedditMaxRounds = int(ex)
	}

	doc, err := s.registry.Crawl(ctx, url, opts)
	if err != nil {
		return errorResp(req.ID, codeInternalError, err.Error())
	}

	return ok(req.ID, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": doc.PageContent},
		},
		"_meta": doc.Metadata,
	})
}

func ok(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResp(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// ServeHTTP handles MCP-over-HTTP/SSE per the Streamable HTTP transport spec.
// Each POST to the path is a single JSON-RPC request; the response is
// returned in the body. Optionally, GET on the same path opens an SSE stream
// for server-initiated messages (none today — kept open for future server
// notifications).
//
// This is the minimum-viable implementation: synchronous request/response.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.serveHTTPPost(w, r)
	case http.MethodGet:
		s.serveHTTPSSE(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) serveHTTPPost(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	defer func() { _ = body.Close() }()

	var req rpcRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		httpJSON(w, http.StatusBadRequest, errorResp(nil, codeParseError, err.Error()))
		return
	}
	if req.JSONRPC != "2.0" {
		httpJSON(w, http.StatusBadRequest, errorResp(req.ID, codeInvalidRequest, "jsonrpc must be 2.0"))
		return
	}

	resp := s.dispatch(r.Context(), req)
	if len(req.ID) == 0 {
		// Notification — return 202 Accepted with no body.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	httpJSON(w, http.StatusOK, resp)
}

// serveHTTPSSE keeps the connection open and emits a comment every 30s so
// proxies don't close the stream. We don't currently push server-initiated
// notifications, but the endpoint is here so MCP clients that try to open it
// don't fail.
func (s *Server) serveHTTPSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	<-ctx.Done()
}

func httpJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// Register attaches /mcp (POST + GET for SSE) to mux behind the configured
// authenticator. Both POST and GET routes share the same bearer-token check.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("/mcp", s.requireAuth(s.ServeHTTP))
	mux.Handle("/mcp/sse", s.requireAuth(s.serveHTTPSSE))
}

// requireAuth wraps a handler with the configured authenticator. On failure
// it responds with 401 and the RFC 6750 WWW-Authenticate challenge so clients
// can surface a clear error.
func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.auth.Authenticate(r); err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			http.Error(w, "invalid or missing API key", http.StatusUnauthorized)
			return
		}
		next(w, r)
	})
}
