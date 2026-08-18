package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/browser"
	"github.com/kinorai/omnifeed/internal/domain"
)

// --- fake browser backend ---

// fakeSession records what the searcher navigated to and what it evaluated, and
// replies with a scripted envelope, so the searcher's own logic (URL shaping,
// listing parsing) is tested without a real browser.
type fakeSession struct {
	navs   []string
	evals  []string
	body   string
	status int // envelope status; 0 means 200
}

func (s *fakeSession) Navigate(_ context.Context, rawURL string) error {
	s.navs = append(s.navs, rawURL)
	return nil
}

func (s *fakeSession) Eval(_ context.Context, js string) (string, error) {
	s.evals = append(s.evals, js)
	status := s.status
	if status == 0 {
		status = 200
	}
	env, _ := json.Marshal(fetchEnvelope{S: status, B: s.body})
	return string(env), nil
}

func (s *fakeSession) Close(context.Context) error { return nil }

type fakeBrowser struct{ session *fakeSession }

func (*fakeBrowser) Name() string { return "fake" }

func (b *fakeBrowser) Open(context.Context) (browser.Session, error) { return b.session, nil }

const listingFixture = `{"kind":"Listing","data":{"children":[
  {"kind":"t3","data":{
    "title":"Longhorn disk pressure after a node reboot",
    "permalink":"/r/kubernetes/comments/abc123/longhorn_disk_pressure/",
    "subreddit_name_prefixed":"r/kubernetes",
    "score":142,"num_comments":37,"created_utc":1755000000.0,
    "selftext":"Every reboot leaves replicas rebuilding and the disk fills up."}},
  {"kind":"t3","data":{
    "title":"Weekly discussion",
    "permalink":"/r/kubernetes/comments/def456/weekly/",
    "subreddit_name_prefixed":"r/kubernetes",
    "score":5,"num_comments":0,"created_utc":0,"selftext":""}}
]}}`

// searchedURL pulls the fetched URL back out of the in-page snippet.
func searchedURL(t *testing.T, js string) *url.URL {
	t.Helper()
	start := strings.Index(js, `"`)
	end := strings.Index(js[start+1:], `", {`)
	if start < 0 || end < 0 {
		t.Fatalf("cannot find the fetched URL in %q", js)
	}
	var raw string
	if err := json.Unmarshal([]byte(js[start:start+end+2]), &raw); err != nil {
		t.Fatalf("decode URL literal: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func newTestSearcher(body string) (*Searcher, *fakeSession) {
	session := &fakeSession{body: body}
	return New(Config{Browser: &fakeBrowser{session: session}}), session
}

// The subreddit scopes the in-site search, and it must not stay in the query
// text (Reddit would match "r/x" as a literal term). A query naming none runs
// sitewide.
func TestSearchExtractsTheSubreddit(t *testing.T) {
	for _, tc := range []struct {
		name, query    string
		wantPath       string
		wantQ          string
		wantRestrictSR string
	}{
		{
			name:     "subreddit token scopes the search and leaves the query",
			query:    "r/kubernetes longhorn disk pressure",
			wantPath: "/r/kubernetes/search.json", wantQ: "longhorn disk pressure",
			wantRestrictSR: "1",
		},
		{
			name:     "a mid-query token works too",
			query:    "longhorn in r/selfhosted please",
			wantPath: "/r/selfhosted/search.json", wantQ: "longhorn in please",
			wantRestrictSR: "1",
		},
		{
			// No subreddit runs sitewide: same in-site search, no restrict_sr,
			// which Reddit ignores outside /r/<sub>/ anyway.
			name:     "no subreddit searches all of reddit",
			query:    "longhorn disk pressure",
			wantPath: "/search.json", wantQ: "longhorn disk pressure",
			wantRestrictSR: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, session := newTestSearcher(listingFixture)
			_, err := s.Search(context.Background(), tc.query, domain.SearchOptions{})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			u := searchedURL(t, session.evals[0])
			if u.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", u.Path, tc.wantPath)
			}
			if got := u.Query().Get("q"); got != tc.wantQ {
				t.Errorf("q = %q, want %q", got, tc.wantQ)
			}
			if got := u.Query().Get("restrict_sr"); got != tc.wantRestrictSR {
				t.Errorf("restrict_sr = %q, want %q", got, tc.wantRestrictSR)
			}
		})
	}
}

func TestSearchTimeWindowAndLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      domain.SearchOptions
		wantT     string
		wantLimit string
	}{
		{"no range defaults to a year", domain.SearchOptions{}, "year", "10"},
		{"range maps through", domain.SearchOptions{TimeRange: "week"}, "week", "10"},
		{"unknown range falls back to the default", domain.SearchOptions{TimeRange: "hour"}, "year", "10"},
		{"limit is passed through", domain.SearchOptions{Limit: 4}, "year", "4"},
		{"oversized limit is clamped", domain.SearchOptions{Limit: 900}, "year", "25"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, session := newTestSearcher(listingFixture)
			if _, err := s.Search(context.Background(), "r/kubernetes longhorn", tc.opts); err != nil {
				t.Fatalf("Search: %v", err)
			}
			q := searchedURL(t, session.evals[0]).Query()
			if q.Get("t") != tc.wantT || q.Get("limit") != tc.wantLimit {
				t.Errorf("t=%q limit=%q, want %q/%q", q.Get("t"), q.Get("limit"), tc.wantT, tc.wantLimit)
			}
		})
	}
}

func TestSearchMapsListingPosts(t *testing.T) {
	s, _ := newTestSearcher(listingFixture)
	results, err := s.Search(context.Background(), "r/kubernetes longhorn", domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results: got %d, want 2", len(results))
	}

	first := results[0]
	if first.URL != "https://www.reddit.com/r/kubernetes/comments/abc123/longhorn_disk_pressure/" {
		t.Errorf("URL = %q", first.URL)
	}
	if first.Engine != "reddit" || first.PublishedDate != "2025-08-12T12:00:00Z" {
		t.Errorf("engine/date mapped wrong: %+v", first)
	}
	want := "142 points, 37 comments in r/kubernetes — Every reboot leaves replicas rebuilding and the disk fills up."
	if first.Snippet != want {
		t.Errorf("snippet = %q, want %q", first.Snippet, want)
	}
	// No selftext and no timestamp: the snippet keeps the counts, the date stays
	// empty rather than becoming the epoch.
	if results[1].Snippet != "5 points, 0 comments in r/kubernetes" || results[1].PublishedDate != "" {
		t.Errorf("second result mapped wrong: %+v", results[1])
	}
}

// A Reddit-side block arrives as a non-200 inside the in-page envelope, not as
// a browser error, so it must still surface as a classified failure.
func TestSearchClassifiesABlockedResponse(t *testing.T) {
	s := New(Config{Browser: &fakeBrowser{session: &fakeSession{status: 403, body: "blocked"}}})
	_, err := s.Search(context.Background(), "r/kubernetes longhorn", domain.SearchOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("want *domain.FetchError, got %T: %v", err, err)
	}
	if fe.Kind != domain.KindHTTP403 {
		t.Fatalf("Kind = %q, want %q", fe.Kind, domain.KindHTTP403)
	}
}
