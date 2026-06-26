package httpx

import "regexp"

// HostMatcher returns a case-insensitive regexp matching host and all of its
// subdomains: HostMatcher("reddit.com") matches reddit.com, www.reddit.com, and
// old.reddit.com, but not evilreddit.com or reddit.com.evil.com. Engines use it
// to claim URLs; centralizing the pattern keeps this security-sensitive rule
// from drifting between engines.
func HostMatcher(domain string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|\.)` + regexp.QuoteMeta(domain) + `$`)
}
