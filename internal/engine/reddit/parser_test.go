package reddit

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

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
