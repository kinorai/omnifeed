package fetchcache

import (
	"context"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type pageEngine struct{}

func (pageEngine) Name() string        { return "page" }
func (pageEngine) Matches(string) bool { return true }
func (pageEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	return domain.Document{PageContent: "content", Metadata: map[string]string{}}, nil
}

func TestCacheSpans(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	c := New(Config{
		Inner:      engine.New().Fallback(pageEngine{}),
		Backend:    NewMemory(MemoryConfig{MaxBytes: 1 << 20}),
		DefaultTTL: time.Minute,
	})
	for range 2 {
		if _, err := c.Crawl(context.Background(), "https://example.com/", domain.EngineOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, s := range mem.GetSpans() {
		if s.Name != "cache.get" && s.Name != "cache.set" {
			continue
		}
		for _, kv := range s.Attributes {
			if kv.Key == "cache.result" {
				got = append(got, s.Name+"="+kv.Value.AsString())
			}
		}
	}
	want := []string{"cache.get=miss", "cache.set=ok", "cache.get=hit"}
	if len(got) != len(want) {
		t.Fatalf("cache spans = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cache spans = %v, want %v", got, want)
		}
	}
}
