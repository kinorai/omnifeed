package failover

import (
	"slices"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
)

func urls(rs []domain.SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.URL
	}
	return out
}

func hits(urls ...string) []domain.SearchResult {
	out := make([]domain.SearchResult, len(urls))
	for i, u := range urls {
		out[i] = domain.SearchResult{URL: u}
	}
	return out
}

func TestMerge(t *testing.T) {
	cases := []struct {
		name            string
		primary, backup []domain.SearchResult
		limit           int
		want            []string
	}{
		{
			name:    "interleaves primary first",
			primary: hits("https://p/1", "https://p/2", "https://p/3"),
			backup:  hits("https://b/1"),
			want:    []string{"https://p/1", "https://b/1", "https://p/2", "https://p/3"},
		},
		{
			name:    "drops the backup's copy of a page the primary already has",
			primary: hits("https://example.com/a/", "https://p/2"),
			backup:  hits("http://www.example.com/a#top", "https://b/2"),
			want:    []string{"https://example.com/a/", "https://p/2", "https://b/2"},
		},
		{
			name:    "clamps to limit",
			primary: hits("https://p/1", "https://p/2"),
			backup:  hits("https://b/1", "https://b/2"),
			limit:   3,
			want:    []string{"https://p/1", "https://b/1", "https://p/2"},
		},
		{
			name:   "empty primary is just the backup",
			backup: hits("https://b/1", "https://b/2"),
			want:   []string{"https://b/1", "https://b/2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := urls(merge(tc.primary, tc.backup, tc.limit)); !slices.Equal(got, tc.want) {
				t.Fatalf("merge = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestURLKey(t *testing.T) {
	same := [][2]string{
		{"https://www.Example.com/a/", "http://example.com/a"},
		{"https://example.com/a#frag", "https://example.com/a"},
	}
	for _, pair := range same {
		if urlKey(pair[0]) != urlKey(pair[1]) {
			t.Errorf("urlKey(%q) != urlKey(%q)", pair[0], pair[1])
		}
	}
	different := [][2]string{
		{"https://example.com/a?id=1", "https://example.com/a?id=2"},
		{"https://example.com/a", "https://example.com/b"},
	}
	for _, pair := range different {
		if urlKey(pair[0]) == urlKey(pair[1]) {
			t.Errorf("urlKey(%q) == urlKey(%q), want distinct pages", pair[0], pair[1])
		}
	}
}
