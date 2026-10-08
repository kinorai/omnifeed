package reddit

import (
	"context"
	"errors"
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

// crawlWithBody runs a full Engine.Crawl against a fake browser that serves
// threadJSON for the thread fetch and morechildren for /api/morechildren
// (morechildren == "" fails the expansion round, leaving the gaps in place).
func crawlWithBody(t *testing.T, threadJSON, morechildren string, eo domain.EngineOptions) domain.Document {
	t.Helper()
	sess := &fakeSession{evalFn: func(js string) (string, error) {
		if strings.Contains(js, "morechildren") {
			if morechildren == "" {
				return "", errors.New("expansion disabled in this test")
			}
			return envStr(200, morechildren), nil
		}
		return envStr(200, threadJSON), nil
	}}
	e := New(Config{
		Fetcher: NewFetcher(FetcherConfig{Browser: &fakeBrowser{name: "fake", session: sess}}),
		Limiter: httpx.NewDomainLimiter(1, 0),
	})
	doc, err := e.Crawl(context.Background(), "https://www.reddit.com/r/golang/comments/p1/t/", eo)
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	return doc
}

// assertStrictRoundTrip decodes out with toon-go's strict decoder (which
// enforces declared row counts and field widths), then re-encodes the decoded
// Thread and requires the identical document back.
func assertStrictRoundTrip(t *testing.T, out string) Thread {
	t.Helper()
	if strings.Contains(out, "[#") {
		t.Errorf("output carries a pre-v2 [#N] length marker:\n%.400s", out)
	}
	if _, err := toon.Decode([]byte(out), toon.WithStrictMode(true)); err != nil {
		t.Fatalf("strict decode: %v\n%.400s", err, out)
	}
	var th Thread
	if err := toon.Unmarshal([]byte(out), &th, toon.WithStrictMode(true)); err != nil {
		t.Fatalf("strict unmarshal: %v\n%.400s", err, out)
	}
	again, err := toon.Marshal(th)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if string(again) != out {
		t.Errorf("round-trip changed the document:\n--- engine\n%.600s\n--- re-encoded\n%.600s", out, again)
	}
	return th
}

// A real thread fixture (with an expansion round) must encode to spec-conformant
// TOON: name[N]{…}: tabular headers, totals in the post header, and a lossless
// strict round-trip.
func TestTOONConformanceFixture(t *testing.T) {
	doc := crawlWithBody(t, string(mustReadFixture(t, "reddit_old.json")),
		string(mustReadFixture(t, "morechildren.json")), domain.EngineOptions{RedditMaxComments: 150})
	out := doc.PageContent
	if !tabularHeaderRE.MatchString(out) {
		t.Errorf("no spec tabular header name[N]{...}: in:\n%.400s", out)
	}
	if !strings.Contains(out, "\ncomments[150]{id,parent_id,author,score,body}:\n") {
		t.Errorf("comments header not `comments[150]{...}:`:\n%.400s", out)
	}
	th := assertStrictRoundTrip(t, out)
	p := th.Post
	if p.TotalComments == nil || p.ReturnedComments == nil || p.HiddenMore == nil || p.Truncated == nil {
		t.Fatalf("post header missing totals: %+v", p)
	}
	if *p.TotalComments != p.NumComments || *p.ReturnedComments != 150 || !*p.Truncated {
		t.Errorf("totals = total %d returned %d truncated %v; want %d/150/true",
			*p.TotalComments, *p.ReturnedComments, *p.Truncated, p.NumComments)
	}
	if doc.Metadata["comments"] != "150" || doc.Metadata["total_comments"] == "" {
		t.Errorf("_meta changed: %v", doc.Metadata)
	}
}

// Edge cases the old encoder got wrong: a body with a control char (the old
// build failed the WHOLE encode, so the registry fell back to markdown), a
// leading '#' (must be quoted), and an empty comment list (`comments: []`).
func TestTOONConformanceEdgeCases(t *testing.T) {
	const post = `{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"p1","title":"# Release notes",
		"author":"op","subreddit":"golang","score":5,"upvote_ratio":0.9,"num_comments":%s,"created_utc":1700000000,
		"url":"https://example.com","permalink":"/r/golang/comments/p1/t/"}}]}}`
	withComments := `[` + strings.Replace(post, "%s", "7", 1) + `,{"kind":"Listing","data":{"children":[
		{"kind":"t1","data":{"id":"c1","parent_id":"t3_p1","author":"a","score":3,"body":"# heading-ish\u0001ctl","created_utc":1700000100,"replies":""}},
		{"kind":"t1","data":{"id":"c2","parent_id":"t3_p1","author":"b","score":1,"body":"a, b: \"q\"","created_utc":1700000200,"replies":""}},
		{"kind":"more","data":{"count":4,"parent_id":"t3_p1","depth":0,"children":["c3","c4","c5","c6"]}}
	]}}]`
	empty := `[` + strings.Replace(post, "%s", "0", 1) + `,{"kind":"Listing","data":{"children":[]}}]`

	t.Run("control_char_and_hash", func(t *testing.T) {
		out := crawlWithBody(t, withComments, "", domain.EngineOptions{}).PageContent
		t.Logf("TOON:\n%s", out)
		for _, want := range []string{
			`title: "# Release notes"`,
			"\n  total_comments: 7\n  returned_comments: 2\n  hidden_more: 4\n  truncated: true\n",
			"\ncomments[2]{id,parent_id,author,score,body}:\n",
			`c1,p1,a,3,"# heading-ish\u0001ctl"`,
			`c2,p1,b,1,"a, b: \"q\""`,
			"\ngaps[1]{type,parent_id,depth,count}:\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		th := assertStrictRoundTrip(t, out)
		if th.Comments[0].Body != "# heading-ish\x01ctl" {
			t.Errorf("body round-trip = %q", th.Comments[0].Body)
		}
	})
	t.Run("empty_comments", func(t *testing.T) {
		out := crawlWithBody(t, empty, "", domain.EngineOptions{}).PageContent
		t.Logf("TOON:\n%s", out)
		if !strings.Contains(out, "\ncomments: []") {
			t.Errorf("empty comment list not encoded as `comments: []`:\n%s", out)
		}
		if !strings.Contains(out, "\n  truncated: false\n") {
			t.Errorf("complete empty thread must say truncated: false:\n%s", out)
		}
		assertStrictRoundTrip(t, out)
	})
}
