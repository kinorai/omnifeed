package engine

import (
	"context"
	"testing"

	"github.com/kinorai/crawl4ai-reddit-proxy/internal/domain"
)

// stubEngine is a no-op fallback used to prove the choke point runs BEFORE
// dispatch: if validation rejects a URL, the engine must never be called.
type stubEngine struct{ called bool }

func (*stubEngine) Name() string        { return "stub" }
func (*stubEngine) Matches(string) bool { return false }
func (s *stubEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	s.called = true
	return domain.Document{PageContent: "ok"}, nil
}

// With BlockPrivateIPs enabled, Crawl must reject SSRF targets at the choke
// point — before any engine runs — so every transport (loader, MCP HTTP,
// stdio) is covered, not just the loader. Regression guard for the MCP path,
// which previously called Crawl with no validation at all. Literal/numeric
// hosts resolve without DNS, so this is hermetic.
func TestRegistryCrawl_ChokePointRejectsSSRF(t *testing.T) {
	for _, rawURL := range []string{
		"http://127.0.0.1/",       // loopback
		"http://169.254.169.254/", // link-local (cloud metadata)
		"http://10.0.0.1/",        // RFC1918
		"http://0177.0.0.1/",      // octal-obfuscated loopback
		"file:///etc/passwd",      // non-http scheme
	} {
		stub := &stubEngine{}
		r := New().Fallback(stub).BlockPrivateIPs(true)
		if _, err := r.Crawl(context.Background(), rawURL, domain.EngineOptions{}); err == nil {
			t.Errorf("Crawl(%q) = nil error, want rejected", rawURL)
		}
		if stub.called {
			t.Errorf("Crawl(%q) dispatched to the engine despite an invalid URL", rawURL)
		}
	}
}

// A public URL passes the choke point and reaches the engine.
func TestRegistryCrawl_AllowsPublic(t *testing.T) {
	stub := &stubEngine{}
	r := New().Fallback(stub).BlockPrivateIPs(true)
	if _, err := r.Crawl(context.Background(), "http://8.8.8.8/", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl(public) = %v, want nil", err)
	}
	if !stub.called {
		t.Error("Crawl(public) did not reach the engine")
	}
}
