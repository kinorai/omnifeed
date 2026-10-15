package reddit

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
)

// directClient calls Reddit's JSON endpoints over plain HTTP, authenticated.
// Reddit closed its .json endpoints to logged-out clients in 2026 ("Logged-in
// and authenticated access won't be impacted") and checks the credential, not
// the TLS fingerprint: Go's own client passes, so no browser is needed. One
// thread is one request with live scores and the full tree, against a page
// load plus many expansion requests on the browser path.
//
// Two credentials work, measured 2026-10-15:
//   - an OAuth client ID, used with the installed-client grant: an app-only
//     token for oauth.reddit.com, no Reddit account, 1000 requests per window;
//   - a logged-in reddit_session cookie for www.reddit.com: tied to an
//     account, 100 requests per window.
type directClient struct {
	http      *http.Client
	userAgent string
	origin    string // where the JSON endpoints are called

	cookie string // reddit_session, cookie mode

	clientID string // installed-client grant, token mode
	deviceID string
	mu       sync.Mutex
	token    string
	expires  time.Time
}

const (
	oauthOrigin = "https://oauth.reddit.com"
	// #nosec G101 -- a public endpoint URL, not a credential
	tokenEndpoint = "https://www.reddit.com/api/v1/access_token"
	// installedClientGrant is Reddit's grant for apps that cannot keep a
	// secret: the client ID alone gets an app-only token.
	installedClientGrant = "https://oauth.reddit.com/grants/installed_client"
	// tokenEarlyRefresh renews a token this long before Reddit expires it,
	// so a crawl never starts on a token about to lapse.
	tokenEarlyRefresh = 5 * time.Minute
	// tokenDefaultLife is the token life assumed when Reddit sends no
	// expires_in: Reddit's documented app-only token life.
	tokenDefaultLife = time.Hour
)

func newCookieClient(h *http.Client, userAgent, cookie string) *directClient {
	return &directClient{http: h, userAgent: userAgent, origin: redditOrigin, cookie: cookie}
}

func newTokenClient(h *http.Client, userAgent, clientID string) *directClient {
	return &directClient{http: h, userAgent: userAgent, origin: oauthOrigin, clientID: clientID, deviceID: randomDeviceID()}
}

// randomDeviceID is the per-process device ID the installed-client grant
// requires: 20 to 30 characters, unique per install.
func randomDeviceID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// maxDirectBody caps a Reddit JSON response. A 300-comment thread is about
// 600 KB; 32 MB leaves room for the largest threads without letting one
// response exhaust memory.
const maxDirectBody = 32 << 20

// accessToken returns a valid app-only token, fetching a new one when the
// cached one is missing or close to expiry. The lock is held during the token
// request on purpose: concurrent calls wait for one token instead of each
// sending its own request. That wait happens once per token life.
func (c *directClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	form := url.Values{"grant_type": {installedClientGrant}, "device_id": {c.deviceID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.clientID, "")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", &domain.FetchError{Kind: domain.KindUpstreamError, Err: fmt.Errorf("reddit token request: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		// A refused client ID is a refused credential: the chain moves on.
		return "", &domain.FetchError{Kind: domain.KindHTTP403, StatusCode: resp.StatusCode,
			Err: fmt.Errorf("reddit refused the installed-client token for OMNIFEED_REDDIT_CLIENT_ID (status %d, error %q)", resp.StatusCode, tok.Error)}
	}
	c.token = tok.AccessToken
	c.expires = time.Now().Add(tokenLife(tok.ExpiresIn))
	return c.token, nil
}

// tokenLife is how long a token is used, from Reddit's expires_in in
// seconds. It renews early, but never earlier than half the token's life, so
// a short or missing expires_in cannot cause a token request on every call.
func tokenLife(expiresIn int) time.Duration {
	life := time.Duration(expiresIn) * time.Second
	if life <= 0 {
		life = tokenDefaultLife
	}
	return life - min(tokenEarlyRefresh, life/2)
}

// dropToken forgets the cached token after Reddit rejected it, so the next
// request fetches a fresh one.
func (c *directClient) dropToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

func (c *directClient) request(ctx context.Context, rawURL string, authenticate bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if authenticate {
		if c.clientID != "" {
			tok, err := c.accessToken(ctx)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		} else {
			// #nosec G124 -- a request cookie: Secure, HttpOnly and SameSite apply to Set-Cookie only
			req.AddCookie(&http.Cookie{Name: "reddit_session", Value: c.cookie})
		}
	}
	return c.http.Do(req)
}

// get returns the response as the envelope the browser snippets produce, so
// both paths share unwrapEnvelope's block and back-off handling.
func (c *directClient) get(ctx context.Context, rawURL string) (fetchEnvelope, error) {
	resp, err := c.request(ctx, rawURL, true)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && c.clientID != "" {
		// The token lapsed early or was revoked: fetch a new one once.
		_ = resp.Body.Close()
		c.dropToken()
		resp, err = c.request(ctx, rawURL, true)
	}
	if err != nil {
		return fetchEnvelope{}, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDirectBody))
	if err != nil {
		return fetchEnvelope{}, transportError(ctx, err)
	}
	return fetchEnvelope{
		S:  resp.StatusCode,
		B:  string(body),
		RA: resp.Header.Get("Retry-After"),
		RS: resp.Header.Get("X-Ratelimit-Reset"),
		RM: resp.Header.Get("X-Ratelimit-Remaining"),
	}, nil
}

// location resolves a share link to the URL it redirects to. The client does
// not follow redirects, so the target comes from the Location header. Share
// links live on www.reddit.com and redirect without credentials, so only the
// cookie mode sends one.
func (c *directClient) location(ctx context.Context, shareURL string) (string, error) {
	resp, err := c.request(ctx, shareURL, c.clientID == "")
	if err != nil {
		return "", transportError(ctx, err)
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		// Classified, so a refused credential (403) moves the chain on.
		return "", &domain.FetchError{Kind: domain.KindForStatus(resp.StatusCode), StatusCode: resp.StatusCode,
			Err: fmt.Errorf("share link answered %d with no redirect", resp.StatusCode)}
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	return resp.Request.URL.ResolveReference(u).String(), nil
}

// transportError classifies a request that got no answer from Reddit (DNS,
// connection reset, the client's own timeout) as upstream_error, so the chain
// tries the next method. The caller's own cancel or deadline stays as it is:
// that call is over, on every method.
func transportError(ctx context.Context, err error) error {
	var fe *domain.FetchError
	if ctx.Err() != nil || errors.As(err, &fe) {
		return err
	}
	return &domain.FetchError{Kind: domain.KindUpstreamError, Err: fmt.Errorf("reddit request: %w", err)}
}

// refusalReason returns why Reddit refused a subreddit to everyone, read from
// a 403 body such as {"reason": "private"}, or "" for any other 403. These
// refusals are about the subreddit, not the credential: every method gets the
// same answer.
func refusalReason(body string) string {
	var r struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(body), &r) != nil {
		return ""
	}
	switch r.Reason {
	case "private", "quarantined", "banned", "gold_only":
		return r.Reason
	}
	return ""
}

// SessionExpiry reads the expiry from a reddit_session cookie. The cookie is a
// signed token whose payload carries `exp`, and Reddit does not renew it on
// use, so this is the date a human must log in again by. The signature is not
// checked: the value only feeds a warning and a metric.
func SessionExpiry(cookie string) (time.Time, bool) {
	parts := strings.Split(cookie, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0), true
}
