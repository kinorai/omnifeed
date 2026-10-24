package observability

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
)

// Panic sites, the values of omnifeed_panics_total's `where` label. A fixed
// set: every recover point in the binary names one of these.
const (
	PanicHTTP           = "http"          // an HTTP handler (any server)
	PanicMCPTool        = "mcp_tool"      // an MCP tool handler (stdio or HTTP)
	PanicLoaderCrawl    = "loader_crawl"  // one URL of a POST /crawl batch
	PanicGitHubFanout   = "github_fanout" // one parallel GitHub API call
	PanicBackground     = "background"    // a background loop (dependency checks, SearXNG init)
	panicWhereUnchecked = "other"         // never passed deliberately; keeps the label bounded
)

var panicSites = []string{PanicHTTP, PanicMCPTool, PanicLoaderCrawl, PanicGitHubFanout, PanicBackground}

// panicsTotal is package-level, not a Metrics field: the recover points live
// in packages (engines, transports) that are not handed a *Metrics, and a
// panic must be countable wherever it happens. Every Metrics registry
// registers this one collector.
var panicsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "omnifeed_panics_total",
	Help: `Panics recovered, by site: http (an HTTP handler), mcp_tool (an MCP tool handler), loader_crawl (one URL of a /crawl batch), github_fanout (a parallel GitHub API call), background (a background loop). Any increase is a bug: the request failed with an error instead of killing the process.`,
}, []string{"where"})

func init() {
	for _, w := range panicSites {
		panicsTotal.WithLabelValues(w)
	}
}

// ObservePanic counts and logs one recovered panic. v is what recover()
// returned. Callers that recover themselves (middleware that must also write a
// 500) use this; everyone else defers Recover.
func ObservePanic(where string, v any) {
	known := slices.Contains(panicSites, where)
	if !known {
		where = panicWhereUnchecked
	}
	panicsTotal.WithLabelValues(where).Inc()
	slog.Default().Error("panic recovered", "where", where, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
}

// Recover is deferred at the top of a goroutine (or a call that must not take
// the process down): it recovers a panic, counts it in omnifeed_panics_total,
// logs it with its stack and, when errp is non-nil, turns it into an error.
//
//	defer observability.Recover(observability.PanicGitHubFanout, &err)
//
// It must be deferred directly — recover() only stops a panic when called by
// the deferred function itself.
func Recover(where string, errp *error) {
	v := recover()
	if v == nil {
		return
	}
	ObservePanic(where, v)
	if errp != nil {
		*errp = fmt.Errorf("internal error: panic in %s: %v", where, v)
	}
}
