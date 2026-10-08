package engine

import (
	"context"
	"errors"
	"strings"
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

// failingEngine claims every URL and always errors with kind (upstream_error
// when unset — a transient fault the fallback may cover).
type failingEngine struct {
	calls int
	kind  domain.FailureKind
}

func (*failingEngine) Name() string        { return "failing" }
func (*failingEngine) Matches(string) bool { return true }
func (f *failingEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	f.calls++
	kind := f.kind
	if kind == "" {
		kind = domain.KindUpstreamError
	}
	return domain.Document{}, &domain.FetchError{Kind: kind}
}

// A dedicated engine failing on a transient fault must fall back to the
// generic engine instead of hard-failing a URL the fallback can still render —
// unless the caller's context is already dead.
func TestRegistryCrawl_FallsBackOnEngineError(t *testing.T) {
	failing, fallback := &failingEngine{}, &stubEngine{}
	r := New().Register(failing).Fallback(fallback)

	doc, err := r.Crawl(context.Background(), "http://8.8.8.8/", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl = %v, want the fallback's success", err)
	}
	if !strings.HasSuffix(doc.PageContent, "ok") || failing.calls != 1 || !fallback.called {
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
	if err := m.EngineFallbacks.WithLabelValues("failing", string(domain.KindUpstreamError)).Write(&dm); err != nil {
		t.Fatal(err)
	}
	if got := dm.GetCounter().GetValue(); got != 1 {
		t.Fatalf(`engine_fallbacks{from_engine="failing",reason="upstream_error"} = %v, want 1`, got)
	}

	// The direct-fallback path (no engine matched) is not a handoff — no count.
	stub := &stubEngine{}
	r2 := New().Fallback(stub).Metrics(m)
	if _, err := r2.Crawl(context.Background(), "http://8.8.8.8/", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl = %v, want success", err)
	}
	if err := m.EngineFallbacks.WithLabelValues("failing", string(domain.KindUpstreamError)).Write(&dm); err != nil {
		t.Fatal(err)
	}
	if got := dm.GetCounter().GetValue(); got != 1 {
		t.Fatalf("counter moved on the no-match path: %v, want still 1", got)
	}
}

// A caller that explicitly asked for a structured format parses the reply, so
// a dedicated engine failing — even transiently — must surface its own error,
// never the fallback's markdown. Regression guard for the 2026-10-08 incident:
// a format=json Reddit request got "[ Skip to main content ](…)" back.
func TestRegistryCrawl_NoFallbackOnExplicitFormat(t *testing.T) {
	for _, kind := range []domain.FailureKind{domain.KindTimeout, domain.KindUpstreamError, domain.KindHTTP429} {
		t.Run(string(kind), func(t *testing.T) {
			failing, fallback := &failingEngine{kind: kind}, &stubEngine{}
			r := New().Register(failing).Fallback(fallback)
			_, err := r.Crawl(context.Background(), "http://8.8.8.8/",
				domain.EngineOptions{RedditFormat: "json", FormatExplicit: true})
			var fe *domain.FetchError
			if !errors.As(err, &fe) || fe.Kind != kind {
				t.Fatalf("Crawl err = %v, want the engine's %s error", err, kind)
			}
			if fallback.called {
				t.Fatal("fallback ran despite an explicit structured format")
			}
		})
	}
}

// Block and rate verdicts (and omnifeed's own spent quota) never fall back,
// even for an AI-agent caller with no explicit format: the browser render
// would hit the host that just refused us and prolong the block.
func TestRegistryCrawl_NoFallbackOnBlockKinds(t *testing.T) {
	m := observability.NewMetrics()
	for _, kind := range []domain.FailureKind{
		domain.KindHTTP429, domain.KindHTTP403, domain.KindCaptcha, domain.KindBotBlock, domain.KindQuotaExhausted,
	} {
		t.Run(string(kind), func(t *testing.T) {
			failing, fallback := &failingEngine{kind: kind}, &stubEngine{}
			r := New().Register(failing).Fallback(fallback).Metrics(m)
			_, err := r.Crawl(context.Background(), "http://8.8.8.8/", domain.EngineOptions{})
			var fe *domain.FetchError
			if !errors.As(err, &fe) || fe.Kind != kind {
				t.Fatalf("Crawl err = %v, want the engine's %s error", err, kind)
			}
			if fallback.called {
				t.Fatalf("fallback ran on a %s failure", kind)
			}
			var dm dto.Metric
			if err := m.EngineFallbacks.WithLabelValues("failing", string(kind)).Write(&dm); err != nil {
				t.Fatal(err)
			}
			if got := dm.GetCounter().GetValue(); got != 0 {
				t.Fatalf("engine_fallbacks counted a refused handoff: %v", got)
			}
		})
	}
}

// A transient failure with no explicit format still falls back for an AI
// agent, and the result says so: _meta fallback_from/fallback_reason plus one
// notice line on top of the body.
func TestRegistryCrawl_FallbackOnTimeoutIsMarked(t *testing.T) {
	failing := &failingEngine{kind: domain.KindTimeout}
	fallback := &metaStubEngine{meta: map[string]string{"source": "http://8.8.8.8/"}}
	r := New().Register(failing).Fallback(fallback)

	doc, err := r.Crawl(context.Background(), "http://8.8.8.8/", domain.EngineOptions{RedditFormat: "toon"})
	if err != nil {
		t.Fatalf("Crawl = %v, want the fallback's success", err)
	}
	if got := doc.Metadata["fallback_from"]; got != "failing" {
		t.Errorf("fallback_from = %q, want failing", got)
	}
	if got := doc.Metadata["fallback_reason"]; got != "timeout" {
		t.Errorf("fallback_reason = %q, want timeout", got)
	}
	if doc.Metadata["source"] != "http://8.8.8.8/" {
		t.Errorf("fallback metadata lost: %v", doc.Metadata)
	}
	if _, mutated := fallback.meta["fallback_from"]; mutated {
		t.Error("markFallback mutated the fallback engine's metadata map")
	}
	first, rest, _ := strings.Cut(doc.PageContent, "\n")
	want := "> Note: the dedicated failing engine failed (timeout); this is the generic page render instead."
	if first != want {
		t.Errorf("first line = %q, want %q", first, want)
	}
	if !strings.HasSuffix(rest, "page body") {
		t.Errorf("body lost after the notice: %q", doc.PageContent)
	}
}

// metaStubEngine is a fallback that returns a body and a metadata map, so the
// marking test can check both survive.
type metaStubEngine struct{ meta map[string]string }

func (*metaStubEngine) Name() string        { return "metastub" }
func (*metaStubEngine) Matches(string) bool { return false }
func (s *metaStubEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	return domain.Document{PageContent: "page body", Metadata: s.meta}, nil
}
