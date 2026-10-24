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
// The server is dual-era. MCP 2026-07-28 removed the initialize handshake:
// every request carries its protocol version itself (the MCP-Protocol-Version
// header plus `_meta`), capabilities are fetched via server/discover, and the
// required Mcp-Method/Mcp-Name routing headers must match the body. Requests
// in that shape get the modern treatment (resultType, cache hints, serverInfo
// in `_meta`, HTTP 404 for unknown methods). Everything else — including all
// initialize-era clients back to 2024-11-05 — gets the exact legacy behavior,
// byte-compatible with what this server always returned. The era is decided
// per request, so old and new clients coexist on the same endpoint.
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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/auth"
	"github.com/kinorai/omnifeed/internal/observability"
	"github.com/kinorai/omnifeed/internal/version"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// modernProtocolVersion is the newest MCP revision this server speaks: the
// stateless, handshake-free era. Requests claim it per call (header + _meta)
// instead of negotiating it via initialize.
const modernProtocolVersion = "2026-07-28"

// latestLegacyProtocolVersion is the newest initialize-era revision. It is
// what initialize falls back to when the client requests a version we don't
// recognize — never modernProtocolVersion, because a server must not steer an
// initialize-era client onto the handshake-free protocol it isn't speaking.
const latestLegacyProtocolVersion = "2025-11-25"

// legacyVersions lists the initialize-era MCP revisions this server speaks,
// newest first. On initialize we echo the client's requested version when
// it's one of these (the MCP lifecycle negotiation rule) and otherwise fall
// back to latestLegacyProtocolVersion.
var legacyVersions = []string{latestLegacyProtocolVersion, "2025-06-18", "2025-03-26", "2024-11-05"}

// supportedVersions is every revision this server speaks, newest first —
// advertised by server/discover and by UnsupportedProtocolVersion errors so
// callers know what to retry with.
var supportedVersions = append([]string{modernProtocolVersion}, legacyVersions...)

// negotiateProtocolVersion implements the initialize version handshake: return
// the client's requested version if we support it, otherwise our latest
// initialize-era version.
func negotiateProtocolVersion(requested string) string {
	if slices.Contains(legacyVersions, requested) {
		return requested
	}
	return latestLegacyProtocolVersion
}

// Reserved `_meta` keys defined by MCP 2026-07-28.
const (
	metaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	metaServerInfo      = "io.modelcontextprotocol/serverInfo"
)

// metaRequestID is the `_meta` key tools/call results carry the request id
// under (the X-Request-Id of the HTTP request, or one generated per stdio
// message), so a caller can quote it when reporting a problem.
const metaRequestID = "request_id"

// listTTLMs and listCacheScope are the required cache hints on modern
// tools/list (and server/discover) results. The tool catalog is wired once in
// main and identical for every caller, so it is public and safe to cache for
// an hour — a restart that changes it also drops the connection.
const (
	listTTLMs      = 3600000
	listCacheScope = "public"
)

// serverInstructions is returned in the initialize response's optional
// `instructions` field (and the server/discover result in the modern era).
// It matters because some clients — notably Claude Code — defer-load per-tool
// descriptions and only surface tool *names* up-front, so a description never
// reaches the model until it explicitly searches for the tool. This string,
// by contrast, is loaded into the model's context as soon as the server
// connects, making it the one reliable place to steer tool selection. Keep it
// short and behavioral; avoid implementation details that can drift.
const serverInstructions = "omnifeed is a global web search and fetch gateway: " +
	"`web_search` searches the whole web and returns result URLs; " +
	"`fetch_url` fetches any URL as LLM-friendly content. " +
	"You MUST use them for Reddit and Hacker News."

// Server is a JSON-RPC 2.0 MCP server.
type Server struct {
	tools   []Tool
	byName  map[string]Tool
	auth    auth.Authenticator
	logger  *slog.Logger
	metrics *observability.Metrics
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
	// Metrics records omnifeed_mcp_tool_calls_total / omnifeed_mcp_tool_seconds.
	// Nil disables them.
	Metrics *observability.Metrics
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
		tools:   cfg.Tools,
		byName:  byName,
		auth:    cfg.Authenticator,
		logger:  cfg.Logger,
		metrics: cfg.Metrics,
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
	// MCP 2026-07-28 protocol errors (spec-reserved -32020..-32099 range).
	codeHeaderMismatch     = -32020
	codeUnsupportedVersion = -32022
)

// paramsProbe is the transport's single parse of the request fields it must
// see itself, for every method: Name feeds both Mcp-Name header validation
// and tools/call dispatch (one parse, so the value checked is the value
// executed), and the `_meta` protocol version feeds era detection. The tag
// duplicates metaProtocolVersion because struct tags must be literals.
type paramsProbe struct {
	Name string `json:"name"`
	Meta struct {
		ProtocolVersion string `json:"io.modelcontextprotocol/protocolVersion"`
		// ClientInfo is the per-request client description of the
		// handshake-free era; it only labels metrics.
		ClientInfo clientInfo `json:"io.modelcontextprotocol/clientInfo"`
	} `json:"_meta"`
}

func probeParams(req rpcRequest) paramsProbe {
	var p paramsProbe
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p)
	}
	return p
}

// eraOf decides which protocol era a message belongs to. A message is modern
// when it claims a non-initialize-era version — in `_meta` (any transport) or
// in the MCP-Protocol-Version header (HTTP; empty on stdio). A legacy version
// in either spot keeps the message legacy: `_meta` has been an open bag since
// 2024-11-05, and rejecting a version we do support — just because it arrived
// in modern framing — would tell the client to retry with the version it
// already used.
func eraOf(headerVersion, metaVersion string) bool {
	if metaVersion != "" {
		return !slices.Contains(legacyVersions, metaVersion)
	}
	return headerVersion != "" && !slices.Contains(legacyVersions, headerVersion)
}

// checkModernVersion validates the effective version a modern-era message
// requested (the `_meta` value, or the header when `_meta` is absent). Both
// transports share it so they can never disagree on which versions exist;
// ok=false means resp carries the -32022 rejection with the supported list.
func checkModernVersion(id json.RawMessage, headerVersion, metaVersion string) (resp rpcResponse, ok bool) {
	requested := metaVersion
	if requested == "" {
		requested = headerVersion
	}
	if requested != modernProtocolVersion {
		return unsupportedVersionResp(id, requested), false
	}
	return rpcResponse{}, true
}

// unsupportedVersionResp is the modern-era rejection for a protocol version
// we don't speak: error data carries our supported list so the caller can
// pick one and retry (or fall back to initialize).
func unsupportedVersionResp(id json.RawMessage, requested string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{
		Code:    codeUnsupportedVersion,
		Message: fmt.Sprintf("unsupported protocol version: %q", requested),
		Data:    map[string]any{"supported": supportedVersions, "requested": requested},
	}}
}

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

	// One subprocess is one client: its initialize names every later call.
	ctx = withSession(ctx, &session{})
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

		// Era detection on stdio: no HTTP headers exist, so the `_meta`
		// protocol version alone decides. An unsupported modern version is
		// rejected here, mirroring the HTTP transport's 400 path — except
		// for notifications, which have no response channel on stdio.
		probe := probeParams(req)
		modern := eraOf("", probe.Meta.ProtocolVersion)
		var resp rpcResponse
		verOK := true
		if modern {
			resp, verOK = checkModernVersion(req.ID, "", probe.Meta.ProtocolVersion)
		}
		if verOK {
			// No headers on stdio: every message gets a generated request id,
			// so its log lines and its result's _meta.request_id join.
			resp = s.dispatch(observability.WithRequestID(ctx, observability.NewRequestID()), req, modern, probe)
		}
		if !isNotification {
			writeResp(resp)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// dispatch routes one request. The modern era exposes exactly the 2026-07-28
// method set — initialize and ping were removed from the spec, so a modern
// request for them is method-not-found. The legacy era keeps the historical
// behavior untouched, plus server/discover: the spec blesses it as the
// backward-compatibility probe, so it must answer even without modern framing.
func (s *Server) dispatch(ctx context.Context, req rpcRequest, modern bool, probe paramsProbe) rpcResponse {
	if modern {
		switch req.Method {
		case "server/discover":
			return s.handleDiscover(req)
		case "tools/list":
			return s.handleToolsList(req, true)
		case "tools/call":
			return s.handleToolsCall(ctx, req, probe, true)
		default:
			return errorResp(req.ID, codeMethodNotFound, "method not found: "+req.Method)
		}
	}
	switch req.Method {
	case "initialize":
		return s.handleInitialize(ctx, req)
	case "initialized", "notifications/initialized":
		// Notification — no response needed, but we still return a valid struct.
		return rpcResponse{JSONRPC: "2.0", ID: req.ID}
	case "ping":
		return ok(req.ID, struct{}{})
	case "server/discover":
		return s.handleDiscover(req)
	case "tools/list":
		return s.handleToolsList(req, false)
	case "tools/call":
		return s.handleToolsCall(ctx, req, probe, false)
	default:
		return errorResp(req.ID, codeMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *Server) handleInitialize(ctx context.Context, req rpcRequest) rpcResponse {
	// Negotiate the protocol version against what the client asked for. Parsing
	// is best-effort: absent/malformed params fall through to our latest
	// version. We never reject initialize over this — the client drives
	// compatibility and disconnects if it can't speak what we return.
	var params struct {
		ProtocolVersion string     `json:"protocolVersion"`
		ClientInfo      clientInfo `json:"clientInfo"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	if sess := sessionFrom(ctx); sess != nil {
		ci := params.ClientInfo
		sess.client.Store(&ci)
	}
	// Who connects, and with what: the raw name/version (bounded by the
	// client, logged once per connection) next to the label metrics use.
	s.logger.InfoContext(ctx, "mcp client initialized",
		"client_name", truncate(params.ClientInfo.Name, 100),
		"client_version", truncate(params.ClientInfo.Version, 100),
		"client", initLabel(ctx, params.ClientInfo),
		"protocol_version", truncate(params.ProtocolVersion, 32))

	return ok(req.ID, map[string]any{
		"protocolVersion": negotiateProtocolVersion(params.ProtocolVersion),
		"serverInfo":      serverInfo(),
		"capabilities":    serverCapabilities(),
		"instructions":    serverInstructions,
	})
}

// handleDiscover implements server/discover (MCP 2026-07-28): the stateless
// replacement for initialize's capability advertisement. Always answered in
// the modern shape — the method itself is modern regardless of framing.
func (s *Server) handleDiscover(req rpcRequest) rpcResponse {
	return ok(req.ID, modernize(map[string]any{
		"supportedVersions": supportedVersions,
		"capabilities":      serverCapabilities(),
		"instructions":      serverInstructions,
		"ttlMs":             listTTLMs,
		"cacheScope":        listCacheScope,
	}))
}

func serverInfo() map[string]string {
	return map[string]string{
		"name":    "omnifeed",
		"version": version.Version,
	}
}

func serverCapabilities() map[string]any {
	return map[string]any{
		"tools": map[string]any{
			"listChanged": false,
		},
	}
}

// modernize decorates a result with the fields MCP 2026-07-28 requires on
// every response: resultType (always "complete" — this server never needs
// multi round-trip input) and serverInfo in `_meta`. Legacy responses never
// pass through here, so their wire shape stays byte-identical.
func modernize(result map[string]any) map[string]any {
	result["resultType"] = "complete"
	meta, _ := result["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[metaServerInfo] = serverInfo()
	result["_meta"] = meta
	return result
}

type toolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Meta        map[string]any `json:"_meta,omitempty"`
}

func (s *Server) handleToolsList(req rpcRequest, modern bool) rpcResponse {
	tools := make([]toolSchema, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, toolSchema{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Annotations: t.Annotations,
			Meta:        t.Meta,
		})
	}
	result := map[string]any{"tools": tools}
	if modern {
		// Required cache hints (SEP-2549). The slice order is fixed at wiring
		// time, which also satisfies the deterministic-order requirement that
		// keeps client prompt caches stable.
		result["ttlMs"] = listTTLMs
		result["cacheScope"] = listCacheScope
		result = modernize(result)
	}
	return ok(req.ID, result)
}

// handleToolsCall dispatches on the name from the transport's paramsProbe —
// the same parse the Mcp-Name header was validated against. A second parse
// of `name` here could diverge from the validated one and let the header
// check authorize one tool while another runs.
func (s *Server) handleToolsCall(ctx context.Context, req rpcRequest, probe paramsProbe, modern bool) rpcResponse {
	name := probe.Name
	var p struct {
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errorResp(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}

	tool, found := s.byName[name]
	if !found {
		// Not counted: the tool label is the configured tool set, never a
		// caller-chosen string.
		return errorResp(req.ID, codeInvalidParams, "unknown tool: "+name)
	}

	client := callerLabel(ctx, probe)
	requestID := observability.RequestIDFrom(ctx)
	start := time.Now()
	ctx, span := observability.Tracer().Start(ctx, "mcp.tool", trace.WithAttributes(
		attribute.String("mcp.tool", name), attribute.String("mcp.client", client)))
	res, err := s.callTool(ctx, tool, p.Arguments)
	observability.SpanError(span, err)
	span.End()
	if err != nil {
		if paramErr, ok := errors.AsType[ParamError](err); ok {
			s.observeTool(name, "invalid_params", client, start)
			return errorResp(req.ID, codeInvalidParams, paramErr.Error())
		}
		s.observeTool(name, "error", client, start)
		s.logger.WarnContext(ctx, "mcp tool call failed", "tool", name, "args", p.Arguments,
			"client", client, "code", string(observability.Classify(err).Code),
			"duration_ms", time.Since(start).Milliseconds(), "err", err)
		// A tool that ran and failed is a tool execution error, not a protocol
		// error: the spec puts it in the result with isError so the model sees
		// it and can react (retry, try another URL, give up). JSON-RPC errors
		// stay reserved for bad params, unknown tools, and the protocol itself.
		result := toolErrorResult(name, p.Arguments, err)
		if requestID != "" {
			result["_meta"] = map[string]any{metaRequestID: requestID}
		}
		if modern {
			return ok(req.ID, modernize(result))
		}
		return ok(req.ID, result)
	}
	s.observeTool(name, "ok", client, start)
	// Success exemplar for latency triage: metrics carry the duration
	// distributions but can never carry the URL/query (label cardinality) —
	// this line is how a slow histogram bucket is traced back to the exact
	// call in VictoriaLogs. The result's own _meta says how it was served.
	attrs := []any{
		"tool", name, "args", p.Arguments, "client", client,
		"duration_ms", time.Since(start).Milliseconds(),
		"chars", utf8.RuneCountInString(res.Text),
	}
	for k, v := range res.Log {
		attrs = append(attrs, k, v)
	}
	for _, k := range []string{"cache", "fallback_from", "partial", "partial_reason", "reddit_method", "truncated", "query_id"} {
		if v := res.Meta[k]; v != "" {
			attrs = append(attrs, k, v)
		}
	}
	s.logger.InfoContext(ctx, "mcp tool call completed", attrs...)

	content := []map[string]any{
		{"type": "text", "text": res.Text},
	}
	meta := make(map[string]any, len(res.Meta)+1)
	for k, v := range res.Meta {
		meta[k] = v
	}
	if requestID != "" {
		meta[metaRequestID] = requestID
	}
	if modern {
		return ok(req.ID, modernize(map[string]any{
			"content": content,
			"_meta":   meta,
		}))
	}
	return ok(req.ID, map[string]any{
		"content": content,
		"_meta":   meta,
	})
}

// callTool runs a tool handler, turning a panic into an error result: on
// stdio a panic would kill the subprocess, and on HTTP it would drop the
// connection with no JSON-RPC answer at all.
func (s *Server) callTool(ctx context.Context, tool Tool, args map[string]any) (res ToolResult, err error) {
	defer observability.Recover(observability.PanicMCPTool, &err)
	return tool.Handle(ctx, args)
}

func (s *Server) observeTool(tool, outcome, client string, start time.Time) {
	if s.metrics != nil {
		s.metrics.ObserveMCPToolCall(tool, outcome, client, time.Since(start))
	}
}

// initLabel is the client label for an initialize: its own clientInfo first,
// then the User-Agent.
func initLabel(ctx context.Context, ci clientInfo) string {
	var p paramsProbe
	p.Meta.ClientInfo = ci
	return callerLabel(ctx, p)
}

// truncate bounds a client-supplied string before it is logged.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
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

	// Era detection: a request is modern when it carries a non-legacy
	// per-request version in `_meta` or in the MCP-Protocol-Version header
	// (legacy clients since 2025-06-18 send their negotiated version there;
	// older clients send nothing). Anything legacy takes the old path
	// exactly as before.
	probe := probeParams(req)
	headerVersion := r.Header.Get("MCP-Protocol-Version")
	modern := eraOf(headerVersion, probe.Meta.ProtocolVersion)

	// Modern messages must pass validation (SEP-2243): version and routing
	// headers must match the body, or the message is rejected with 400. The
	// spec leaves notification header rules undefined, so notifications
	// (ID absent) are held only to consistency of the headers they DO send —
	// required-presence applies to requests alone. Never lax on mismatches:
	// executing the body's tool while a gateway routed on a different
	// Mcp-Name is the split-brain this check exists to prevent.
	if modern {
		if resp, bad := validateModernRequest(r, req, probe, headerVersion, len(req.ID) > 0); bad {
			httpJSON(w, http.StatusBadRequest, resp)
			return
		}
	}

	resp := s.dispatch(withUserAgent(r.Context(), r.UserAgent()), req, modern, probe)
	if len(req.ID) == 0 {
		// Notification — return 202 Accepted with no body.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	httpJSON(w, httpStatusFor(modern, resp), resp)
}

// httpStatusFor maps a dispatched response to its HTTP status. The modern era
// requires 404 for unknown methods — that status is how clients tell a modern
// server from a legacy endpoint. Everything else — including every legacy
// response — stays 200 with any error in the JSON-RPC body. The 400-class
// protocol errors (-32020/-32022) never reach here: validateModernRequest
// writes them with 400 at the call site.
func httpStatusFor(modern bool, resp rpcResponse) int {
	if modern && resp.Error != nil && resp.Error.Code == codeMethodNotFound {
		return http.StatusNotFound
	}
	return http.StatusOK
}

// validateModernRequest enforces the 2026-07-28 Streamable HTTP rules: the
// protocol version must be one we speak, the MCP-Protocol-Version header must
// match `_meta`, and the Mcp-Method/Mcp-Name routing headers must match the
// body (they exist so gateways can route on headers — a mismatch means two
// components would disagree about what is being called, which the spec treats
// as a security problem, not a nit). strict is true for requests, where the
// headers and the `_meta` version are also required to be present.
func validateModernRequest(r *http.Request, req rpcRequest, probe paramsProbe, headerVersion string, strict bool) (rpcResponse, bool) {
	if resp, ok := checkModernVersion(req.ID, headerVersion, probe.Meta.ProtocolVersion); !ok {
		return resp, true
	}
	if strict && probe.Meta.ProtocolVersion == "" {
		return errorResp(req.ID, codeHeaderMismatch,
			"body _meta is missing the required "+metaProtocolVersion+" field"), true
	}
	if strict && headerVersion == "" {
		return errorResp(req.ID, codeHeaderMismatch, "missing required MCP-Protocol-Version header"), true
	}
	if headerVersion != "" && probe.Meta.ProtocolVersion != "" && headerVersion != probe.Meta.ProtocolVersion {
		return errorResp(req.ID, codeHeaderMismatch, fmt.Sprintf(
			"MCP-Protocol-Version header %q does not match body _meta value %q",
			headerVersion, probe.Meta.ProtocolVersion)), true
	}
	mcpMethod := r.Header.Get("Mcp-Method")
	if strict && mcpMethod == "" {
		return errorResp(req.ID, codeHeaderMismatch, "missing required Mcp-Method header"), true
	}
	if mcpMethod != "" && mcpMethod != req.Method {
		return errorResp(req.ID, codeHeaderMismatch, fmt.Sprintf(
			"Mcp-Method header %q does not match body method %q", mcpMethod, req.Method)), true
	}
	if req.Method == "tools/call" {
		raw := r.Header.Get("Mcp-Name")
		if strict && raw == "" {
			return errorResp(req.ID, codeHeaderMismatch, "missing required Mcp-Name header"), true
		}
		if raw != "" {
			name, err := decodeHeaderValue(raw)
			if err != nil {
				return errorResp(req.ID, codeHeaderMismatch, "Mcp-Name header has invalid base64 encoding"), true
			}
			if name != probe.Name {
				return errorResp(req.ID, codeHeaderMismatch, fmt.Sprintf(
					"Mcp-Name header %q does not match body value %q", name, probe.Name)), true
			}
		}
	}
	return rpcResponse{}, false
}

// decodeHeaderValue undoes the spec's Base64 sentinel encoding
// (`=?base64?...?=`) used when a header value isn't plain ASCII. Values
// without the sentinel pass through unchanged; the sentinel markers are
// case-sensitive by spec.
func decodeHeaderValue(v string) (string, error) {
	const pre, suf = "=?base64?", "?="
	if len(v) >= len(pre)+len(suf) && v[:len(pre)] == pre && v[len(v)-len(suf):] == suf {
		b, err := base64.StdEncoding.DecodeString(v[len(pre) : len(v)-len(suf)])
		return string(b), err
	}
	return v, nil
}

// serveHTTPSSE keeps the connection open and emits a keepalive comment every
// 30s so intermediaries don't drop an otherwise-idle stream — Cloudflare cuts
// proxied/tunnel connections that send no bytes for ~100s. We don't currently
// push server-initiated notifications, but the endpoint is here so MCP clients
// that try to open it don't fail. It belongs to the legacy era — 2026-07-28
// replaced the GET stream with subscriptions/listen — and is kept for those
// older clients.
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
// Both routes share the same bearer-token check. Origin validation (the
// DNS-rebinding guard the transport spec requires) is applied one level up:
// main wraps every HTTP mux in auth.OriginGuard, so the loader and search
// transports in the same binary are covered by the same guard.
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
