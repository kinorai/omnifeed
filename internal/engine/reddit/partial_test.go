package reddit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// partialEngine wires a Reddit engine to a fake browser that serves the
// reddit_old.json thread and answers every /api/morechildren round with more.
func partialEngine(t *testing.T, more func() string) *Engine {
	t.Helper()
	thread := string(mustReadFixture(t, "reddit_old.json"))
	sess := &fakeSession{evalFn: func(js string) (string, error) {
		if strings.Contains(js, "/api/morechildren") {
			return more(), nil
		}
		return envStr(200, thread), nil
	}}
	return New(Config{
		Fetcher: NewFetcher(FetcherConfig{Browser: &fakeBrowser{name: "fake", session: sess}}),
		Limiter: httpx.NewDomainLimiter(2, 0),
		DefaultOpts: Options{
			Format:    "toon",
			MaxRounds: 3,
		},
	})
}

const partialURL = "https://www.reddit.com/r/news/comments/1t056xf/oxycontin_maker_purdue_pharma"

// A morechildren round that Reddit blocks mid-crawl must not pass for a
// complete thread: the crawl succeeds with what it has, flagged partial in
// _meta and with a note an agent reading only the body will see.
func TestCrawl_BlockedMoreChildrenMarksPartial(t *testing.T) {
	for _, format := range []string{"toon", "json"} {
		t.Run(format, func(t *testing.T) {
			e := partialEngine(t, func() string { return envStr(429, "Too Many Requests") })
			doc, err := e.Crawl(context.Background(), partialURL, domain.EngineOptions{RedditFormat: format})
			if err != nil {
				t.Fatalf("Crawl: %v (a blocked expansion round must not fail the crawl)", err)
			}
			if doc.Metadata[domain.PartialKey] != "true" {
				t.Fatalf("partial = %q, want true; meta=%v", doc.Metadata[domain.PartialKey], doc.Metadata)
			}
			if got := doc.Metadata[domain.PartialReasonKey]; got != string(domain.KindHTTP429) {
				t.Errorf("partial_reason = %q, want %s", got, domain.KindHTTP429)
			}
			if doc.Metadata["missing_replies"] == "" || doc.Metadata["missing_replies"] == "0" {
				t.Errorf("missing_replies = %q, want > 0", doc.Metadata["missing_replies"])
			}
			wantNote := doc.Metadata["missing_replies"] + " more replies could not be loaded (http_429)"
			switch format {
			case "toon":
				first, _, _ := strings.Cut(doc.PageContent, "\n")
				if first != "note: "+wantNote {
					t.Errorf("first body line = %q, want %q", first, "note: "+wantNote)
				}
			case "json":
				var v map[string]any
				if err := json.Unmarshal([]byte(doc.PageContent), &v); err != nil {
					t.Fatalf("partial JSON body no longer parses: %v", err)
				}
				if v["note"] != wantNote {
					t.Errorf("json note = %v, want %q", v["note"], wantNote)
				}
			}
		})
	}
}

// Running out of the caller's round budget is not a failure: no partial flag,
// no note.
func TestCrawl_CompleteExpansionIsNotPartial(t *testing.T) {
	more := string(mustReadFixture(t, "morechildren.json"))
	e := partialEngine(t, func() string { return envStr(200, more) })
	doc, err := e.Crawl(context.Background(), partialURL, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if _, ok := doc.Metadata[domain.PartialKey]; ok {
		t.Errorf("partial set on a clean crawl: %v", doc.Metadata)
	}
	if strings.HasPrefix(doc.PageContent, "note:") {
		t.Errorf("clean crawl body starts with a note: %.80q", doc.PageContent)
	}
}
