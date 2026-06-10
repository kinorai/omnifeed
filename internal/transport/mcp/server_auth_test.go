package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinorai/search-crawl-reddit-proxy/internal/auth"
)

// HTTP transport must reject requests without a valid bearer token when a
// SharedBearer authenticator is configured.
func TestHTTPTransport_RejectsMissingBearer(t *testing.T) {
	srv := New(Config{Authenticator: auth.NewSharedBearer("s3cret")})
	mux := http.NewServeMux()
	srv.Register(mux)

	for _, path := range []string{"/mcp", "/mcp/sse"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status: got %d, want 401", rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
				t.Fatalf("WWW-Authenticate: got %q, want Bearer challenge", got)
			}
		})
	}
}

func TestHTTPTransport_RejectsWrongBearer(t *testing.T) {
	srv := New(Config{Authenticator: auth.NewSharedBearer("s3cret")})
	mux := http.NewServeMux()
	srv.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

func TestHTTPTransport_AcceptsCorrectBearer(t *testing.T) {
	srv := New(Config{Authenticator: auth.NewSharedBearer("s3cret")})
	mux := http.NewServeMux()
	srv.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
}

// A nil Authenticator must default to AlwaysAllow so callers that omit auth
// (e.g. tests, stdio-only deployments) keep working.
func TestHTTPTransport_NilAuthenticatorDefaultsToAlwaysAllow(t *testing.T) {
	srv := New(Config{})
	mux := http.NewServeMux()
	srv.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
}
