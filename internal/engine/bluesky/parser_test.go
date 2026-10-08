package bluesky

import (
	"strconv"
	"strings"
	"testing"
)

// linkFacet builds a #link facet over the UTF-8 byte range [start, end).
func linkFacet(start, end int, uri string) facet {
	var f facet
	f.Index.ByteStart, f.Index.ByteEnd = start, end
	f.Features = []facetFeature{{Type: linkFeature, URI: uri}}
	return f
}

// span returns the byte range of sub in text (facets index UTF-8 bytes).
func span(t *testing.T, text, sub string) (int, int) {
	t.Helper()
	i := strings.Index(text, sub)
	if i < 0 {
		t.Fatalf("%q not in %q", sub, text)
	}
	return i, i + len(sub)
}

// record.text holds the shortened display form of a link; the full URL lives
// only in its #link facet, so expandLinks must splice it back in by byte range.
func TestExpandLinks(t *testing.T) {
	t.Run("ascii shortened link", func(t *testing.T) {
		text := "read this: example.com/a/very/lo..."
		s, e := span(t, text, "example.com/a/very/lo...")
		got := expandLinks(text, []facet{linkFacet(s, e, "https://example.com/a/very/long/path")})
		if want := "read this: https://example.com/a/very/long/path"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("emoji before the link shifts byte offsets", func(t *testing.T) {
		text := "🎉 new post: example.com/a/very… 🚀"
		// 🎉 is 4 UTF-8 bytes; "🎉 new post: " is 15 bytes but 12 runes, and
		// an index counted in runes would cut the link three bytes early.
		s, e := span(t, text, "example.com/a/very…")
		if s != 15 {
			t.Fatalf("byteStart = %d, want 15", s)
		}
		got := expandLinks(text, []facet{linkFacet(s, e, "https://example.com/a/very/long")})
		if want := "🎉 new post: https://example.com/a/very/long 🚀"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("multiple links, custom text, facets out of order", func(t *testing.T) {
		text := "docs at go.dev/doc/tutori… and the blog post, plus go.dev"
		s1, e1 := span(t, text, "go.dev/doc/tutori…")
		s2, e2 := span(t, text, "the blog post")
		s3 := strings.LastIndex(text, "go.dev") // the trailing bare one
		e3 := s3 + len("go.dev")
		got := expandLinks(text, []facet{
			linkFacet(s3, e3, "https://go.dev"),
			linkFacet(s1, e1, "https://go.dev/doc/tutorial/getting-started"),
			linkFacet(s2, e2, "https://go.dev/blog/go1.25"),
		})
		want := "docs at https://go.dev/doc/tutorial/getting-started and [the blog post](https://go.dev/blog/go1.25), plus https://go.dev"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("invalid ranges are skipped", func(t *testing.T) {
		text := "🎉 see example.com/x…"
		s, e := span(t, text, "example.com/x…")
		good := linkFacet(s, e, "https://example.com/x/full")
		got := expandLinks(text, []facet{
			linkFacet(-1, 3, "https://bad.example/negative"),
			linkFacet(0, len(text)+5, "https://bad.example/past-end"),
			linkFacet(5, 5, "https://bad.example/empty"),
			linkFacet(9, 4, "https://bad.example/reversed"),
			linkFacet(1, 3, "https://bad.example/mid-rune"), // inside 🎉
			linkFacet(s+2, e, "https://bad.example/overlap"),
			good,
		})
		if want := "🎉 see https://example.com/x/full"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("mentions and tags stay as text", func(t *testing.T) {
		text := "hi @alice.bsky.social #golang"
		s, e := span(t, text, "@alice.bsky.social")
		ts, te := span(t, text, "#golang")
		var m, tg facet
		m.Index.ByteStart, m.Index.ByteEnd = s, e
		m.Features = []facetFeature{{Type: "app.bsky.richtext.facet#mention"}}
		tg.Index.ByteStart, tg.Index.ByteEnd = ts, te
		tg.Features = []facetFeature{{Type: "app.bsky.richtext.facet#tag"}}
		if got := expandLinks(text, []facet{m, tg}); got != text {
			t.Errorf("got %q, want unchanged %q", got, text)
		}
	})
}

// Every rendered post — anchor, ancestors, replies, feed items — goes through
// toPost, so the wire facets must be decoded and applied on each.
func TestParseLinkFacets(t *testing.T) {
	post := func(uri, url string) string {
		short := "example.com/" + url[:3] + "…"
		full := "🙂 " + short
		start := len("🙂 ")
		return `{"uri":"` + uri + `","author":{"handle":"a.bsky.social"},"record":{"text":"` + full + `","createdAt":"2026-08-01T10:00:00Z",` +
			`"facets":[{"index":{"byteStart":` + strconv.Itoa(start) + `,"byteEnd":` + strconv.Itoa(start+len(short)) + `},` +
			`"features":[{"$type":"app.bsky.richtext.facet#link","uri":"https://example.com/` + url + `"}]}]}}`
	}
	raw := `{"thread":{"post":` + post("at://x/p/anchor", "anchor-full") + `,
		"parent":{"post":` + post("at://x/p/parent", "parent-full") + `},
		"replies":[{"post":` + post("at://x/p/reply", "reply-full") + `,"replies":[]}]}}`
	th, err := parseThread([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"anchor", th.Post.Text, "🙂 https://example.com/anchor-full"},
		{"ancestor", th.Ancestors[0].Text, "🙂 https://example.com/parent-full"},
		{"reply", th.Replies[0].Text, "🙂 https://example.com/reply-full"},
	} {
		if c.got != c.want {
			t.Errorf("%s text = %q, want %q", c.name, c.got, c.want)
		}
	}

	feed, err := parseFeed([]byte(`{"feed":[{"post":`+post("at://x/p/f", "feed-full")+`}]}`), "a.bsky.social")
	if err != nil {
		t.Fatal(err)
	}
	if want := "🙂 https://example.com/feed-full"; feed.Posts[0].Text != want {
		t.Errorf("feed text = %q, want %q", feed.Posts[0].Text, want)
	}
}
