package hackernews

import (
	"fmt"
	"slices"
	"testing"
)

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

// megaThreadSizes is the top-level subtree-size profile measured on real HN
// megathreads (item 49372583: 105, 55, 16, 13, 12, 11, 10, 6, … over 62
// top-level threads) — the shape the per-subtree cap exists for.
var megaThreadSizes = []int{105, 55, 16, 13, 12, 11, 10, 6}

// node is a synthetic comment tree node used to build a mega-thread fixture.
type node struct {
	id   int
	kids []*node
}

// buildBranch returns a branch of exactly n nodes, filled breadth-first with up
// to 2 replies per comment. Breadth-first fill means pre-order (document order)
// and breadth-first selection pick different comments, so a test can tell the
// two apart.
func buildBranch(nextID *int, n int) *node {
	root := &node{id: *nextID}
	*nextID++
	queue := []*node{root}
	for created := 1; created < n; {
		parent := queue[0]
		queue = queue[1:]
		for i := 0; i < 2 && created < n; i++ {
			kid := &node{id: *nextID}
			*nextID++
			parent.kids = append(parent.kids, kid)
			queue = append(queue, kid)
			created++
		}
	}
	return root
}

func (n *node) item() algoliaItem {
	it := algoliaItem{ID: n.id, Author: fmt.Sprintf("u%d", n.id), Text: fmt.Sprintf("comment %d", n.id)}
	for _, k := range n.kids {
		it.Children = append(it.Children, k.item())
	}
	return it
}

// megaThread returns a Thread whose top-level subtrees have the given sizes.
func megaThread(sizes []int) Thread {
	nextID := 2 // 1 is the story
	children := make([]algoliaItem, 0, len(sizes))
	for _, size := range sizes {
		children = append(children, buildBranch(&nextID, size).item())
	}
	var comments []Comment
	flattenComments(children, 1, &comments)
	return Thread{Story: Item{ID: 1, Title: "megathread"}, Comments: comments}
}

// bfsOrder returns the comment ids of one subtree in breadth-first order.
func bfsOrder(comments []Comment, root int) []int {
	idx := indexSubtrees(comments)
	maxDepth := 0
	for _, d := range idx.depth {
		if d > maxDepth {
			maxDepth = d
		}
	}
	var out []int
	for d := 0; d <= maxDepth; d++ {
		for _, c := range comments {
			if idx.root[c.ID] == root && idx.depth[c.ID] == d {
				out = append(out, c.ID)
			}
		}
	}
	return out
}

// capPerSubtree on a real mega-thread shape must keep min(size, cap) comments in
// every top-level thread, pick them breadth-first, and leave document order and
// parent links intact.
func TestCapPerSubtree(t *testing.T) {
	const perSubtree = 12
	full := megaThread(megaThreadSizes)
	if got, want := len(full.Comments), 228; got != want { // sum(megaThreadSizes)
		t.Fatalf("fixture has %d comments, want %d", got, want)
	}
	fullOrder := make([]int, len(full.Comments))
	for i, c := range full.Comments {
		fullOrder[i] = c.ID
	}

	capped := megaThread(megaThreadSizes)
	capPerSubtree(&capped, perSubtree)

	// 12+12+12+12+12+11+10+6 = 87
	wantTotal := 0
	for _, size := range megaThreadSizes {
		wantTotal += min(size, perSubtree)
	}
	if len(capped.Comments) != wantTotal {
		t.Errorf("kept %d comments, want %d", len(capped.Comments), wantTotal)
	}

	// Document order is unchanged: the kept ids are a subsequence of the full list.
	next := 0
	for _, c := range capped.Comments {
		for next < len(fullOrder) && fullOrder[next] != c.ID {
			next++
		}
		if next == len(fullOrder) {
			t.Fatalf("comment %d out of document order", c.ID)
		}
		next++
	}

	// Per-subtree counts, and the kept set is exactly the breadth-first prefix.
	idx := indexSubtrees(capped.Comments)
	perRoot := map[int][]int{}
	for _, c := range capped.Comments {
		r := idx.root[c.ID]
		perRoot[r] = append(perRoot[r], c.ID)
	}
	if len(perRoot) != len(megaThreadSizes) {
		t.Errorf("kept %d top-level threads, want %d", len(perRoot), len(megaThreadSizes))
	}
	fullIdx := indexSubtrees(full.Comments)
	var fullRoots []int
	for _, c := range full.Comments {
		if fullIdx.root[c.ID] == c.ID {
			fullRoots = append(fullRoots, c.ID)
		}
	}
	for i, size := range megaThreadSizes {
		root := fullRoots[i]
		want := bfsOrder(full.Comments, root)[:min(size, perSubtree)]
		got := perRoot[root]
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("subtree %d (size %d): kept %v, want breadth-first prefix %v", root, size, got, want)
		}
	}

	// No orphans: every kept comment's parent is kept, or is a subtree root's
	// parent (the story).
	present := map[int]bool{}
	for _, c := range capped.Comments {
		present[c.ID] = true
	}
	for _, c := range capped.Comments {
		if c.ParentID != 1 && !present[c.ParentID] {
			t.Errorf("comment %d kept but its parent %d was dropped", c.ID, c.ParentID)
		}
	}
}

// capPerSubtree(0) is unlimited, and a cap above every subtree size is a no-op.
func TestCapPerSubtreeNoop(t *testing.T) {
	for _, n := range []int{0, 1000} {
		th := megaThread(megaThreadSizes)
		capPerSubtree(&th, n)
		if len(th.Comments) != 228 {
			t.Errorf("capPerSubtree(%d) kept %d comments, want 228 (no-op)", n, len(th.Comments))
		}
	}
}

// capTopLevel keeps the first n top-level threads whole and drops the rest.
func TestCapTopLevel(t *testing.T) {
	th := megaThread(megaThreadSizes)
	capTopLevel(&th, 3)
	want := megaThreadSizes[0] + megaThreadSizes[1] + megaThreadSizes[2] // 176
	if len(th.Comments) != want {
		t.Fatalf("kept %d comments, want %d", len(th.Comments), want)
	}

	// 0 and an oversized cap are both no-ops.
	for _, n := range []int{0, 100} {
		all := megaThread(megaThreadSizes)
		capTopLevel(&all, n)
		if len(all.Comments) != 228 {
			t.Errorf("capTopLevel(%d) kept %d comments, want 228 (no-op)", n, len(all.Comments))
		}
	}
}

// capComments truncates the flat list, preserving order.
func TestCapComments(t *testing.T) {
	th := megaThread(megaThreadSizes)
	capComments(&th, 40)
	if len(th.Comments) != 40 {
		t.Fatalf("kept %d comments, want 40", len(th.Comments))
	}
	if th.Comments[0].ID != 2 {
		t.Errorf("first comment id = %d, want 2 (order preserved)", th.Comments[0].ID)
	}
}

// A live reply under a skipped deleted parent becomes its own top-level subtree
// (its parent was never emitted), so the caps never drop it as an orphan.
func TestCapsKeepRepliesUnderDeletedParent(t *testing.T) {
	tree := []algoliaItem{
		{ID: 2, Author: "alice", Text: "a", Children: []algoliaItem{{ID: 3, Author: "bob", Text: "b"}}},
		{ID: 4, Children: []algoliaItem{{ID: 5, Author: "carol", Text: "c"}}}, // deleted parent
	}
	var comments []Comment
	flattenComments(tree, 1, &comments)
	th := Thread{Story: Item{ID: 1}, Comments: comments}

	idx := indexSubtrees(th.Comments)
	if idx.root[5] != 5 || idx.depth[5] != 0 {
		t.Fatalf("comment 5 root/depth = %d/%d, want 5/0", idx.root[5], idx.depth[5])
	}
	capPerSubtree(&th, 1)
	if len(th.Comments) != 2 { // #2 (root of its branch) and #5 (its own root)
		t.Fatalf("kept %d comments, want 2: %+v", len(th.Comments), th.Comments)
	}
}
