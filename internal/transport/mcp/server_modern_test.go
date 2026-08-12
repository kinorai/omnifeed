package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modernMeta is the request `_meta` a 2026-07-28 client sends on every call.
const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// postRec sends one JSON-RPC request to /mcp with the given headers and
// returns the raw recorder, so callers can assert on non-200 statuses.
func postRec(t *testing.T, srv *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// modernHeaders is the header set a conforming 2026-07-28 client sends for a
// tools/call. Tests mutate copies of it to build the failure cases.
func modernHeaders(method, name string) map[string]string {
	h := map[string]string{
		"MCP-Protocol-Version": "2026-07-28",
		"Mcp-Method":           method,
	}
	if name != "" {
		h["Mcp-Name"] = name
	}
	return h
}

func echoTool() Tool {
	return Tool{
		Name:        "fetch_url",
		InputSchema: map[string]any{"type": "object"},
		Meta:        map[string]any{"anthropic/maxResultSizeChars": 500000},
		Handle: func(context.Context, map[string]any) (ToolResult, error) {
			return ToolResult{Text: "hello", Meta: map[string]string{"final_url": "https://example.com"}}, nil
		},
	}
}

// server/discover must advertise every era we speak (modern first), the
// capabilities, and the instructions — it replaces initialize as the one
// channel that steers tool selection for modern clients. It must also answer
// without modern framing: the spec blesses it as the backward-compat probe.
func TestDiscover_AdvertisesVersionsAndInstructions(t *testing.T) {
	rec := postRec(t, New(Config{}), `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Result struct {
			ResultType        string   `json:"resultType"`
			SupportedVersions []string `json:"supportedVersions"`
			Instructions      string   `json:"instructions"`
			TTLMs             int      `json:"ttlMs"`
			CacheScope        string   `json:"cacheScope"`
			Meta              struct {
				ServerInfo struct {
					Name string `json:"name"`
				} `json:"io.modelcontextprotocol/serverInfo"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r := resp.Result
	if r.ResultType != "complete" {
		t.Errorf("resultType: got %q, want %q", r.ResultType, "complete")
	}
	if len(r.SupportedVersions) == 0 || r.SupportedVersions[0] != modernProtocolVersion {
		t.Errorf("supportedVersions: got %v, want %q first", r.SupportedVersions, modernProtocolVersion)
	}
	found := false
	for _, v := range r.SupportedVersions {
		if v == "2024-11-05" {
			found = true
		}
	}
	if !found {
		t.Errorf("supportedVersions must include the legacy eras, got %v", r.SupportedVersions)
	}
	if !strings.Contains(r.Instructions, "Reddit") {
		t.Errorf("instructions should mention Reddit, got %q", r.Instructions)
	}
	if r.TTLMs <= 0 || r.CacheScope != "public" {
		t.Errorf("cache hints: got ttlMs=%d cacheScope=%q", r.TTLMs, r.CacheScope)
	}
	if r.Meta.ServerInfo.Name != "omnifeed" {
		t.Errorf("_meta serverInfo name: got %q, want omnifeed", r.Meta.ServerInfo.Name)
	}
}

// A conforming modern tools/call must succeed and carry the 2026-07-28 result
// fields: resultType, serverInfo in _meta, and the tool's own meta preserved.
func TestModernToolsCall_HappyPath(t *testing.T) {
	srv := New(Config{Tools: []Tool{echoTool()}})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{},` + modernMeta + `}}`
	rec := postRec(t, srv, body, modernHeaders("tools/call", "fetch_url"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Result struct {
			ResultType string           `json:"resultType"`
			Content    []map[string]any `json:"content"`
			Meta       map[string]any   `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Result.ResultType != "complete" {
		t.Errorf("resultType: got %q, want complete", resp.Result.ResultType)
	}
	if len(resp.Result.Content) != 1 || resp.Result.Content[0]["text"] != "hello" {
		t.Errorf("content: got %v", resp.Result.Content)
	}
	if resp.Result.Meta["final_url"] != "https://example.com" {
		t.Errorf("tool meta must be preserved, got %v", resp.Result.Meta)
	}
	if _, hasInfo := resp.Result.Meta["io.modelcontextprotocol/serverInfo"]; !hasInfo {
		t.Errorf("_meta must carry serverInfo, got %v", resp.Result.Meta)
	}
}

// The Mcp-Name header may arrive Base64-sentinel-encoded; the server must
// decode it before comparing against the body.
func TestModernToolsCall_Base64SentinelName(t *testing.T) {
	srv := New(Config{Tools: []Tool{echoTool()}})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{},` + modernMeta + `}}`
	// "fetch_url" in the spec's =?base64?...?= sentinel format.
	rec := postRec(t, srv, body, modernHeaders("tools/call", "=?base64?ZmV0Y2hfdXJs?="))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// Legacy responses must keep their historical wire shape: no resultType, no
// cache hints, no serverInfo injected into _meta. Modern fields leaking into
// legacy responses would change bytes that existing clients already parse.
func TestLegacyResponses_UnchangedShape(t *testing.T) {
	srv := New(Config{Tools: []Tool{echoTool()}})

	for _, tc := range []struct{ name, body string }{
		{"tools/list", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{"tools/call", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := post(t, srv, tc.body)
			var resp struct {
				Result map[string]any `json:"result"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, field := range []string{"resultType", "ttlMs", "cacheScope"} {
				if _, exists := resp.Result[field]; exists {
					t.Errorf("legacy %s result must not carry %q, got %v", tc.name, field, resp.Result)
				}
			}
		})
	}
}

// Modern tools/list must carry the required cache hints (SEP-2549).
func TestModernToolsList_CarriesCacheHints(t *testing.T) {
	srv := New(Config{Tools: []Tool{echoTool()}})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + modernMeta + `}}`
	rec := postRec(t, srv, body, modernHeaders("tools/list", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Result struct {
			ResultType string `json:"resultType"`
			TTLMs      int    `json:"ttlMs"`
			CacheScope string `json:"cacheScope"`
			Tools      []any  `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Result.TTLMs <= 0 || resp.Result.CacheScope != "public" || resp.Result.ResultType != "complete" {
		t.Errorf("modern tools/list: got ttlMs=%d cacheScope=%q resultType=%q",
			resp.Result.TTLMs, resp.Result.CacheScope, resp.Result.ResultType)
	}
	if len(resp.Result.Tools) != 1 {
		t.Errorf("tools: got %d, want 1", len(resp.Result.Tools))
	}
}

// Modern request validation (SEP-2243/SEP-2575): version and routing headers
// must be present and must match the body, or the request is rejected with
// HTTP 400 and the spec's error code.
func TestModernValidation_RejectsBadRequests(t *testing.T) {
	toolsCall := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{},` + modernMeta + `}}`
	cases := []struct {
		name     string
		body     string
		headers  map[string]string
		wantCode int // JSON-RPC error code
	}{
		{
			name:     "missing protocol version header",
			body:     toolsCall,
			headers:  map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "fetch_url"},
			wantCode: codeHeaderMismatch,
		},
		{
			name: "header and _meta version mismatch",
			body: toolsCall,
			headers: map[string]string{
				"MCP-Protocol-Version": "2025-06-18", "Mcp-Method": "tools/call", "Mcp-Name": "fetch_url",
			},
			wantCode: codeHeaderMismatch,
		},
		{
			name:     "missing Mcp-Method header",
			body:     toolsCall,
			headers:  map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Name": "fetch_url"},
			wantCode: codeHeaderMismatch,
		},
		{
			name: "Mcp-Method does not match body",
			body: toolsCall,
			headers: map[string]string{
				"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/list", "Mcp-Name": "fetch_url",
			},
			wantCode: codeHeaderMismatch,
		},
		{
			name:     "missing Mcp-Name header on tools/call",
			body:     toolsCall,
			headers:  map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call"},
			wantCode: codeHeaderMismatch,
		},
		{
			name: "Mcp-Name does not match body",
			body: toolsCall,
			headers: map[string]string{
				"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "web_search",
			},
			wantCode: codeHeaderMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(Config{Tools: []Tool{echoTool()}})
			rec := postRec(t, srv, tc.body, tc.headers)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			var resp struct {
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error.Code != tc.wantCode {
				t.Errorf("error code: got %d, want %d", resp.Error.Code, tc.wantCode)
			}
		})
	}
}

// A protocol version we don't speak must be rejected with HTTP 400 and error
// -32022, with the supported list in the error data so the client can retry.
func TestModernValidation_UnsupportedVersion(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01"}}}`
	rec := postRec(t, New(Config{}), body, map[string]string{
		"MCP-Protocol-Version": "2099-01-01", "Mcp-Method": "tools/list",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Error struct {
			Code int `json:"code"`
			Data struct {
				Supported []string `json:"supported"`
				Requested string   `json:"requested"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != codeUnsupportedVersion {
		t.Errorf("error code: got %d, want %d", resp.Error.Code, codeUnsupportedVersion)
	}
	if resp.Error.Data.Requested != "2099-01-01" {
		t.Errorf("requested: got %q", resp.Error.Data.Requested)
	}
	if len(resp.Error.Data.Supported) == 0 || resp.Error.Data.Supported[0] != modernProtocolVersion {
		t.Errorf("supported: got %v, want %q first", resp.Error.Data.Supported, modernProtocolVersion)
	}
}

// The modern era removed initialize and ping. A modern-framed request for
// them must get HTTP 404 + -32601 — that status is how clients tell a modern
// server from a legacy endpoint during version fallback.
func TestModern_RemovedMethodsAre404(t *testing.T) {
	for _, method := range []string{"initialize", "ping"} {
		t.Run(method, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{` + modernMeta + `}}`
			rec := postRec(t, New(Config{}), body, modernHeaders(method, ""))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status: got %d, want 404 (body: %s)", rec.Code, rec.Body.String())
			}
			var resp struct {
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error.Code != codeMethodNotFound {
				t.Errorf("error code: got %d, want %d", resp.Error.Code, codeMethodNotFound)
			}
		})
	}
}

// Legacy clients keep the legacy contract: an unknown method stays HTTP 200
// with the error in the JSON-RPC body, exactly as before the dual-era split.
func TestLegacy_UnknownMethodStays200(t *testing.T) {
	rec := postRec(t, New(Config{}), `{"jsonrpc":"2.0","id":1,"method":"nope"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":-32601`) {
		t.Fatalf("body must carry -32601, got: %s", rec.Body.String())
	}
}

// stdio speaks both eras too: server/discover answers as a compat probe, a
// modern-framed request gets modern result fields, and an unsupported
// version is rejected with -32022 (there are no HTTP statuses on stdio).
func TestStdio_ModernEra(t *testing.T) {
	srv := New(Config{Tools: []Tool{echoTool()}})
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"server/discover"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + modernMeta + `}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01"}}}` + "\n")
	var out strings.Builder
	if err := srv.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("responses: got %d, want 3 (out: %s)", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"supportedVersions"`) {
		t.Errorf("discover response missing supportedVersions: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"resultType":"complete"`) {
		t.Errorf("modern tools/list missing resultType: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"code":-32022`) {
		t.Errorf("unsupported version must return -32022: %s", lines[2])
	}
}

// Every initialize-era header version must keep a request on the legacy
// path. This pins the era decision itself: if a version is ever dropped or
// mistyped in legacyVersions, deployed clients that send it (Claude Code,
// Cursor, Codex all send their negotiated version on each request) would
// silently land on the modern path and 400 on every call.
func TestLegacyHeaderVersions_StayLegacy(t *testing.T) {
	for _, v := range legacyVersions {
		t.Run(v, func(t *testing.T) {
			srv := New(Config{Tools: []Tool{echoTool()}})
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{}}}`
			rec := postRec(t, srv, body, map[string]string{"MCP-Protocol-Version": v})
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			var resp struct {
				Result map[string]any `json:"result"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if _, exists := resp.Result["resultType"]; exists {
				t.Errorf("legacy request got a modern-shaped result: %v", resp.Result)
			}
			if resp.Result["content"] == nil {
				t.Errorf("tool did not execute: %v", resp.Result)
			}
		})
	}
}

// A legacy version inside `_meta` must not flip the request modern: `_meta`
// is an open bag in every era, and rejecting a version we support — with an
// error listing that same version as supported — would tell the client to
// retry with what it just sent.
func TestLegacyMetaVersion_StaysLegacy(t *testing.T) {
	srv := New(Config{Tools: []Tool{echoTool()}})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-06-18"}}}`
	rec := postRec(t, srv, body, map[string]string{"MCP-Protocol-Version": "2025-06-18"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"resultType"`) {
		t.Errorf("legacy request got a modern-shaped result: %s", rec.Body.String())
	}
}

// Notifications (no id) cannot dodge the header/body agreement check by
// dropping the id: headers they DO send must still match the body, or the
// split-brain a routing gateway would suffer goes undetected. Only header
// *presence* is optional for notifications — the spec leaves their header
// requirements undefined, so a bare notification must still pass.
func TestModernNotification_Validation(t *testing.T) {
	notif := `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"fetch_url","arguments":{},` + modernMeta + `}}`

	t.Run("mismatched Mcp-Name is rejected", func(t *testing.T) {
		srv := New(Config{Tools: []Tool{echoTool()}})
		rec := postRec(t, srv, notif, modernHeaders("tools/call", "web_search"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("unsupported version is rejected", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01"}}}`
		rec := postRec(t, New(Config{}), body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("bare notification still passes", func(t *testing.T) {
		srv := New(Config{Tools: []Tool{echoTool()}})
		rec := postRec(t, srv, notif, nil)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status: got %d, want 202 (body: %s)", rec.Code, rec.Body.String())
		}
	})
}

// A modern request whose body lacks the required `_meta` protocol version
// must fail with a message naming the missing field — not a baffling
// comparison against an empty string.
func TestModernValidation_MissingMetaVersion(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	rec := postRec(t, New(Config{}), body, modernHeaders("tools/list", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), metaProtocolVersion) {
		t.Errorf("error must name the missing _meta field, got: %s", rec.Body.String())
	}
}
