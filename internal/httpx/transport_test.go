package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// NewTransport must widen the per-host idle pool past DefaultTransport's 2
// while inheriting the rest of its behavior (proxy-from-env, HTTP/2) — a
// hand-rolled &http.Transport{} would silently lose both.
func TestNewTransportWidensIdlePool(t *testing.T) {
	tr := NewTransport()
	def := http.DefaultTransport.(*http.Transport)

	if tr.MaxIdleConnsPerHost <= def.MaxIdleConnsPerHost && tr.MaxIdleConnsPerHost <= 2 {
		t.Errorf("MaxIdleConnsPerHost = %d, want > 2", tr.MaxIdleConnsPerHost)
	}
	if tr.Proxy == nil {
		t.Error("Proxy = nil, want DefaultTransport's ProxyFromEnvironment inherited")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want DefaultTransport's true inherited")
	}
	if tr == def {
		t.Error("NewTransport returned DefaultTransport itself; mutating it would leak into every client")
	}
}

// The guarded client must refuse dials whose RESOLVED address is
// private/reserved (dial-time, so DNS rebinding can't race it), and allow them
// when the guard is off.
func TestNewGuardedClientBlocksReservedDials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close() // listens on 127.0.0.1 — exactly what the guard must refuse

	guarded := NewGuardedClient(true, 5*time.Second)
	resp, err := guarded.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("guarded client reached a loopback server, want dial refused")
	}
	if !strings.Contains(err.Error(), "private/reserved") {
		t.Fatalf("error = %v, want the reserved-address refusal", err)
	}

	open := NewGuardedClient(false, 5*time.Second)
	resp, err = open.Get(srv.URL)
	if err != nil {
		t.Fatalf("unguarded client: %v", err)
	}
	_ = resp.Body.Close()
}
