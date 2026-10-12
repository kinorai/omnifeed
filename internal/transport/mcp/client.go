package mcp

import (
	"context"
	"strings"
	"sync/atomic"
)

// Client labels: the closed set omnifeed_mcp_tool_calls_total's `client`
// label takes. Anything unrecognized — or nothing at all — is "other".
const (
	clientClaudeCode  = "claude-code"
	clientOpenCode    = "opencode"
	clientOpenWebUI   = "openwebui"
	clientTheDigestif = "thedigestif"
	clientCursor      = "cursor"
	clientOther       = "other"
)

// ClientLabel maps an MCP clientInfo.name or an HTTP User-Agent to the bounded
// client label. Matching is a case-insensitive substring test, first match
// wins, because both shapes wrap the product name: clientInfo "claude-code",
// UA "claude-code/2.1.3 (cli)"; UA "thedigestif/1.4.0".
func ClientLabel(s string) string {
	s = strings.ToLower(s)
	switch {
	case s == "":
		return clientOther
	case strings.Contains(s, "thedigestif"):
		return clientTheDigestif
	case strings.Contains(s, "claude-code"), strings.Contains(s, "claude code"), strings.Contains(s, "claude-cli"):
		return clientClaudeCode
	case strings.Contains(s, "opencode"):
		return clientOpenCode
	case strings.Contains(s, "open-webui"), strings.Contains(s, "openwebui"), strings.Contains(s, "open webui"):
		return clientOpenWebUI
	case strings.Contains(s, "cursor"):
		return clientCursor
	default:
		return clientOther
	}
}

// clientInfo is the MCP Implementation object a client describes itself with
// (initialize params.clientInfo; per-request `_meta` in the modern era).
type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type clientCtxKey int

const (
	userAgentKey clientCtxKey = iota
	sessionKey
)

// session is per-connection state, and only the stdio transport has a
// connection: one subprocess serves one client, so the clientInfo its
// initialize carried names every later call. Streamable HTTP is stateless
// here (no Mcp-Session-Id), so it falls back to the User-Agent per request.
type session struct {
	client atomic.Pointer[clientInfo]
}

func withSession(ctx context.Context, s *session) context.Context {
	return context.WithValue(ctx, sessionKey, s)
}

func sessionFrom(ctx context.Context) *session {
	s, _ := ctx.Value(sessionKey).(*session)
	return s
}

func withUserAgent(ctx context.Context, ua string) context.Context {
	return context.WithValue(ctx, userAgentKey, ua)
}

func userAgentFrom(ctx context.Context) string {
	ua, _ := ctx.Value(userAgentKey).(string)
	return ua
}

// callerLabel resolves the client label for one request from, in order, the
// clientInfo the request itself carries (modern `_meta`), the one its stdio
// session announced at initialize, and the HTTP User-Agent — the first that
// names a known client wins, so an unrecognized clientInfo name does not hide
// a recognizable User-Agent.
func callerLabel(ctx context.Context, probe paramsProbe) string {
	candidates := []string{probe.Meta.ClientInfo.Name, "", userAgentFrom(ctx)}
	if s := sessionFrom(ctx); s != nil {
		if ci := s.client.Load(); ci != nil {
			candidates[1] = ci.Name
		}
	}
	for _, c := range candidates {
		if l := ClientLabel(c); l != clientOther {
			return l
		}
	}
	return clientOther
}
