package hackernews

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/toon-format/toon-go"
)

// tabularHeaderRE is a TOON spec v4 tabular array header: key[N]{f1,f2,…}:
// The pre-v2 "[#N]" length marker is gone (encoders MUST NOT emit it).
var tabularHeaderRE = regexp.MustCompile(`(?m)^\s*[a-z_]+\[\d+\]\{[a-z_,]+\}:$`)

// The engine's real TOON output must be spec-conformant and round-trip through
// toon-go's own decoder in strict mode: no "[#N]" markers, tabular headers as
// name[N]{…}:, a body with a control char and a leading '#' quoted and escaped
// (the old encoder failed the whole document on such a char, so the registry
// fell back to markdown), and an empty comment list encoded as an empty array.
func TestTOONConformance(t *testing.T) {
	const thread = `{"id":1,"created_at_i":1700000000,"type":"story","author":"pg","title":"# Not a heading",
		"text":"<p># Ask HN body","points":7,"children":[
		{"id":2,"author":"alice","text":"# looks like a comment\u0001with a control char","created_at_i":1700000100,"parent_id":1,"children":[
			{"id":3,"author":"bob","text":"a, b: \"quoted\"<p>second para","created_at_i":1700000200,"parent_id":2,"children":[]}
		]},
		{"id":4,"author":"carol","text":"plain","created_at_i":1700000300,"parent_id":1,"children":[]}
	]}`
	const empty = `{"id":9,"created_at_i":1700000000,"type":"story","author":"pg","title":"Quiet","points":1,"children":[]}`

	cases := []struct {
		name, body string
		want       Thread
	}{
		{"thread", thread, Thread{
			Story: Item{ID: 1, Title: "# Not a heading", Author: "pg", Points: 7, Created: 1700000000,
				TotalComments: 3, Truncated: false, Text: "# Ask HN body"},
			Comments: []Comment{
				{ID: 2, ParentID: 1, Author: "alice", Body: "# looks like a comment\x01with a control char", Created: 1700000100},
				{ID: 3, ParentID: 2, Author: "bob", Body: "a, b: \"quoted\"\n\nsecond para", Created: 1700000200},
				{ID: 4, ParentID: 1, Author: "carol", Body: "plain", Created: 1700000300},
			},
		}},
		{"empty_comments", empty, Thread{
			Story:    Item{ID: 9, Title: "Quiet", Author: "pg", Points: 1, Created: 1700000000},
			Comments: []Comment{},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
			doc, err := e.Crawl(context.Background(), "https://news.ycombinator.com/item?id=1", domain.EngineOptions{})
			if err != nil {
				t.Fatalf("Crawl: %v", err)
			}
			out := doc.PageContent
			t.Logf("TOON:\n%s", out)

			if strings.Contains(out, "[#") {
				t.Errorf("output carries a pre-v2 [#N] length marker:\n%s", out)
			}
			if len(tc.want.Comments) > 0 {
				if !tabularHeaderRE.MatchString(out) {
					t.Errorf("no spec tabular header name[N]{...}: in:\n%s", out)
				}
				for _, want := range []string{
					`title: "# Not a heading"`,                          // a leading '#' is quoted
					`"# looks like a comment\u0001with a control char"`, // control char escaped as \uXXXX
					`"a, b: \"quoted\"\n\nsecond para"`,                 // delimiter, quotes and newlines escaped
				} {
					if !strings.Contains(out, want) {
						t.Errorf("output missing %s:\n%s", want, out)
					}
				}
			} else if !strings.Contains(out, "\ncomments: []") {
				t.Errorf("empty comment list not encoded as `comments: []`:\n%s", out)
			}

			// Strict-mode decode: the generic decoder enforces the spec's
			// structural checks (declared vs actual row counts, field widths).
			if _, derr := toon.Decode([]byte(out), toon.WithStrictMode(true)); derr != nil {
				t.Fatalf("strict decode: %v\n%s", derr, out)
			}
			var got Thread
			if uerr := toon.Unmarshal([]byte(out), &got, toon.WithStrictMode(true)); uerr != nil {
				t.Fatalf("strict unmarshal: %v\n%s", uerr, out)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("round-trip mismatch:\n got  %+v\n want %+v\n%s", got, tc.want, out)
			}
		})
	}
}
