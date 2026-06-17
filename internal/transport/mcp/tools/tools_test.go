package tools

import (
	"testing"

	"github.com/kinorai/omnifeed/internal/engine/reddit"
	"github.com/kinorai/omnifeed/internal/transport/mcp"
)

// Both tools are read-only web reads. They must advertise readOnlyHint +
// openWorldHint so clients don't treat them as potentially destructive (the
// MCP default when annotations are absent) and gate them behind a prompt.
func TestTools_AnnotatedReadOnly(t *testing.T) {
	cases := map[string]mcp.Tool{
		"fetch_url":  FetchURL(nil, reddit.Options{}, nil),
		"web_search": WebSearch(nil, 10, nil),
	}
	for name, tool := range cases {
		t.Run(name, func(t *testing.T) {
			if tool.Annotations["readOnlyHint"] != true {
				t.Errorf("readOnlyHint: got %v, want true", tool.Annotations["readOnlyHint"])
			}
			if tool.Annotations["openWorldHint"] != true {
				t.Errorf("openWorldHint: got %v, want true", tool.Annotations["openWorldHint"])
			}
		})
	}
}
