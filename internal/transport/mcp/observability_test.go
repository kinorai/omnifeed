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
	"github.com/kinorai/omnifeed/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestClientLabel(t *testing.T) {
	cases := map[string]string{
		"claude-code":                       "claude-code",
		"claude-code/2.1.3 (external, cli)": "claude-code",
		"Claude Code":                       "claude-code",
		"opencode":                          "opencode",
		"opencode/0.9.1":                    "opencode",
		"open-webui":                        "openwebui",
		"OpenWebUI/0.6":                     "openwebui",
		"thedigestif/1.4.0":                 "thedigestif",
		"cursor-vscode":                     "cursor",
		"Cursor/1.2":                        "cursor",
		"python-httpx/0.27.0":               "other",
		"":                                  "other",
		"Mozilla/5.0 (X11; Linux x86_64)":   "other",
	}
	for in, want := range cases {
		if got := ClientLabel(in); got != want {
			t.Errorf("ClientLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func counter(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// postAs sends one JSON-RPC request through the full HTTP stack the binary
// mounts (request-id middleware included) with the given User-Agent.
func postAs(t *testing.T, srv *Server, ua, reqID, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("User-Agent", ua)
	if reqID != "" {
		req.Header.Set(observability.RequestIDHeader, reqID)
	}
	rec := httptest.NewRecorder()
	observability.RequestID(mux).ServeHTTP(rec, req)
	return rec
}

func observedServer(buf *bytes.Buffer, tools ...Tool) (*Server, *observability.Metrics) {
	m := observability.NewMetrics()
	logger := slog.New(observability.NewContextHandler(slog.NewJSONHandler(buf, nil)))
	return New(Config{Tools: tools, Logger: logger, Metrics: m}), m
}

func logLine(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for l := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(l), &line) == nil && line["msg"] == msg {
			return line
		}
	}
	t.Fatalf("no %q line in:\n%s", msg, buf.String())
	return nil
}

// A tool call is counted by tool/outcome/client, its result carries the
// request id in _meta, and the completion line carries the request id, the
// client and the served-how fields from _meta.
func TestToolCallMetricsLogsAndRequestID(t *testing.T) {
	var buf bytes.Buffer
	srv, m := observedServer(&buf, Tool{
		Name: "fetch_url",
		Handle: func(context.Context, map[string]any) (ToolResult, error) {
			return ToolResult{
				Text: "hello",
				Meta: map[string]string{"cache": "hit", "fallback_from": "reddit", "truncated": "true", "source": "x"},
				Log:  map[string]string{"engine": "reddit"},
			}, nil
		},
	})
	rec := postAs(t, srv, "thedigestif/1.4.0", "trace-me-1",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{"url":"https://example.com"}}}`)
	if rec.Header().Get(observability.RequestIDHeader) != "trace-me-1" {
		t.Fatalf("X-Request-Id = %q", rec.Header().Get(observability.RequestIDHeader))
	}
	var resp struct {
		Result struct {
			Meta map[string]any `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result.Meta["request_id"] != "trace-me-1" || resp.Result.Meta["cache"] != "hit" {
		t.Fatalf("_meta = %v, want request_id and the tool's own keys", resp.Result.Meta)
	}
	if got := counter(t, m.MCPToolCalls.WithLabelValues("fetch_url", "ok", "thedigestif")); got != 1 {
		t.Fatalf(`mcp_tool_calls_total{fetch_url,ok,thedigestif} = %v, want 1`, got)
	}
	line := logLine(t, &buf, "mcp tool call completed")
	for k, want := range map[string]any{
		"request_id": "trace-me-1", "client": "thedigestif", "engine": "reddit",
		"cache": "hit", "fallback_from": "reddit", "truncated": "true",
	} {
		if line[k] != want {
			t.Errorf("completion line %s = %v, want %v", k, line[k], want)
		}
	}
	if _, leaked := line["source"]; leaked {
		t.Error("completion line must log only the allowlisted _meta keys")
	}
}

// A failure is counted as outcome="error" and logged with its classified
// code; a panicking tool becomes an isError result, not a dead process.
func TestToolCallFailureAndPanic(t *testing.T) {
	var buf bytes.Buffer
	srv, m := observedServer(&buf,
		Tool{Name: "fetch_url", Handle: func(context.Context, map[string]any) (ToolResult, error) {
			return ToolResult{}, &domain.FetchError{Kind: domain.KindHTTP429, StatusCode: 429, Err: errors.New("slow down")}
		}},
		Tool{Name: "web_search", Handle: func(context.Context, map[string]any) (ToolResult, error) {
			panic("bug")
		}},
		Tool{Name: "bad", Handle: func(context.Context, map[string]any) (ToolResult, error) {
			return ToolResult{}, InvalidParams("nope")
		}},
	)
	postAs(t, srv, "claude-code/2.1.0", "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{}}}`)
	if got := counter(t, m.MCPToolCalls.WithLabelValues("fetch_url", "error", "claude-code")); got != 1 {
		t.Fatalf("error outcome = %v, want 1", got)
	}
	line := logLine(t, &buf, "mcp tool call failed")
	if line["code"] != string(observability.Classify(&domain.FetchError{Kind: domain.KindHTTP429, StatusCode: 429}).Code) {
		t.Fatalf("failure line code = %v", line["code"])
	}
	if id, _ := line["request_id"].(string); len(id) != 32 {
		t.Fatalf("failure line request_id = %v, want a generated id", line["request_id"])
	}

	rec := postAs(t, srv, "", "", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"web_search","arguments":{}}}`)
	if !strings.Contains(rec.Body.String(), `"isError":true`) {
		t.Fatalf("panicking tool: body = %s, want an isError result", rec.Body.String())
	}
	if got := counter(t, m.MCPToolCalls.WithLabelValues("web_search", "error", "other")); got != 1 {
		t.Fatalf("panic outcome = %v, want 1 error", got)
	}

	postAs(t, srv, "", "", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"bad","arguments":{}}}`)
	if got := counter(t, m.MCPToolCalls.WithLabelValues("bad", "invalid_params", "other")); got != 1 {
		t.Fatalf("invalid_params outcome = %v, want 1", got)
	}
}

// initialize logs who connected; on stdio that clientInfo labels every later
// call of the session.
func TestInitializeLogsClientAndLabelsStdioSession(t *testing.T) {
	var buf bytes.Buffer
	srv, m := observedServer(&buf, Tool{Name: "fetch_url", Handle: func(context.Context, map[string]any) (ToolResult, error) {
		return ToolResult{Text: "ok"}, nil
	}})
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"opencode","version":"0.9.1"}}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fetch_url","arguments":{}}}` + "\n")
	var out bytes.Buffer
	if err := srv.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	line := logLine(t, &buf, "mcp client initialized")
	if line["client_name"] != "opencode" || line["client_version"] != "0.9.1" || line["client"] != "opencode" {
		t.Fatalf("initialize line = %v", line)
	}
	if got := counter(t, m.MCPToolCalls.WithLabelValues("fetch_url", "ok", "opencode")); got != 1 {
		t.Fatalf("stdio call labelled from clientInfo = %v, want 1", got)
	}
	if !strings.Contains(out.String(), `"request_id"`) {
		t.Fatalf("stdio result lacks _meta.request_id: %s", out.String())
	}
}
