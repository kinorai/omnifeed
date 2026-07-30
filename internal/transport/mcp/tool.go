package mcp

import (
	"context"

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
	Handle      func(ctx context.Context, args map[string]any) (ToolResult, error)
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

// toolFailureMessage is the JSON-RPC error message for a failed tools/call: the
// tool name plus the classified reason, upstream status, and root cause that
// observability already records as metric labels. A bare "<tool> failed" tells
// the calling agent nothing about whether to retry, use another URL, or give up.
func toolFailureMessage(tool string, err error) string {
	return tool + " failed: " + observability.Explain(err)
}
