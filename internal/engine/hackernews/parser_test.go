package hackernews

import "testing"

func TestCleanHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain text", "plain text"},
		{"<p>one<p>two", "one\n\ntwo"},
		{"a &amp; b &lt;c&gt;", "a & b <c>"},
		{`see <a href="https://x.com" rel="nofollow">https://x.com</a>`, "see https://x.com"},
		{"line1<p><p><p>line2", "line1\n\nline2"},
		// Code blocks must survive as Markdown fences (entity-encoded chars decoded inside).
		{"<pre><code>x := 1</code></pre>", "```\nx := 1\n```"},
		{"<pre><code>if a &gt; b {}</code></pre>", "```\nif a > b {}\n```"},
	}
	for _, tc := range cases {
		if got := cleanHTML(tc.in); got != tc.want {
			t.Errorf("cleanHTML(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// flattenComments must produce a pre-order, parent_id-linked list and skip
// deleted nodes while preserving their subtree's structure.
func TestFlattenComments(t *testing.T) {
	tree := []algoliaItem{
		{ID: 2, Author: "alice", Text: "a", Children: []algoliaItem{
			{ID: 3, Author: "bob", Text: "b"},
		}},
		{ID: 4, Author: "", Text: "", Children: []algoliaItem{ // deleted
			{ID: 5, Author: "carol", Text: "c"},
		}},
	}
	var out []Comment
	flattenComments(tree, 1, &out)

	if len(out) != 3 {
		t.Fatalf("got %d comments, want 3 (deleted #4 skipped): %+v", len(out), out)
	}
	want := []struct{ id, parent int }{{2, 1}, {3, 2}, {5, 4}}
	for i, w := range want {
		if out[i].ID != w.id || out[i].ParentID != w.parent {
			t.Errorf("out[%d] = (id %d, parent %d), want (id %d, parent %d)", i, out[i].ID, out[i].ParentID, w.id, w.parent)
		}
	}
}
