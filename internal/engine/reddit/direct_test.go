package reddit

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roundTrip fakes Reddit for the direct client: it records each request and
// answers with the handler's status, headers and body.
type roundTrip func(*http.Request) *http.Response

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }

func reply(r *http.Request, status int, body string, hdr map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

// cookieFetcher is a fetcher whose only method is the session cookie, sent to
// a fake Reddit.
func cookieFetcher(rt roundTrip) *Fetcher {
	return NewFetcher(FetcherConfig{SessionCookie: "sess-123", UserAgent: "omnifeed/test",
		HTTP: &http.Client{Transport: rt}})
}

func directSession(t *testing.T, rt roundTrip, penalize func(string, time.Duration)) *Session {
	t.Helper()
	f := NewFetcher(FetcherConfig{
		SessionCookie: "sess-123",
		UserAgent:     "omnifeed/test",
		HTTP: &http.Client{Transport: rt,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	if got := fmt.Sprint(f.Methods()); got != "[session]" {
		t.Fatalf("methods = %s, a session cookie must enable the session method", got)
	}
	s := openFirst(t, f)
	if penalize != nil {
		s.penalize = penalize
	}
	return s
}

func TestDirectFetchThread(t *testing.T) {
	var got *http.Request
	s := directSession(t, func(r *http.Request) *http.Response {
		got = r
		return reply(r, 200, validListing, nil)
	}, nil)
	body, err := s.FetchThread(context.Background(), "/r/news/comments/abc123/some_title/", 500, 20, "top")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != validListing {
		t.Fatalf("body = %q", body)
	}
	if want := "https://www.reddit.com/r/news/comments/abc123/some_title.json?limit=500&depth=20&sort=top&raw_json=1"; got.URL.String() != want {
		t.Errorf("url = %s, want %s", got.URL, want)
	}
	if c, err := got.Cookie("reddit_session"); err != nil || c.Value != "sess-123" {
		t.Errorf("reddit_session cookie not sent: %v %v", c, err)
	}
	if ua := got.Header.Get("User-Agent"); ua != "omnifeed/test" {
		t.Errorf("User-Agent = %q", ua)
	}
}

// A refused session is a configuration problem, not the content: the error
// must say so, so the operator refreshes the cookie.
func TestDirectForbiddenNamesTheSession(t *testing.T) {
	directRetryDelay = time.Millisecond
	s := directSession(t, func(r *http.Request) *http.Response {
		return reply(r, 403, "<html>blocked by network security</html>", nil)
	}, nil)
	_, err := s.FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top")
	if err == nil || !strings.Contains(err.Error(), "OMNIFEED_REDDIT_SESSION") {
		t.Fatalf("err = %v, want a hint to refresh OMNIFEED_REDDIT_SESSION", err)
	}
}

// One transient 403 between successes is retried, not reported.
func TestDirectRetriesOneForbidden(t *testing.T) {
	directRetryDelay = time.Millisecond
	calls := 0
	s := directSession(t, func(r *http.Request) *http.Response {
		calls++
		if calls == 1 {
			return reply(r, 403, "<html>blocked</html>", nil)
		}
		return reply(r, 200, validListing, nil)
	}, nil)
	if _, err := s.FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top"); err != nil {
		t.Fatalf("a single 403 must be retried: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

// Reddit's spent budget on a direct response reaches the limiter, as it does
// on the browser path.
func TestDirectRateLimitPenalizes(t *testing.T) {
	var held time.Duration
	s := directSession(t, func(r *http.Request) *http.Response {
		return reply(r, 200, validListing, map[string]string{"X-Ratelimit-Remaining": "0.0", "X-Ratelimit-Reset": "42"})
	}, func(_ string, d time.Duration) { held = d })
	if _, err := s.FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top"); err != nil {
		t.Fatal(err)
	}
	if held != 42*time.Second {
		t.Errorf("penalty = %s, want 42s", held)
	}
}

func TestDirectMoreChildrenAndShare(t *testing.T) {
	var urls []string
	s := directSession(t, func(r *http.Request) *http.Response {
		urls = append(urls, r.URL.String())
		if strings.Contains(r.URL.Path, "/s/") {
			return reply(r, 307, "", map[string]string{"Location": "/r/news/comments/abc123/t/?share_id=x"})
		}
		return reply(r, 200, `{"json":{"data":{"things":[]}}}`, nil)
	}, nil)
	// No FetchThread first: the direct path needs no recorded page.
	if _, err := s.FetchMoreChildren(context.Background(), "t3_abc123", []string{"x1", "x2"}, "top"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(urls[0], "https://www.reddit.com/api/morechildren.json?") || !strings.Contains(urls[0], "children=x1%2Cx2") {
		t.Errorf("morechildren url = %s", urls[0])
	}
	got, err := s.ResolveShareURL(context.Background(), "https://www.reddit.com/r/news/s/AbC123")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://www.reddit.com/r/news/comments/abc123/t/?share_id=x" {
		t.Errorf("resolved = %s", got)
	}
}

// Token mode: the client ID gets an installed-client token once, every
// request carries it to oauth.reddit.com, and a 401 renews it once.
func TestDirectTokenMode(t *testing.T) {
	tokens, calls := 0, []string{}
	rt := roundTrip(func(r *http.Request) *http.Response {
		if r.URL.Host == "www.reddit.com" && r.URL.Path == "/api/v1/access_token" {
			tokens++
			if id, _, _ := r.BasicAuth(); id != "app-id" {
				t.Errorf("token request client id = %q", id)
			}
			_ = r.ParseForm()
			if r.PostForm.Get("grant_type") != installedClientGrant || len(r.PostForm.Get("device_id")) < 20 {
				t.Errorf("token form = %v", r.PostForm)
			}
			return reply(r, 200, fmt.Sprintf(`{"access_token":"tok%d","expires_in":86400}`, tokens), nil)
		}
		calls = append(calls, r.URL.Host+" "+r.Header.Get("Authorization"))
		if len(calls) == 2 { // the second API call sees a revoked token
			return reply(r, 401, `{"message":"Unauthorized"}`, nil)
		}
		if _, err := r.Cookie("reddit_session"); err == nil {
			t.Error("token mode must not send the session cookie")
		}
		return reply(r, 200, validListing, nil)
	})
	f := NewFetcher(FetcherConfig{ClientID: "app-id", SessionCookie: "ignored", UserAgent: "omnifeed/test",
		HTTP: &http.Client{Transport: rt}})
	s := openFirst(t, f)
	for range 2 {
		if _, err := s.FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top"); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"oauth.reddit.com Bearer tok1", "oauth.reddit.com Bearer tok1", "oauth.reddit.com Bearer tok2"}
	if fmt.Sprint(calls) != fmt.Sprint(want) || tokens != 2 {
		t.Errorf("calls = %v (tokens %d), want %v (tokens 2)", calls, tokens, want)
	}
}

func TestSessionExpiry(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"t2_x","exp":1807401600}`))
	got, ok := SessionExpiry("eyJhbGciOiJSUzI1NiJ9." + payload + ".sig")
	if !ok || got.Unix() != 1807401600 {
		t.Errorf("expiry = %v %v", got, ok)
	}
	if _, ok := SessionExpiry("not-a-token"); ok {
		t.Error("a non-token value must not parse")
	}
}

// A share link refused without a redirect is a classified failure, so the
// chain can move on to the next method.
func TestDirectShareRefusedIsClassified(t *testing.T) {
	s := directSession(t, func(r *http.Request) *http.Response {
		return reply(r, 403, "<html>blocked</html>", nil)
	}, nil)
	_, err := s.ResolveShareURL(context.Background(), "https://www.reddit.com/r/news/s/AbC123")
	if !failover(err) {
		t.Fatalf("err = %v, want a failover error", err)
	}
}

// A short or missing expires_in still gives a usable token life.
func TestTokenLife(t *testing.T) {
	for _, tc := range []struct {
		expiresIn int
		want      time.Duration
	}{
		{86400, 24*time.Hour - 5*time.Minute},
		{300, 150 * time.Second},
		{60, 30 * time.Second},
		{0, 55 * time.Minute},
		{-1, 55 * time.Minute},
	} {
		if got := tokenLife(tc.expiresIn); got != tc.want {
			t.Errorf("tokenLife(%d) = %v, want %v", tc.expiresIn, got, tc.want)
		}
	}
}

// Each direct answer reports Reddit's remaining budget for its method, so an
// operator sees a shared account's budget run down before a 429.
func TestDirectReportsBudget(t *testing.T) {
	var got []string
	f := NewFetcher(FetcherConfig{SessionCookie: "sess-123", UserAgent: "omnifeed/test",
		HTTP: &http.Client{Transport: roundTrip(func(r *http.Request) *http.Response {
			return reply(r, 200, validListing, map[string]string{"X-Ratelimit-Remaining": "57.0"})
		})},
		OnBudget: func(m string, left float64) { got = append(got, fmt.Sprintf("%s:%g", m, left)) },
	})
	if _, err := openFirst(t, f).FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[session:57]" {
		t.Errorf("budget = %v, want [session:57]", got)
	}
}
