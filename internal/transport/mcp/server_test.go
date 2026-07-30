package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
)

// post sends a single JSON-RPC request to /mcp and returns the recorded
// response body. Auth defaults to AlwaysAllow for a zero Config.
func post(t *testing.T, srv *Server, body string) []byte {
	t.Helper()
	mux := http.NewServeMux()
	srv.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// initialize must advertise the optional `instructions` field. Clients that
// defer-load per-tool descriptions (e.g. Claude Code) only see this string and
// the tool names up-front, so it's the only reliable channel for steering tool
// selection — losing it would silently regress that behavior.
func TestInitialize_AdvertisesInstructions(t *testing.T) {
	body := post(t, New(Config{}), `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)

	var resp struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}
	if resp.Result.Instructions == "" {
		t.Fatal("initialize result is missing the instructions field")
	}
	// The whole point of the field is to steer agents to omnifeed for Reddit.
	if !strings.Contains(resp.Result.Instructions, "Reddit") {
		t.Fatalf("instructions should mention Reddit, got: %q", resp.Result.Instructions)
	}
}

// initialize must negotiate the protocol version: echo the client's requested
// version when we support it, otherwise return our latest.
func TestInitialize_NegotiatesProtocolVersion(t *testing.T) {
	cases := []struct {
		name      string
		requested string // empty → omit params
		want      string
	}{
		{"supported version is echoed", "2024-11-05", "2024-11-05"},
		{"latest is echoed", ProtocolVersion, ProtocolVersion},
		{"unknown/newer falls back to latest", "2099-01-01", ProtocolVersion},
		{"absent falls back to latest", "", ProtocolVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
			if tc.requested != "" {
				body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + tc.requested + `"}}`
			}
			raw := post(t, New(Config{}), body)

			var resp struct {
				Result struct {
					ProtocolVersion string `json:"protocolVersion"`
				} `json:"result"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Result.ProtocolVersion != tc.want {
				t.Fatalf("protocolVersion: got %q, want %q", resp.Result.ProtocolVersion, tc.want)
			}
		})
	}
}

// tools/list must serialize a tool's annotations so clients receive the
// behavioral hints (e.g. readOnlyHint) and can skip approval friction.
func TestToolsList_SerializesAnnotations(t *testing.T) {
	srv := New(Config{Tools: []Tool{{
		Name:        "fetch_url",
		Description: "x",
		InputSchema: map[string]any{"type": "object"},
		Annotations: map[string]any{"readOnlyHint": true, "openWorldHint": true},
	}}})
	body := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	var resp struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Annotations map[string]bool `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Result.Tools) != 1 {
		t.Fatalf("tools: got %d, want 1", len(resp.Result.Tools))
	}
	if !resp.Result.Tools[0].Annotations["readOnlyHint"] {
		t.Fatalf("readOnlyHint: got %v, want true", resp.Result.Tools[0].Annotations)
	}
}

// A failed tool call must log the call arguments (e.g. the target url) so a
// failure is triageable from omnifeed's own logs without pivoting to the
// upstream's. The transport stays generic — it logs p.Arguments wholesale, not
// any tool-specific field.
func TestToolsCall_LogsArgsOnFailure(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	srv := New(Config{
		Logger: logger,
		Tools: []Tool{{
			Name:        "fetch_url",
			InputSchema: map[string]any{"type": "object"},
			Handle: func(context.Context, map[string]any) (ToolResult, error) {
				return ToolResult{}, errors.New("boom")
			},
		}},
	})
	post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{"url":"https://example.com/doc.pdf"}}}`)

	if !strings.Contains(buf.String(), "https://example.com/doc.pdf") {
		t.Fatalf("failure log missing the call args/url; got: %s", buf.String())
	}
}

// A tools/call failure must carry the classified reason (and the upstream status
// when the error has one) into the JSON-RPC error message: a bare
// "fetch_url failed" leaves the calling agent unable to tell a retryable
// upstream fault from a bot wall. The code stays -32603 (no protocol change).
func TestToolsCall_ErrorMessageCarriesReasonAndStatus(t *testing.T) {
	cases := []struct {
		name string
		tool string
		err  error
		want string
	}{
		{
			name: "fetch_error_with_kind_and_status",
			tool: "fetch_url",
			err: &domain.FetchError{
				Kind:       domain.KindUpstreamError,
				StatusCode: 500,
				Err:        errors.New("crawl4ai returned 500: Internal Server Error"),
			},
			want: "fetch_url failed: upstream_error (HTTP 500): crawl4ai returned 500: Internal Server Error",
		},
		{
			// Same error the searxng searcher returns for a degraded upstream. The
			// reason prefix is dropped because the cause text already says it.
			name: "degraded_search",
			tool: "web_search",
			err: &domain.FetchError{
				Kind: domain.KindDegraded,
				Err:  errors.New("search degraded: 2 engines unavailable (google: Suspended: too many requests, bing: timeout) — retry shortly"),
			},
			want: "web_search failed: search degraded: 2 engines unavailable (google: Suspended: too many requests, bing: timeout) — retry shortly",
		},
		{
			name: "captcha_without_wrapped_error",
			tool: "fetch_url",
			err:  &domain.FetchError{Kind: domain.KindCaptcha, StatusCode: 403, Marker: "cf-challenge"},
			want: `fetch_url failed: captcha (HTTP 403): matched block marker "cf-challenge"`,
		},
		{
			name: "plain_error",
			tool: "fetch_url",
			err:  errors.New("boom"),
			want: "fetch_url failed: boom",
		},
		{
			// The upstream endpoint is a configured internal address — it must not
			// reach the client.
			name: "upstream_url_redacted",
			tool: "fetch_url",
			err: &domain.FetchError{
				Kind: domain.KindUpstreamError,
				Err:  errors.New(`Post "http://crawl4ai:11235/crawl": dial tcp: connection refused`),
			},
			want: `fetch_url failed: upstream_error: Post "[upstream]": dial tcp: connection refused`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(Config{
				Tools: []Tool{{
					Name:        tc.tool,
					InputSchema: map[string]any{"type": "object"},
					Handle: func(context.Context, map[string]any) (ToolResult, error) {
						return ToolResult{}, tc.err
					},
				}},
			})
			body := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tc.tool+`","arguments":{}}}`)

			var resp struct {
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error.Code != codeInternalError {
				t.Fatalf("code: got %d, want %d", resp.Error.Code, codeInternalError)
			}
			if resp.Error.Message != tc.want {
				t.Fatalf("message:\n got %q\nwant %q", resp.Error.Message, tc.want)
			}
		})
	}
}

// tools/list must serialize Tool.Meta as `_meta` — that is how fetch_url
// declares anthropic/maxResultSizeChars — and must omit the field entirely for
// tools that set no Meta, rather than sending a null or empty object.
func TestToolsList_SerializesMeta(t *testing.T) {
	srv := New(Config{Tools: []Tool{
		{
			Name:        "fetch_url",
			InputSchema: map[string]any{"type": "object"},
			Meta:        map[string]any{"anthropic/maxResultSizeChars": 500000},
		},
		{Name: "web_search", InputSchema: map[string]any{"type": "object"}},
	}})
	body := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Result.Tools) != 2 {
		t.Fatalf("tools: got %d, want 2", len(resp.Result.Tools))
	}

	meta, isObject := resp.Result.Tools[0]["_meta"].(map[string]any)
	if !isObject {
		t.Fatalf("fetch_url _meta: got %v, want an object", resp.Result.Tools[0]["_meta"])
	}
	if meta["anthropic/maxResultSizeChars"] != float64(500000) {
		t.Errorf("anthropic/maxResultSizeChars: got %v, want 500000", meta["anthropic/maxResultSizeChars"])
	}
	if _, exists := resp.Result.Tools[1]["_meta"]; exists {
		t.Errorf("web_search must have no _meta key, got %v", resp.Result.Tools[1])
	}
}
