package mcp

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/kinorai/omnifeed/internal/observability"
)

// Tool is one MCP tool: the schema surfaced by tools/list plus the handler
// invoked by tools/call. The transport stays generic — domain-specific
// behavior lives in the handlers (see the tools subpackage), so adding a tool
// never touches the JSON-RPC plumbing.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Annotations are optional MCP ToolAnnotations surfaced in tools/list —
	// behavioral hints like readOnlyHint and openWorldHint that let clients
	// decide how much friction to put in front of a call (a read-only tool can
	// be auto-approved). nil sends no annotations.
	Annotations map[string]any
	// Meta is serialized as the tool's `_meta` object in tools/list — the MCP
	// escape hatch for client-specific hints. fetch_url uses it to declare
	// `anthropic/maxResultSizeChars`, which raises Claude Code's per-tool text
	// cap; clients that don't know the key ignore it. nil sends no `_meta`.
	Meta   map[string]any
	Handle func(ctx context.Context, args map[string]any) (ToolResult, error)
}

// ToolResult is what a Tool handler returns: the text content plus optional
// metadata serialized into the response _meta field.
type ToolResult struct {
	Text string
	Meta map[string]string
}

// ParamError marks a tools/call failure as a caller mistake (JSON-RPC
// -32602 invalid params) instead of an internal error. Handlers return it
// via InvalidParams.
type ParamError struct{ msg string }

func (e ParamError) Error() string { return e.msg }

// InvalidParams returns a ParamError with the given message.
func InvalidParams(msg string) error { return ParamError{msg: msg} }

// toolFailureMessage is the human-readable text of a failed tools/call: the
// tool name plus the classified reason, upstream status, and root cause that
// observability already records as metric labels. A bare "<tool> failed" tells
// the calling agent nothing about whether to retry, use another URL, or give up.
func toolFailureMessage(tool string, err error) string {
	return tool + " failed: " + observability.Explain(err)
}

// toolErrorResult renders a failed tools/call as a CallToolResult with
// isError: true (MCP tool execution error). content[0] is the short human
// text — what failed, why, and whether to retry; structuredContent carries the
// same verdict as stable fields (code, retryable, retry_after_s?,
// upstream_status?, url?) so clients branch without parsing prose. The
// structured object is also serialized into a second text block, as the spec
// recommends for clients that predate structuredContent (added in 2025-06-18;
// older clients ignore the unknown field). Codes: see docs/errors.md.
func toolErrorResult(tool string, args map[string]any, err error) map[string]any {
	f := observability.Classify(err)
	structured := f.Fields()
	if u, isString := args["url"].(string); isString && u != "" {
		structured["url"] = u
	}
	text := toolFailureMessage(tool, err) + " [" + string(f.Code) + "] " + retryHint(f)
	serialized, _ := json.Marshal(structured)
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
			{"type": "text", "text": string(serialized)},
		},
		"structuredContent": structured,
		"isError":           true,
	}
}

// retryHint is the caller-facing retry advice for a failure.
func retryHint(f observability.Failure) string {
	switch {
	case !f.Retryable:
		return "Not retryable."
	case f.RetryAfterSeconds() > 0:
		return "Retryable after " + strconv.Itoa(f.RetryAfterSeconds()) + "s."
	default:
		return "Retryable."
	}
}
