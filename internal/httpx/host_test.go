package httpx

import "testing"

func TestHostMatcher(t *testing.T) {
	m := HostMatcher("reddit.com")
	for _, h := range []string{"reddit.com", "www.reddit.com", "old.reddit.com"} {
		if !m.MatchString(h) {
			t.Errorf("HostMatcher(reddit.com).MatchString(%q) = false, want true", h)
		}
	}
	// Must not match look-alikes — this is the security-sensitive part.
	for _, h := range []string{"evilreddit.com", "reddit.com.evil.com", "notreddit.com", "reddit.comx"} {
		if m.MatchString(h) {
			t.Errorf("HostMatcher(reddit.com).MatchString(%q) = true, want false", h)
		}
	}
}
