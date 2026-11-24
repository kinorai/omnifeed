package twitter

import (
	"net/url"
	"regexp"
	"strings"
)

// postHosts are the hosts whose /{user}/status/{id} URLs name an X post (and
// whose /home, /{user} and /search URLs name a timeline):
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
	// profilePath matches /{handle}, with an optional trailing slash. Its
	// tabs (/with_replies, /media, …) are not matched.
	profilePath = regexp.MustCompile(`^/([A-Za-z0-9_]{1,15})/?$`)
	// shortLinkPath matches a t.co slug.
	shortLinkPath = regexp.MustCompile(`^/[A-Za-z0-9]{1,32}/?$`)
)

// reservedPaths are first path segments X uses for its own pages, so a URL
// like x.com/explore never reads as the profile of an account "explore".
var reservedPaths = map[string]bool{
	"about": true, "account": true, "bookmarks": true, "communities": true, "compose": true,
	"download": true, "explore": true, "hashtag": true, "help": true, "home": true, "i": true,
	"intent": true, "jobs": true, "lists": true, "login": true, "logout": true, "messages": true,
	"notifications": true, "oauth": true, "premium": true, "premium_sign_up": true, "privacy": true,
	"rules": true, "search": true, "settings": true, "share": true,
	"signup": true, "topics": true, "tos": true, "who_to_follow": true, "your_twitter_data": true,
}

type targetKind int

const (
	kindPost targetKind = iota
	kindShortLink
	kindHome      // x.com/home: the For you timeline
	kindFollowing // x.com/home?f=following
	kindProfile
	kindSearch
)

// String is the kind as the Timeline output and the kind metadata name it.
func (k targetKind) String() string {
	switch k {
	case kindPost:
		return "post"
	case kindShortLink:
		return "short_link"
	case kindHome:
		return "home"
	case kindFollowing:
		return "following"
	case kindProfile:
		return "profile"
	default:
		return "search"
	}
}

// target is a parsed URL this engine claims: a post id, a t.co link whose
// destination is not known yet, or a timeline.
type target struct {
	kind   targetKind
	id     string // post
	handle string // profile
	query  string // search, verbatim
}

// parseTarget classifies rawURL. ok is false for every URL this engine does
// not render (profile tabs, lists, …), which then fall through.
func parseTarget(rawURL string) (target, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return target{}, false
	}
	host := strings.ToLower(u.Hostname())
	if host == shortLinkHost {
		return target{kind: kindShortLink}, shortLinkPath.MatchString(u.Path)
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "www."), "mobile.")
	if !postHosts[host] {
		return target{}, false
	}
	if m := statusPath.FindStringSubmatch(u.Path); m != nil {
		return target{kind: kindPost, id: m[1]}, true
	}
	q := u.Query()
	switch u.Path {
	case "/home", "/home/":
		if q.Get("f") == "following" {
			return target{kind: kindFollowing}, true
		}
		return target{kind: kindHome}, true
	case "/search", "/search/":
		t := target{kind: kindSearch, query: q.Get("q")}
		return t, strings.TrimSpace(t.query) != ""
	}
	if m := profilePath.FindStringSubmatch(u.Path); m != nil && !reservedPaths[strings.ToLower(m[1])] {
		return target{kind: kindProfile, handle: m[1]}, true
	}
	return target{}, false
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
