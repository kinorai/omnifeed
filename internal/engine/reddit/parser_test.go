package reddit

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/toon-format/toon-go"
)

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func defaultOpts() Options {
	return Options{KeepDepth: false, KeepCreated: true, MaxRounds: 3, Format: "toon"}
}

func TestParseThread_Fixture(t *testing.T) {
	raw := mustReadFixture(t, "reddit_old.json")
	thread, err := ParseThread(raw, defaultOpts())
	if err != nil {
		t.Fatalf("ParseThread: %v", err)
	}
	if thread.Post.ID != "1t056xf" {
		t.Errorf("post id = %q, want 1t056xf", thread.Post.ID)
	}
	if thread.Post.NumComments < 3000 {
		t.Errorf("num_comments = %d, want >=3000", thread.Post.NumComments)
	}
	if len(thread.Comments) < 400 {
		t.Errorf("comments captured = %d, want >=400", len(thread.Comments))
	}
	if len(thread.Gaps) < 100 {
		t.Errorf("gaps = %d, want >=100", len(thread.Gaps))
	}
	for i, c := range thread.Comments {
		if c.ID == "" || c.ParentID == "" || c.Author == "" || c.Body == "" {
			t.Errorf("comment[%d] missing required fields: %+v", i, c)
			break
		}
	}
	for i, c := range thread.Comments {
		if strings.HasPrefix(c.ParentID, "t1_") || strings.HasPrefix(c.ParentID, "t3_") {
			t.Errorf("comment[%d].parent_id has unstripped prefix: %q", i, c.ParentID)
			break
		}
	}
	t.Logf("post=%s comments=%d gaps=%d", thread.Post.ID, len(thread.Comments), len(thread.Gaps))
}

func TestParseThread_DropsDepthByDefault(t *testing.T) {
	raw := mustReadFixture(t, "reddit_old.json")
	thread, err := ParseThread(raw, defaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range thread.Comments {
		if c.Depth != nil {
			t.Errorf("default opts should drop depth, but comment %s has depth=%d", c.ID, *c.Depth)
			break
		}
	}
}

func TestParseThread_KeepsDepthOnRequest(t *testing.T) {
	raw := mustReadFixture(t, "reddit_old.json")
	opts := defaultOpts()
	opts.KeepDepth = true
	thread, err := ParseThread(raw, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range thread.Comments {
		if c.Depth != nil {
			return
		}
	}
	t.Error("keepDepth=true should retain depth on at least one comment")
}

func TestParseMoreChildren_Fixture(t *testing.T) {
	raw := mustReadFixture(t, "morechildren.json")
	comments, gaps, err := ParseMoreChildren(raw, defaultOpts())
	if err != nil {
		t.Fatalf("ParseMoreChildren: %v", err)
	}
	if len(comments) == 0 {
		t.Error("expected at least one comment from morechildren fixture")
	}
	for _, c := range comments {
		if strings.HasPrefix(c.ParentID, "t1_") || strings.HasPrefix(c.ParentID, "t3_") {
			t.Errorf("morechildren parent_id has prefix: %q", c.ParentID)
			break
		}
	}
	t.Logf("morechildren expansion: %d comments, %d gaps", len(comments), len(gaps))
}

func TestMergeExpanded_RemovesFulfilledGapsAndDedupes(t *testing.T) {
	thread := Thread{
		Comments: []Comment{{ID: "a", ParentID: "post", Author: "u1", Score: 1, Body: "hi"}},
		Gaps: []Gap{
			{Type: "more", ParentID: "a", Depth: 1, Count: 3, Children: []string{"b", "c", "d"}},
			{Type: "more", ParentID: "x", Depth: 2, Count: 1, Children: []string{"e"}},
		},
	}
	newComments := []Comment{
		{ID: "a", ParentID: "post", Author: "u1", Score: 1, Body: "hi"},
		{ID: "b", ParentID: "a", Author: "u2", Score: 2, Body: "yo"},
		{ID: "c", ParentID: "a", Author: "u3", Score: 3, Body: "hey"},
	}
	MergeExpanded(&thread, newComments, nil, []string{"b", "c"}, []int{0})
	if len(thread.Comments) != 3 {
		t.Errorf("got %d comments, want 3", len(thread.Comments))
	}
	if len(thread.Gaps) != 2 {
		t.Fatalf("got %d gaps, want 2", len(thread.Gaps))
	}
	if thread.Gaps[0].Count != 1 || len(thread.Gaps[0].Children) != 1 || thread.Gaps[0].Children[0] != "d" {
		t.Errorf("first gap should have count=1 children=[d], got %+v", thread.Gaps[0])
	}
}

func TestNormalizePermalink(t *testing.T) {
	cases := map[string]string{
		"https://www.reddit.com/r/news/comments/1t056xf/oxycontin_maker_purdue_pharma":        "/r/news/comments/1t056xf/oxycontin_maker_purdue_pharma",
		"https://www.reddit.com/r/news/comments/1t056xf/oxycontin_maker_purdue_pharma/":       "/r/news/comments/1t056xf/oxycontin_maker_purdue_pharma",
		"https://old.reddit.com/r/news/comments/1t056xf/":                                     "/r/news/comments/1t056xf",
		"https://www.reddit.com/r/news/comments/1t056xf":                                      "/r/news/comments/1t056xf",
		"https://www.reddit.com/r/news/comments/1t056xf/foo.json":                             "/r/news/comments/1t056xf/foo",
		"https://www.reddit.com/r/news/comments/1t056xf/oxycontin_maker_purdue_pharma/?x=1#y": "/r/news/comments/1t056xf/oxycontin_maker_purdue_pharma",
	}
	for input, want := range cases {
		got, err := NormalizePermalink(input)
		if err != nil {
			t.Errorf("%s: error %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("%s\n  got:  %q\n  want: %q", input, got, want)
		}
	}
}

func TestCleanBody(t *testing.T) {
	cases := map[string]string{
		"hello   world":                  "hello world",
		"hello \n \n world":              "hello\n\nworld",
		"  trim me  ":                    "trim me",
		"line1\n\n\n\nline2":             "line1\n\nline2",
		"para1\n\npara2":                 "para1\n\npara2",
		"trailing   \nnext":              "trailing\nnext",
		"single\nbreak":                  "single\nbreak",
		"text\n\n```\ncode\n```\n\nmore": "text\n\n```\ncode\n```\n\nmore",
	}
	for in, want := range cases {
		if got := cleanBody(in); got != want {
			t.Errorf("cleanBody(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsRedditURL(t *testing.T) {
	yes := []string{
		"https://www.reddit.com/r/news/comments/1t056xf",
		"https://old.reddit.com/r/news/comments/1t056xf",
		"https://reddit.com/r/news/comments/1t056xf",
		"https://api.reddit.com/api/morechildren",
	}
	no := []string{
		"https://www.notreddit.com/r/news",
		"https://reddit.com.evil.com/",
		"https://example.com/reddit.com",
	}
	for _, u := range yes {
		if !IsRedditURL(u) {
			t.Errorf("expected reddit: %s", u)
		}
	}
	for _, u := range no {
		if IsRedditURL(u) {
			t.Errorf("expected non-reddit: %s", u)
		}
	}
}

func TestToonEncoding_NonEmpty(t *testing.T) {
	raw := mustReadFixture(t, "reddit_old.json")
	thread, err := ParseThread(raw, defaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := toon.Marshal(thread, toon.WithLengthMarkers(true))
	if err != nil {
		t.Fatalf("toon.Marshal: %v", err)
	}
	s := string(encoded)
	if !strings.Contains(s, "post:") {
		t.Error("TOON output missing post: section")
	}
	if !strings.Contains(s, "comments[") {
		t.Error("TOON output missing comments[ tabular header")
	}
	if !strings.Contains(s, "{id,parent_id,author,score,body") {
		end := 400
		if len(s) < end {
			end = len(s)
		}
		t.Errorf("TOON output missing expected fields header; first chars:\n%s", s[:end])
	}
	j, _ := json.Marshal(thread)
	t.Logf("TOON: %d bytes, JSON: %d bytes (reduction: %.1f%%)",
		len(encoded), len(j), 100*(1-float64(len(encoded))/float64(len(j))))
}

func TestCapComments(t *testing.T) {
	mk := func(ids ...string) []Comment {
		cs := make([]Comment, len(ids))
		for i, id := range ids {
			cs[i] = Comment{ID: id, ParentID: "p", Body: "x"}
		}
		return cs
	}
	t.Run("truncates to n preserving order", func(t *testing.T) {
		thread := Thread{Post: Post{ID: "p"}, Comments: mk("a", "b", "c", "d", "e")}
		capComments(&thread, 3)
		if len(thread.Comments) != 3 || thread.Comments[0].ID != "a" || thread.Comments[2].ID != "c" {
			t.Fatalf("got %+v, want [a b c]", thread.Comments)
		}
	})
	t.Run("zero is unlimited", func(t *testing.T) {
		thread := Thread{Post: Post{ID: "p"}, Comments: mk("a", "b")}
		capComments(&thread, 0)
		if len(thread.Comments) != 2 {
			t.Errorf("n=0 must not truncate, got %d", len(thread.Comments))
		}
	})
}

func TestCapTopLevel(t *testing.T) {
	// post p: t1 -> t1a ; t2 -> t2a -> t2a1 ; t3
	newThread := func() Thread {
		return Thread{
			Post: Post{ID: "p"},
			Comments: []Comment{
				{ID: "t1", ParentID: "p"},
				{ID: "t1a", ParentID: "t1"},
				{ID: "t2", ParentID: "p"},
				{ID: "t2a", ParentID: "t2"},
				{ID: "t2a1", ParentID: "t2a"},
				{ID: "t3", ParentID: "p"},
			},
		}
	}

	t.Run("keeps first n threads with full subtrees", func(t *testing.T) {
		thread := newThread()
		capTopLevel(&thread, 2)
		got := map[string]bool{}
		for _, c := range thread.Comments {
			got[c.ID] = true
		}
		for _, id := range []string{"t1", "t1a", "t2", "t2a", "t2a1"} {
			if !got[id] {
				t.Errorf("expected %s kept; have %+v", id, thread.Comments)
			}
		}
		if got["t3"] {
			t.Error("3rd top-level thread (t3) and its subtree must be dropped")
		}
	})

	t.Run("no-op when threads <= n", func(t *testing.T) {
		thread := newThread()
		capTopLevel(&thread, 5)
		if len(thread.Comments) != 6 {
			t.Errorf("cap above thread count must not drop anything, got %d", len(thread.Comments))
		}
	})

	t.Run("keeps comments with an unresolvable parent chain", func(t *testing.T) {
		// orphan's parent "ghost" was never in the list (e.g. deleted) -> keep it.
		thread := Thread{
			Post: Post{ID: "p"},
			Comments: []Comment{
				{ID: "t1", ParentID: "p"},
				{ID: "t2", ParentID: "p"},
				{ID: "orphan", ParentID: "ghost"},
			},
		}
		capTopLevel(&thread, 1)
		got := map[string]bool{}
		for _, c := range thread.Comments {
			got[c.ID] = true
		}
		if !got["t1"] || got["t2"] || !got["orphan"] {
			t.Errorf("want t1+orphan kept, t2 dropped; got %+v", thread.Comments)
		}
	})
}

func TestResolveOptions(t *testing.T) {
	e := &Engine{defaultOpts: Options{}}

	t.Run("hard fallbacks for reddit params", func(t *testing.T) {
		got := e.resolveOptions(domain.EngineOptions{})
		if got.FetchLimit != domain.DefaultRedditFetchLimit || got.Depth != domain.DefaultRedditDepth || got.Sort != domain.DefaultRedditSort {
			t.Errorf("fallbacks not applied: limit=%d depth=%d sort=%q", got.FetchLimit, got.Depth, got.Sort)
		}
	})

	t.Run("per-request overrides", func(t *testing.T) {
		got := e.resolveOptions(domain.EngineOptions{
			RedditFetchLimit: 50, RedditDepth: 3, RedditSort: "new",
			RedditMaxComments: 20, RedditMaxTopLevel: 5,
		})
		if got.FetchLimit != 50 || got.Depth != 3 || got.Sort != "new" || got.MaxComments != 20 || got.MaxTopLevel != 5 {
			t.Errorf("overrides not applied: %+v", got)
		}
	})

	t.Run("invalid sort ignored", func(t *testing.T) {
		got := e.resolveOptions(domain.EngineOptions{RedditSort: "bogus"})
		if got.Sort != domain.DefaultRedditSort {
			t.Errorf("invalid sort should fall back to %q, got %q", domain.DefaultRedditSort, got.Sort)
		}
	})
}

func TestParseListingURL(t *testing.T) {
	cases := []struct {
		url      string
		wantSub  string
		wantSort string
		wantOK   bool
	}{
		{"https://www.reddit.com/r/devops/", "devops", "hot", true},
		{"https://www.reddit.com/r/golang", "golang", "hot", true},
		{"https://old.reddit.com/r/golang/top", "golang", "top", true},
		{"https://www.reddit.com/r/golang/new/", "golang", "new", true},
		{"https://www.reddit.com/r/golang.json", "golang", "hot", true},     // .json suffix stripped
		{"https://www.reddit.com/r/golang/top.json", "golang", "top", true}, // sort + .json
		{"https://www.reddit.com/r/news/comments/1t056xf", "", "", false},   // a thread
		{"https://www.reddit.com/r/golang/wiki/index", "", "", false},       // wiki page
		{"https://www.reddit.com/r/golang/best", "", "", false},             // 'best' is not a subreddit sort
		{"https://www.reddit.com/user/spez", "", "", false},                 // profile
	}
	for _, tc := range cases {
		sub, sort, ok := ParseListingURL(tc.url)
		if ok != tc.wantOK || sub != tc.wantSub || sort != tc.wantSort {
			t.Errorf("ParseListingURL(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.url, sub, sort, ok, tc.wantSub, tc.wantSort, tc.wantOK)
		}
	}
}

func TestParseSubredditListing(t *testing.T) {
	raw := []byte(`{"kind":"Listing","data":{"children":[
		{"kind":"t3","data":{"id":"abc","title":"First","author":"alice","subreddit":"golang","score":42,"upvote_ratio":0.95,"num_comments":7,"created_utc":1700000000,"url":"https://example.com/a","permalink":"/r/golang/comments/abc/first/"}},
		{"kind":"t3","data":{"id":"def","title":"Second","author":"bob","subreddit":"golang","score":10,"num_comments":2,"created_utc":1700000100,"permalink":"/r/golang/comments/def/second/"}},
		{"kind":"more","data":{"id":"xyz"}}
	]}}`)
	posts, err := ParseSubredditListing(raw)
	if err != nil {
		t.Fatalf("ParseSubredditListing: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("posts = %d, want 2 (the 'more' child must be skipped)", len(posts))
	}
	if posts[0].ID != "abc" || posts[0].Title != "First" || posts[0].Author != "alice" || posts[0].Score != 42 {
		t.Errorf("posts[0] = %+v", posts[0])
	}
	if posts[0].Created != 1700000000 {
		t.Errorf("posts[0].Created = %d, want 1700000000", posts[0].Created)
	}
}
