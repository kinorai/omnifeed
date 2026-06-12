// Package mcp implements a minimal Model Context Protocol server.
//
// The server speaks JSON-RPC 2.0 over two transports:
//
//   - stdio: one JSON message per line on stdin/stdout. Used by local MCP
//     clients that spawn the proxy as a subprocess.
//   - Streamable HTTP (MCP spec 2025-03-26): a single endpoint at /mcp that
//     accepts POST (one-shot JSON-RPC request/response) and GET (server-
//     initiated SSE event stream). This is the canonical HTTP transport for
//     remote MCP clients (Claude Code, OpenCode, Cursor remote, hosted MCP).
//
// For backwards compatibility with older clients that only speak the
// deprecated dual-endpoint SSE shape, the server also exposes /mcp/sse as
// a legacy alias — same handler as the GET path of /mcp. New clients
// should target /mcp; /mcp/sse is preserved for compat and may eventually
// be removed.
//
// The server is a pure transport: it owns JSON-RPC framing, auth, and SSE
// keepalive, and dispatches tools/list and tools/call against the Tool slice
// it was configured with. The tools themselves (fetch_url, web_search)
// live in the tools subpackage and are wired in by main.
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
	"time"

	"github.com/kinorai/omnifeed/internal/auth"
	"github.com/kinorai/omnifeed/internal/version"
)

// ProtocolVersion is the MCP version this server speaks.
const ProtocolVersion = "2024-11-05"

// Server is a JSON-RPC 2.0 MCP server.
type Server struct {
	tools  []Tool
	byName map[string]Tool
	auth   auth.Authenticator
	logger *slog.Logger
}

// Config configures the Server.
//
// Tools is the ordered list surfaced by tools/list and dispatched by
// tools/call. Authenticator gates the HTTP transport (POST /mcp and GET
// /mcp/sse). The stdio transport is unaffected because it runs as a local
// subprocess and inherits trust from its parent. If nil, auth.AlwaysAllow
// is used.
type Config struct {
	Tools         []Tool
	Authenticator auth.Authenticator
	Logger        *slog.Logger
}

// New constructs the server.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Authenticator == nil {
		cfg.Authenticator = auth.AlwaysAllow{}
	}
	byName := make(map[string]Tool, len(cfg.Tools))
	for _, t := range cfg.Tools {
		byName[t.Name] = t
	}
	return &Server{
		tools:  cfg.Tools,
		byName: byName,
		auth:   cfg.Authenticator,
		logger: cfg.Logger,
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
			"name":    "omnifeed",
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
	tools := make([]toolSchema, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, toolSchema{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
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

	tool, found := s.byName[p.Name]
	if !found {
		return errorResp(req.ID, codeInvalidParams, "unknown tool: "+p.Name)
	}

	res, err := tool.Handle(ctx, p.Arguments)
	if err != nil {
		var paramErr ParamError
		if errors.As(err, &paramErr) {
			return errorResp(req.ID, codeInvalidParams, paramErr.Error())
		}
		s.logger.Warn("mcp tool call failed", "tool", p.Name, "err", err)
		return errorResp(req.ID, codeInternalError, p.Name+" failed")
	}

	return ok(req.ID, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": res.Text},
		},
		"_meta": res.Meta,
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

// serveHTTPSSE keeps the connection open and emits a keepalive comment every
// 30s so intermediaries don't drop an otherwise-idle stream — Cloudflare cuts
// proxied/tunnel connections that send no bytes for ~100s. We don't currently
// push server-initiated notifications, but the endpoint is here so MCP clients
// that try to open it don't fail.
func (s *Server) serveHTTPSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, isFlusher := w.(http.Flusher)
	if !isFlusher {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// The HTTP server sets a finite WriteTimeout (see runServers) that would
	// otherwise tear this long-lived stream down mid-flight. Clear the write
	// deadline for this connection. Best-effort: a ResponseWriter that doesn't
	// support it (e.g. httptest) keeps its default deadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	ctx := r.Context()
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return // client or proxy went away
			}
			flusher.Flush()
		}
	}
}

func httpJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// Register attaches the MCP HTTP routes behind the configured authenticator.
//
//   - /mcp     — canonical Streamable HTTP endpoint (POST = JSON-RPC,
//     GET = SSE event stream). This is what new clients use.
//   - /mcp/sse — legacy alias kept for compatibility with older clients
//     that only speak the deprecated dual-endpoint SSE shape.
//     Same handler as GET /mcp.
//
// Both routes share the same bearer-token check.
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
