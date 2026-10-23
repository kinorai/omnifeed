package failover

import (
	"net/url"
	"strings"

	"github.com/kinorai/omnifeed/internal/domain"
)

// merge interleaves two rankings — primary first, then alternating — and drops
// any URL already taken, so neither source buries the other: the backup's top
// hits land in the top positions instead of after the primary's whole list.
// limit <= 0 keeps every result.
func merge(primary, backup []domain.SearchResult, limit int) []domain.SearchResult {
	merged := make([]domain.SearchResult, 0, len(primary)+len(backup))
	seen := make(map[string]struct{}, len(primary)+len(backup))
	take := func(r domain.SearchResult) {
		key := urlKey(r.URL)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		merged = append(merged, r)
	}
	for i := 0; i < max(len(primary), len(backup)); i++ {
		if i < len(primary) {
			take(primary[i])
		}
		if i < len(backup) {
			take(backup[i])
		}
	}
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// urlKey is the identity two searchers' copies of the same page share. Search
// engines disagree on the cosmetic parts — scheme, a leading "www.", a trailing
// slash, a fragment — so those are dropped; the path and query are kept, since
// they are what actually names the page. An unparseable URL is its own key.
func urlKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return raw
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	key := host + strings.TrimSuffix(u.EscapedPath(), "/")
	if u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return key
}
