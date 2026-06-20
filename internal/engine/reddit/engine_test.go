package reddit

import "testing"

// Matches must claim only the reddit.com URLs the engine can actually render —
// comments permalinks and share links — so everything else (profiles, wikis,
// search pages, /dev/api) falls through to the generic fallback engine instead
// of hard-failing in NormalizePermalink. The first two fall-through cases are
// real URLs that paged OmnifeedCrawlErrors before Matches was narrowed.
func TestMatches(t *testing.T) {
	e := &Engine{}

	claim := []string{
		"https://www.reddit.com/r/news/comments/1t056xf/oxycontin_maker_purdue_pharma",
		"https://www.reddit.com/r/news/comments/1t056xf",
		"https://old.reddit.com/r/news/comments/1t056xf/",
		"https://www.reddit.com/r/OpenWebUI/s/ibnxYbmeOE", // share link → resolves to a permalink
	}
	for _, u := range claim {
		if !e.Matches(u) {
			t.Errorf("Matches(%q) = false, want true (engine should claim it)", u)
		}
	}

	fallThrough := []string{
		"https://www.reddit.com/dev/api",                                    // API docs
		"https://www.reddit.com/r/kubernetes/search/?q=longhorn+nfs+backup", // subreddit search page
		"https://www.reddit.com/user/spez",                                  // user profile
		"https://www.reddit.com/r/golang",                                   // subreddit listing
		"https://www.reddit.com/r/golang/wiki/index",                        // wiki page
		"https://example.com/r/news/comments/1t056xf",                       // not reddit at all
	}
	for _, u := range fallThrough {
		if e.Matches(u) {
			t.Errorf("Matches(%q) = true, want false (should fall through to fallback)", u)
		}
	}
}
