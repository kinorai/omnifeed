package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
