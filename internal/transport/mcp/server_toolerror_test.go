package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
)

func failingTool(err error) Tool {
	return Tool{
		Name:        "fetch_url",
		InputSchema: map[string]any{"type": "object"},
		Handle: func(context.Context, map[string]any) (ToolResult, error) {
			return ToolResult{}, err
		},
	}
}

type toolErrorResp struct {
	Error  *rpcError `json:"error"`
	Result struct {
		ResultType        string `json:"resultType"`
		IsError           bool   `json:"isError"`
		Content           []struct{ Type, Text string }
		StructuredContent map[string]any `json:"structuredContent"`
	} `json:"result"`
}

func decodeToolError(t *testing.T, body []byte) toolErrorResp {
	t.Helper()
	var r toolErrorResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if r.Error != nil {
		t.Fatalf("tool failure must be a result, got JSON-RPC error %+v", r.Error)
	}
	if !r.Result.IsError {
		t.Fatalf("isError = false, want true: %s", body)
	}
	return r
}

// The structured verdict: every field the contract names, and the same object
// serialized into the second text block for pre-structuredContent clients.
func TestToolsCall_ToolErrorShape(t *testing.T) {
	err := &domain.FetchError{
		Kind:       domain.KindHTTP429,
		StatusCode: 429,
		RetryAfter: 89500 * time.Millisecond,
		Err:        errors.New("crawl failed: HTTP 429 Too Many Requests"),
	}
	body := post(t, New(Config{Tools: []Tool{failingTool(err)}}),
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"fetch_url","arguments":{"url":"https://example.com/a"}}}`)
	r := decodeToolError(t, body)

	want := map[string]any{
		"code":            "rate_limited",
		"retryable":       true,
		"retry_after_s":   float64(90),
		"upstream_status": float64(429),
		"url":             "https://example.com/a",
	}
	if fmt.Sprint(r.Result.StructuredContent) != fmt.Sprint(want) {
		t.Errorf("structuredContent = %v, want %v", r.Result.StructuredContent, want)
	}
	if len(r.Result.Content) != 2 || r.Result.Content[0].Type != "text" || r.Result.Content[1].Type != "text" {
		t.Fatalf("content = %+v, want two text blocks", r.Result.Content)
	}
	if txt := r.Result.Content[0].Text; !strings.HasPrefix(txt, "fetch_url failed: http_429 (HTTP 429)") ||
		!strings.HasSuffix(txt, "[rate_limited] Retryable after 90s.") {
		t.Errorf("human text = %q", txt)
	}
	var serialized map[string]any
	if err := json.Unmarshal([]byte(r.Result.Content[1].Text), &serialized); err != nil {
		t.Fatalf("second block is not JSON: %v", err)
	}
	if fmt.Sprint(serialized) != fmt.Sprint(want) {
		t.Errorf("serialized block = %v, want %v", serialized, want)
	}
	if r.Result.ResultType != "" {
		t.Errorf("legacy result must not carry resultType, got %q", r.Result.ResultType)
	}
}

// Each FailureKind lands on its documented code (docs/errors.md).
func TestToolsCall_ToolErrorCodes(t *testing.T) {
	cases := []struct {
		err       error
		code      string
		retryable bool
	}{
		{&domain.FetchError{Kind: domain.KindHTTP429}, "rate_limited", true},
		{&domain.FetchError{Kind: domain.KindHTTP403, StatusCode: 403}, "blocked", false},
		{&domain.FetchError{Kind: domain.KindBotBlock}, "blocked", false},
		{&domain.FetchError{Kind: domain.KindCaptcha}, "captcha", false},
		{&domain.FetchError{Kind: domain.KindTimeout}, "timeout", true},
		{context.DeadlineExceeded, "timeout", true},
		{&domain.FetchError{Kind: domain.KindThinContent}, "thin_content", false},
		{&domain.FetchError{Kind: domain.KindUpstreamError}, "upstream_error", true},
		{&domain.FetchError{Kind: domain.KindUpstreamRejected}, "upstream_error", false},
		{&domain.FetchError{Kind: domain.KindBadResponse}, "upstream_error", true},
		{&domain.FetchError{Kind: domain.KindQuotaExhausted, RetryAfter: 3 * time.Second}, "quota_exhausted", true},
		{fmt.Errorf("url rejected: %w", &domain.InvalidRequestError{Err: errors.New(`scheme "file" not allowed`)}), "invalid_request", false},
	}
	for _, tc := range cases {
		t.Run(tc.code+"/"+tc.err.Error(), func(t *testing.T) {
			body := post(t, New(Config{Tools: []Tool{failingTool(tc.err)}}),
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{}}}`)
			sc := decodeToolError(t, body).Result.StructuredContent
			if sc["code"] != tc.code || sc["retryable"] != tc.retryable {
				t.Errorf("structuredContent = %v, want code=%s retryable=%v", sc, tc.code, tc.retryable)
			}
			if _, has := sc["url"]; has {
				t.Errorf("no url argument, yet structuredContent has url: %v", sc)
			}
		})
	}
}

// The modern era decorates the error result like any other result.
func TestModernToolsCall_ToolErrorIsModernized(t *testing.T) {
	srv := New(Config{Tools: []Tool{failingTool(&domain.FetchError{Kind: domain.KindCaptcha})}})
	rec := postRec(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{"url":"https://x.test/"},`+modernMeta+`}}`,
		modernHeaders("tools/call", "fetch_url"))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	r := decodeToolError(t, rec.Body.Bytes())
	if r.Result.ResultType != "complete" || r.Result.StructuredContent["code"] != "captcha" {
		t.Errorf("result = %+v, want resultType complete and code captcha", r.Result)
	}
}

// stdio carries the same result shape.
func TestStdio_ToolErrorIsResult(t *testing.T) {
	srv := New(Config{Tools: []Tool{failingTool(&domain.FetchError{Kind: domain.KindTimeout})}})
	var out bytes.Buffer
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_url","arguments":{}}}` + "\n")
	if err := srv.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	if sc := decodeToolError(t, out.Bytes()).Result.StructuredContent; sc["code"] != "timeout" {
		t.Errorf("structuredContent = %v, want code timeout", sc)
	}
}

// Protocol-level failures stay JSON-RPC errors: an unknown tool, malformed
// params, and a handler's InvalidParams.
func TestToolsCall_ProtocolErrorsStayJSONRPC(t *testing.T) {
	paramTool := Tool{
		Name:        "web_search",
		InputSchema: map[string]any{"type": "object"},
		Handle: func(context.Context, map[string]any) (ToolResult, error) {
			return ToolResult{}, InvalidParams("missing required argument: query")
		},
	}
	srv := New(Config{Tools: []Tool{paramTool}})
	cases := map[string]struct {
		body string
		code int
	}{
		"unknown tool":   {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`, codeInvalidParams},
		"bad params":     {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":"x"}}`, codeInvalidParams},
		"invalid params": {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{}}}`, codeInvalidParams},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var resp struct {
				Error  *rpcError       `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(post(t, srv, tc.body), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error == nil || resp.Error.Code != tc.code || resp.Result != nil {
				t.Errorf("got error=%+v result=%s, want JSON-RPC error %d and no result", resp.Error, resp.Result, tc.code)
			}
		})
	}
}
