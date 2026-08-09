package engine

import (
	"context"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
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

// failingEngine claims every URL and always errors.
type failingEngine struct{ calls int }

func (*failingEngine) Name() string        { return "failing" }
func (*failingEngine) Matches(string) bool { return true }
func (f *failingEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	f.calls++
	return domain.Document{}, &domain.FetchError{Kind: domain.KindHTTP403}
}

// A dedicated engine failing must fall back to the generic engine instead of
// hard-failing a URL the fallback can still render (e.g. anonymous GitHub
// quota exhaustion) — unless the caller's context is already dead.
func TestRegistryCrawl_FallsBackOnEngineError(t *testing.T) {
	failing, fallback := &failingEngine{}, &stubEngine{}
	r := New().Register(failing).Fallback(fallback)

	doc, err := r.Crawl(context.Background(), "http://8.8.8.8/", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl = %v, want the fallback's success", err)
	}
	if doc.PageContent != "ok" || failing.calls != 1 || !fallback.called {
		t.Fatalf("want engine tried once then fallback used; got calls=%d fallback=%v", failing.calls, fallback.called)
	}

	// Dead context: the error comes back as-is, the fallback is not burned.
	failing2, fallback2 := &failingEngine{}, &stubEngine{}
	r2 := New().Register(failing2).Fallback(fallback2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r2.Crawl(ctx, "http://8.8.8.8/", domain.EngineOptions{}); err == nil {
		t.Fatal("Crawl with dead ctx = nil error, want the engine error")
	}
	if fallback2.called {
		t.Fatal("fallback ran despite a dead context")
	}
}

// The engine→fallback handoff must be counted with the failing engine's name
// and the classified failure reason (omnifeed_engine_fallbacks_total).
func TestRegistryCrawl_CountsFallbacks(t *testing.T) {
	m := observability.NewMetrics()
	failing, fallback := &failingEngine{}, &stubEngine{}
	r := New().Register(failing).Fallback(fallback).Metrics(m)

	if _, err := r.Crawl(context.Background(), "http://8.8.8.8/", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl = %v, want the fallback's success", err)
	}

	var dm dto.Metric
	if err := m.EngineFallbacks.WithLabelValues("failing", string(domain.KindHTTP403)).Write(&dm); err != nil {
		t.Fatal(err)
	}
	if got := dm.GetCounter().GetValue(); got != 1 {
		t.Fatalf(`engine_fallbacks{from_engine="failing",reason="http_403"} = %v, want 1`, got)
	}

	// The direct-fallback path (no engine matched) is not a handoff — no count.
	stub := &stubEngine{}
	r2 := New().Fallback(stub).Metrics(m)
	if _, err := r2.Crawl(context.Background(), "http://8.8.8.8/", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl = %v, want success", err)
	}
	if err := m.EngineFallbacks.WithLabelValues("failing", string(domain.KindHTTP403)).Write(&dm); err != nil {
		t.Fatal(err)
	}
	if got := dm.GetCounter().GetValue(); got != 1 {
		t.Fatalf("counter moved on the no-match path: %v, want still 1", got)
	}
}
