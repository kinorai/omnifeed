package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// OriginGuard is the DNS-rebinding gate for every HTTP transport: no Origin
// (native clients) always passes, loopback passes by IP semantics rather
// than a literal list, the operator allowlist passes case-insensitively,
// and everything else is 403 before the request reaches a handler.
func TestOriginGuard(t *testing.T) {
	cases := []struct {
		name    string
		origin  string // empty → no Origin header
		allowed []string
		want    int
	}{
		{"no origin passes", "", nil, http.StatusOK},
		{"localhost passes", "http://localhost:6274", nil, http.StatusOK},
		{"uppercase localhost passes", "http://LOCALHOST:6274", nil, http.StatusOK},
		{"loopback ip passes", "http://127.0.0.1:8081", nil, http.StatusOK},
		{"whole loopback /8 passes", "http://127.0.0.2:6274", nil, http.StatusOK},
		{"ipv6 loopback passes", "http://[::1]:8081", nil, http.StatusOK},
		{"external origin rejected", "https://evil.example.com", nil, http.StatusForbidden},
		{"lan origin rejected by default", "http://192.168.1.20:6274", nil, http.StatusForbidden},
		{"null origin rejected", "null", nil, http.StatusForbidden},
		{"allowlisted origin passes", "https://app.example.com", []string{"https://app.example.com"}, http.StatusOK},
		{"allowlist match is case-insensitive", "https://APP.example.com", []string{"https://app.example.com"}, http.StatusOK},
		{"allowlisted lan origin passes", "http://192.168.1.20:6274", []string{"http://192.168.1.20:6274"}, http.StatusOK},
		{"allowlist does not open other origins", "https://other.example.com", []string{"https://app.example.com"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := OriginGuard(tc.allowed)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status: got %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
