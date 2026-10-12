package crawl4ai

import (
	"context"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Each step of the retry ladder is its own crawl4ai.rung span, numbered.
func TestCrawlRungSpans(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	e, _ := fakeCrawl(t, func(n int32, _ map[string]any) map[string]any {
		if n == 1 {
			return map[string]any{"success": false, "error": "Page.content: Unable to retrieve content because the page is navigating and changing the content."}
		}
		return page(200, "# Title\n"+realProse)
	})
	if _, err := e.Crawl(context.Background(), deadTargetURL+"/x", domain.EngineOptions{}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range mem.GetSpans() {
		if s.Name != "crawl4ai.rung" {
			continue
		}
		var rung string
		var attempt int64
		for _, kv := range s.Attributes {
			switch kv.Key {
			case "rung":
				rung = kv.Value.AsString()
			case "attempt":
				attempt = kv.Value.AsInt64()
			}
		}
		got = append(got, rung+"#"+string(rune('0'+attempt)))
	}
	if len(got) != 2 || got[0] != "base#1" || got[1] != "networkidle#2" {
		t.Fatalf("rung spans = %v, want [base#1 networkidle#2]", got)
	}
}
