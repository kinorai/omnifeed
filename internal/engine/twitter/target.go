package twitter

import (
	"net/url"
	"regexp"
	"strings"
)

// postHosts are the hosts whose /{user}/status/{id} URLs name an X post:
// x.com and twitter.com, and the embed-fixer mirrors people paste instead
// (fxtwitter/fixupx, vxtwitter/fixvx), each optionally under www. or mobile.
var postHosts = map[string]bool{
	"x.com": true, "twitter.com": true,
	"fxtwitter.com": true, "fixupx.com": true,
	"vxtwitter.com": true, "fixvx.com": true,
}

// shortLinkHost is X's link shortener; its targets are resolved with a HEAD.
const shortLinkHost = "t.co"

var (
	// statusPath matches /{user}/status/{id}, /i/status/{id} and
	// /i/web/status/{id}, with an optional /photo/N or /video/N suffix and
	// an optional trailing slash. "statuses" is the legacy spelling.
	statusPath = regexp.MustCompile(`^/(?:[A-Za-z0-9_]{1,50}|i|i/web)/status(?:es)?/([0-9]{1,20})(?:/(?:photo|video)/[0-9]{1,2})?/?$`)
	// shortLinkPath matches a t.co slug.
	shortLinkPath = regexp.MustCompile(`^/[A-Za-z0-9]{1,32}/?$`)
)

// target is a parsed URL this engine claims: a post id, or a t.co link whose
// destination is not known yet.
type target struct {
	id        string
	shortLink bool
}

// parseTarget classifies rawURL. ok is false for every URL this engine does
// not render (profiles, search, lists, …), which then fall through.
func parseTarget(rawURL string) (target, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return target{}, false
	}
	host := strings.ToLower(u.Hostname())
	if host == shortLinkHost {
		return target{shortLink: true}, shortLinkPath.MatchString(u.Path)
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "www."), "mobile.")
	if !postHosts[host] {
		return target{}, false
	}
	m := statusPath.FindStringSubmatch(u.Path)
	if m == nil {
		return target{}, false
	}
	return target{id: m[1]}, true
}

// canonicalURL is the x.com permalink for a post id. /i/status/{id} resolves
// to the right author on x.com, so the handle is not needed.
func canonicalURL(id string) string { return "https://x.com/i/status/" + id }

// postURL is the x.com permalink for a post whose author is known.
func postURL(handle, id string) string {
	if handle == "" {
		return canonicalURL(id)
	}
	return "https://x.com/" + handle + "/status/" + id
}
