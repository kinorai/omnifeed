package crawl4ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

func mustJSON(v interface{}) []byte { b, _ := json.Marshal(v); return b }

// execJSOK builds a successful /execute_js response whose js_execution_result
// wraps the given string (the value the caller's snippet returned).
func execJSOK(jsReturn string) []byte {
	return mustJSON(map[string]interface{}{
		"success":             true,
		"status_code":         200,
		"js_execution_result": map[string]interface{}{"results": []interface{}{jsReturn}},
	})
}

// openOn returns a session pointed at rawURL, backed by a Browser talking to srv.
func openOn(t *testing.T, srvURL, token, navURL string) *session {
	t.Helper()
	b := New(httpx.New(nil), srvURL+"/crawl", token)
	s, err := b.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Navigate(context.Background(), navURL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	return s.(*session)
}

func TestExecJSEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://c4/crawl":     "https://c4/execute_js",
		"https://c4/crawl/":    "https://c4/execute_js", // splits at the last /crawl, trailing slash dropped
		"https://c4/api/crawl": "https://c4/api/execute_js",
		"https://c4":           "https://c4/execute_js", // no /crawl → append
		"https://c4/":          "https://c4/execute_js",
		// "/crawl" inside the HOSTNAME must not anchor the split — a natural k8s
		// service URL with no path derives the sibling by appending.
		"http://crawl4ai.svc:11235":       "http://crawl4ai.svc:11235/execute_js",
		"http://omnifeed-crawl4ai:11235/": "http://omnifeed-crawl4ai:11235/execute_js",
		"":                                "",
	}
	for in, want := range cases {
		if got := execJSEndpoint(in); got != want {
			t.Errorf("execJSEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEvalSuccess(t *testing.T) {
	const payload = `{"s":200,"b":"hello"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(execJSOK(payload))
	}))
	defer srv.Close()

	s := openOn(t, srv.URL, "", "https://www.reddit.com/r/x/")
	got, err := s.Eval(context.Background(), `return "x";`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got != payload {
		t.Fatalf("Eval = %q, want %q", got, payload)
	}
}

// The crawl4ai backend re-navigates on every Eval: it must POST {url, scripts}
// with url = the recorded Navigate target and scripts = [the caller's js].
func TestEvalRequestShape(t *testing.T) {
	var captured execJSRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_, _ = w.Write(execJSOK(`ok`))
	}))
	defer srv.Close()

	s := openOn(t, srv.URL, "", "https://www.reddit.com/r/news/comments/abc123/t/")
	if _, err := s.Eval(context.Background(), `return location.href;`); err != nil {
		t.Fatal(err)
	}
	if captured.URL != "https://www.reddit.com/r/news/comments/abc123/t/" {
		t.Errorf("Eval must navigate the recorded URL; got %q", captured.URL)
	}
	if len(captured.Scripts) != 1 || captured.Scripts[0] != `return location.href;` {
		t.Errorf("Eval must send exactly the caller script; got %v", captured.Scripts)
	}
}

func TestEvalSendsBearerToken(t *testing.T) {
	for _, tc := range []struct{ name, token, want string }{
		{"token sends bearer header", "sekret", "Bearer sekret"},
		{"empty token sends no header", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				_, _ = w.Write(execJSOK(`ok`))
			}))
			defer srv.Close()

			s := openOn(t, srv.URL, tc.token, "https://www.reddit.com/r/x/")
			if _, err := s.Eval(context.Background(), `return 1;`); err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if gotAuth != tc.want {
				t.Fatalf("Authorization = %q, want %q", gotAuth, tc.want)
			}
		})
	}
}

func TestEvalErrorClassification(t *testing.T) {
	cases := []struct {
		name       string
		httpStatus int
		body       []byte
		wantErr    string
		wantKind   domain.FailureKind
	}{
		// crawl4ai's anti-bot detector hard-errors a blocked nav as a 500; it must
		// classify as bot_block (the OmnifeedRedditBlocked alert keys on it), not
		// upstream_error.
		{"5xx anti-bot", 500, []byte(`{"error":"Blocked by anti-bot protection"}`), "500", domain.KindBotBlock},
		{"4xx passthrough", 400, []byte(`{"error":"bad request"}`), "crawl4ai returned 400", domain.KindError},
		{"result success=false", 200, mustJSON(map[string]interface{}{
			"success": false, "error_message": "nav timeout",
		}), "nav timeout", domain.KindBotBlock},
		{"empty js results", 200, mustJSON(map[string]interface{}{
			"success":             true,
			"js_execution_result": map[string]interface{}{"results": []interface{}{}},
		}), "no js result", domain.KindBotBlock},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.httpStatus)
				_, _ = w.Write(tc.body)
			}))
			defer srv.Close()

			s := openOn(t, srv.URL, "", "https://www.reddit.com/r/x/")
			_, err := s.Eval(context.Background(), `return 1;`)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			var fe *domain.FetchError
			if !errors.As(err, &fe) {
				t.Fatalf("want *domain.FetchError, got %T: %v", err, err)
			}
			if fe.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", fe.Kind, tc.wantKind)
			}
		})
	}
}

// Eval before Navigate is a programming error, not a crawl outcome — it must
// fail fast rather than POST an empty url.
func TestEvalBeforeNavigate(t *testing.T) {
	b := New(httpx.New(nil), "https://c4/crawl", "")
	s, _ := b.Open(context.Background())
	if _, err := s.Eval(context.Background(), `return 1;`); err == nil {
		t.Fatal("expected error when Eval is called before Navigate")
	}
}

// With no crawl4ai URL, Eval fails fast (mirrors the config-level guard).
func TestEvalMissingEndpoint(t *testing.T) {
	b := New(httpx.New(nil), "", "")
	s, _ := b.Open(context.Background())
	_ = s.Navigate(context.Background(), "https://www.reddit.com/r/x/")
	if _, err := s.Eval(context.Background(), `return 1;`); err == nil {
		t.Fatal("expected error when crawl4ai URL is empty")
	}
}
