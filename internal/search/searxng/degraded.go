package searxng

// degradation names the first critical engine that did not contribute to a
// search, and why, in the "engine: reason" shape a log line or a metric can
// carry. It returns "" when every critical engine answered with rows.
//
// An engine is missing in one of two ways. It failed openly: SearXNG lists it
// in unresponsive_engines, and the reason is that report's normalized class
// (too_many_requests, captcha, ...). Or it failed silently: it was asked and
// contributed no rows, which on this pool is how a blocked engine answers
// (HTTP 200, empty result set). The second case cannot tell a block from an
// honest zero-hit query for that engine — but a critical engine is one the
// operator relies on for coverage, so its absence is worth topping up either
// way.
func degradation(critical []string, rows map[string]int, unresponsive [][]string) string {
	if len(critical) == 0 {
		return ""
	}
	failed := make(map[string]string, len(unresponsive))
	for _, pair := range unresponsive {
		if len(pair) == 0 {
			continue
		}
		msg := ""
		if len(pair) > 1 {
			msg = pair[1]
		}
		failed[pair[0]] = normalizeEngineError(msg)
	}
	for _, engine := range critical {
		if class, ok := failed[engine]; ok {
			return engine + ": " + class
		}
		if rows[engine] == 0 {
			return engine + ": no results"
		}
	}
	return ""
}

// setOf returns the members of names as a set; nil for an empty list.
func setOf(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}
