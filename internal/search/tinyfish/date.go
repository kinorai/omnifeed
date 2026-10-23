package tinyfish

import (
	"strconv"
	"strings"
	"time"
)

// absoluteLayouts are the absolute date shapes TinyFish returns ("Sep 25,
// 2026"), plus ISO dates in case a source passes one through.
var absoluteLayouts = []string{"Jan 2, 2006", "January 2, 2006", time.RFC3339, "2006-01-02"}

// relativeUnits maps the unit word of a relative date ("5 hours ago") to its
// length. Months and years are approximate, which is all a relative date
// carries anyway.
var relativeUnits = map[string]time.Duration{
	"minute": time.Minute,
	"hour":   time.Hour,
	"day":    24 * time.Hour,
	"week":   7 * 24 * time.Hour,
	"month":  30 * 24 * time.Hour,
	"year":   365 * 24 * time.Hour,
}

// parseDate turns a TinyFish result date into RFC 3339, the shape
// published_date carries for every searcher. It returns "" for an empty or
// unrecognised date: a wrong date is worse for a caller ranking by recency
// than no date.
func parseDate(raw string, now time.Time) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, layout := range absoluteLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	if ago, ok := parseAgo(raw); ok {
		return now.Add(-ago).UTC().Truncate(time.Minute).Format(time.RFC3339)
	}
	return ""
}

// parseAgo reads "<n> <unit>[s] ago".
func parseAgo(raw string) (time.Duration, bool) {
	fields := strings.Fields(strings.ToLower(raw))
	if len(fields) != 3 || fields[2] != "ago" {
		return 0, false
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 0 {
		return 0, false
	}
	unit, ok := relativeUnits[strings.TrimSuffix(fields[1], "s")]
	if !ok {
		return 0, false
	}
	return time.Duration(n) * unit, true
}
