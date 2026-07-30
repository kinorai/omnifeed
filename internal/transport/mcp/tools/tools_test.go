package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/engine/reddit"
	"github.com/kinorai/omnifeed/internal/transport/mcp"
)

// Both tools are read-only web reads. They must advertise readOnlyHint +
// openWorldHint so clients don't treat them as potentially destructive (the
// MCP default when annotations are absent) and gate them behind a prompt.
func TestTools_AnnotatedReadOnly(t *testing.T) {
	cases := map[string]mcp.Tool{
		"fetch_url":  FetchURL(nil, reddit.Options{}, nil, 0),
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

// Claude Code caps a tool's text result at ~25k tokens unless the tool declares
// anthropic/maxResultSizeChars in tools/list; without it, a large page is
// spooled to a file instead of reaching the model. Only fetch_url returns page
// text, so only fetch_url declares it.
func TestFetchURL_DeclaresMaxResultSizeAnnotation(t *testing.T) {
	fetch := FetchURL(nil, reddit.Options{}, nil, 0)
	if got := fetch.Meta["anthropic/maxResultSizeChars"]; got != MaxFetchChars {
		t.Errorf("fetch_url _meta[anthropic/maxResultSizeChars]: got %v, want %d", got, MaxFetchChars)
	}
	if search := WebSearch(nil, 10, nil); search.Meta != nil {
		t.Errorf("web_search must not declare _meta, got %v", search.Meta)
	}
}

func TestFetchURL_SizeParamsInSchema(t *testing.T) {
	props := FetchURL(nil, reddit.Options{}, nil, 120000).
		InputSchema["properties"].(map[string]any)
	for _, name := range []string{"max_chars", "start_char"} {
		p, isObject := props[name].(map[string]any)
		if !isObject {
			t.Fatalf("%s missing from fetch_url schema", name)
		}
		if p["type"] != "integer" {
			t.Errorf("%s type: got %v, want integer", name, p["type"])
		}
		if p["minimum"] != 0 {
			t.Errorf("%s minimum: got %v, want 0", name, p["minimum"])
		}
		if desc, _ := p["description"].(string); !strings.Contains(desc, "arkdown") {
			t.Errorf("%s description must say it is markdown-only, got %q", name, desc)
		}
	}
}

// stubEngine returns a fixed Document for any URL.
type stubEngine struct{ doc domain.Document }

func (stubEngine) Name() string        { return "stub" }
func (stubEngine) Matches(string) bool { return true }
func (e stubEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	return e.doc, nil
}

func stubRegistry(content, contentType string) *engine.Registry {
	return engine.New().Fallback(stubEngine{doc: domain.Document{
		PageContent: content,
		Metadata: map[string]string{
			"source":              "https://example.com/page",
			domain.ContentTypeKey: contentType,
		},
	}})
}

func TestFetchURL_SizeControl(t *testing.T) {
	const markdown = "0123456789abcdefghij" // 20 chars

	cases := []struct {
		name           string
		content        string
		contentType    string
		serverDefault  int
		args           map[string]any
		wantText       string
		wantMetaAbsent bool
		wantTruncated  string
		wantTotal      string
		wantReturned   string
		wantNextStart  string
	}{
		{
			name:    "caller max_chars wins over the server default",
			content: markdown, contentType: domain.ContentTypeMarkdown,
			serverDefault: 15,
			args:          map[string]any{"url": "https://example.com/page", "max_chars": float64(5)},
			wantText:      "01234",
			wantTruncated: "true", wantTotal: "20", wantReturned: "5", wantNextStart: "5",
		},
		{
			name:    "server default applies when max_chars is omitted",
			content: markdown, contentType: domain.ContentTypeMarkdown,
			serverDefault: 8,
			args:          map[string]any{"url": "https://example.com/page"},
			wantText:      "01234567",
			wantTruncated: "true", wantTotal: "20", wantReturned: "8", wantNextStart: "8",
		},
		{
			name:    "max_chars 0 falls back to the server default",
			content: markdown, contentType: domain.ContentTypeMarkdown,
			serverDefault: 8,
			args:          map[string]any{"url": "https://example.com/page", "max_chars": float64(0)},
			wantText:      "01234567",
			wantTruncated: "true", wantTotal: "20", wantReturned: "8", wantNextStart: "8",
		},
		{
			name:    "start_char continues from an offset",
			content: markdown, contentType: domain.ContentTypeMarkdown,
			serverDefault: 8,
			args:          map[string]any{"url": "https://example.com/page", "start_char": float64(8)},
			wantText:      "89abcdef",
			wantTruncated: "true", wantTotal: "20", wantReturned: "8", wantNextStart: "16",
		},
		{
			name:    "server default 0 means unlimited",
			content: markdown, contentType: domain.ContentTypeMarkdown,
			serverDefault:  0,
			args:           map[string]any{"url": "https://example.com/page"},
			wantText:       markdown,
			wantMetaAbsent: true,
		},
		{
			name:    "content under the cap is untouched",
			content: markdown, contentType: domain.ContentTypeMarkdown,
			serverDefault:  500,
			args:           map[string]any{"url": "https://example.com/page"},
			wantText:       markdown,
			wantMetaAbsent: true,
		},
		{
			// Byte-cutting TOON would leave its length markers describing rows
			// that are no longer there.
			name:    "TOON content is never truncated",
			content: markdown, contentType: domain.ContentTypeTOON,
			serverDefault:  5,
			args:           map[string]any{"url": "https://example.com/page", "max_chars": float64(5)},
			wantText:       markdown,
			wantMetaAbsent: true,
		},
		{
			name:    "JSON content is never truncated",
			content: markdown, contentType: domain.ContentTypeJSON,
			serverDefault:  5,
			args:           map[string]any{"url": "https://example.com/page", "max_chars": float64(5)},
			wantText:       markdown,
			wantMetaAbsent: true,
		},
		{
			name:    "start_char past the end says so instead of returning empty",
			content: markdown, contentType: domain.ContentTypeMarkdown,
			serverDefault: 8,
			args:          map[string]any{"url": "https://example.com/page", "start_char": float64(20)},
			wantText:      "[omnifeed: no content at offset 20 — total 20 characters]",
			wantTruncated: "false", wantTotal: "20", wantReturned: "0", wantNextStart: "20",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := FetchURL(stubRegistry(tc.content, tc.contentType), reddit.Options{}, nil, tc.serverDefault)
			res, err := tool.Handle(context.Background(), tc.args)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}

			text, marker := res.Text, ""
			if i := strings.Index(text, "\n\n[omnifeed: content truncated"); i >= 0 {
				text, marker = text[:i], text[i+2:]
			}
			if text != tc.wantText {
				t.Errorf("text: got %q, want %q", text, tc.wantText)
			}
			if res.Meta["source"] != "https://example.com/page" {
				t.Errorf("engine metadata must survive: got %v", res.Meta)
			}

			if tc.wantMetaAbsent {
				if marker != "" {
					t.Errorf("unexpected truncation marker: %q", marker)
				}
				for _, k := range []string{"truncated", "total_chars", "returned_chars", "next_start_char"} {
					if v, exists := res.Meta[k]; exists {
						t.Errorf("meta[%s] must be absent, got %q", k, v)
					}
				}
				return
			}

			want := map[string]string{
				"truncated":       tc.wantTruncated,
				"total_chars":     tc.wantTotal,
				"returned_chars":  tc.wantReturned,
				"next_start_char": tc.wantNextStart,
			}
			for k, v := range want {
				if res.Meta[k] != v {
					t.Errorf("meta[%s]: got %q, want %q", k, res.Meta[k], v)
				}
			}
			if tc.wantTruncated == "true" {
				wantMarker := "[omnifeed: content truncated at " + tc.wantNextStart + " of " + tc.wantTotal +
					" characters. Call fetch_url again with start_char=" + tc.wantNextStart + " to continue.]"
				if marker != wantMarker {
					t.Errorf("marker:\n got %q\nwant %q", marker, wantMarker)
				}
			}
		})
	}
}

// A caller asking for more than the annotation ceiling is clamped, not honored.
func TestFetchURL_MaxCharsClampedToCeiling(t *testing.T) {
	if got := resolveMaxChars(map[string]any{"max_chars": float64(MaxFetchChars + 1)}, 100); got != MaxFetchChars {
		t.Errorf("resolveMaxChars: got %d, want %d", got, MaxFetchChars)
	}
}
