package openwebui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kinorai/omnifeed/internal/auth"
)

// ?no_cache=true|1 is the loader's equivalent of fetch_url's no_cache.
func TestBuildEngineOptionsNoCache(t *testing.T) {
	srv := newTestServer(fakeEngine{}, auth.AlwaysAllow{}, 30)
	for query, want := range map[string]bool{
		"":              false,
		"no_cache=true": true,
		"no_cache=1":    true,
		"no_cache=yes":  false,
		"no_cache=0":    false,
	} {
		req := httptest.NewRequest(http.MethodPost, "/crawl?"+query, nil)
		if got := srv.buildEngineOptions(req).NoCache; got != want {
			t.Errorf("query %q: NoCache = %v, want %v", query, got, want)
		}
	}
}
