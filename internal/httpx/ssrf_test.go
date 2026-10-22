package httpx

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

// stubLookup replaces the resolver for one test, so hostname cases stay
// hermetic. Unlisted names fail like NXDOMAIN.
func stubLookup(t *testing.T, answers map[string][]string) {
	t.Helper()
	orig := lookupNetIP
	t.Cleanup(func() { lookupNetIP = orig })
	lookupNetIP = func(_ context.Context, _, host string) ([]netip.Addr, error) {
		ips, ok := answers[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		out := make([]netip.Addr, len(ips))
		for i, s := range ips {
			out[i] = netip.MustParseAddr(s)
		}
		return out, nil
	}
}

// TestValidateURL locks the SSRF guard: every IANA range that is not globally
// reachable, the numeric host forms a browser reads as IPv4, localhost names,
// and a failed lookup are all refused.
func TestValidateURL(t *testing.T) {
	stubLookup(t, map[string][]string{
		"public.example":   {"93.184.215.14", "2606:2800:21f:cb07:6820:80da:af6b:8b2c"},
		"mapped.example":   {"::ffff:93.184.215.14"}, // a resolver's IPv4 answer in 4-in-6 form
		"internal.example": {"93.184.215.14", "10.0.0.5"},
		"cgnat.example":    {"100.64.1.1"},
		"empty.example":    {},
	})
	for _, tc := range []struct {
		url  string
		want bool // true = allowed
	}{
		// IPv4 special-purpose ranges
		{"http://127.0.0.1/", false},
		{"http://10.0.0.1/", false},
		{"http://172.16.0.1/", false},
		{"http://192.168.1.1/", false},
		{"http://169.254.169.254/", false},
		{"http://0.0.0.0/", false},
		{"http://100.64.0.1/", false},
		{"http://100.127.255.254/", false},
		{"http://192.0.0.170/", false},
		{"http://192.0.2.1/", false},
		{"http://198.18.0.1/", false},
		{"http://198.19.255.255/", false},
		{"http://198.51.100.1/", false},
		{"http://203.0.113.1/", false},
		{"http://224.0.0.1/", false},
		{"http://239.255.255.250/", false},
		{"http://240.0.0.1/", false},
		{"http://255.255.255.255/", false},
		// IPv6 special-purpose ranges
		{"http://[::1]/", false},
		{"http://[::]/", false},
		{"http://[::ffff:127.0.0.1]/", false},
		{"http://[::ffff:8.8.8.8]/", false},
		{"http://[64:ff9b::7f00:1]/", false},
		{"http://[64:ff9b:1::1]/", false},
		{"http://[2002:7f00:1::]/", false},
		{"http://[2001:db8::1]/", false},
		{"http://[fc00::1]/", false},
		{"http://[fd12:3456::1]/", false},
		{"http://[fe80::1]/", false},
		{"http://[fec0::1]/", false},
		{"http://[fe80::1%25eth0]/", false},
		{"http://[ff02::1]/", false},
		// Non-canonical IPv4: a browser reads every one of these as an address.
		{"http://0177.0.0.1/", false},
		{"http://127.1/", false},
		{"http://2130706433/", false},
		{"http://0x7f.0.0.1/", false},
		{"http://0x7f000001/", false},
		{"http://127.0.0.0x1/", false},
		{"http://0X7F.0.0.1/", false},
		{"http://foo.0x1/", false},
		{"http://foo.123/", false},
		{"http://127.0.0.1./", false},
		// localhost names never reach DNS in a browser.
		{"http://localhost/", false},
		{"http://LOCALHOST./", false},
		{"http://foo.localhost/", false},
		// Resolution: any reserved answer, a failure, or nothing at all is refused.
		{"http://internal.example/", false},
		{"http://cgnat.example/", false},
		{"http://nxdomain.example/", false},
		{"http://empty.example/", false},
		// Schemes
		{"file:///etc/passwd", false},
		{"gopher://public.example/", false},
		{"http:///path", false},
		// Allowed
		{"http://8.8.8.8/", true},
		{"https://[2606:4700:4700::1111]/", true},
		{"https://public.example/path", true},
		{"https://public.example:8443/", true},
		{"https://mapped.example/", true},
		{"https://localhost.example/", false}, // unlisted in the stub: NXDOMAIN
		{"http://100.128.0.1/", true},
		{"http://198.20.0.1/", true},
	} {
		err := ValidateURL(tc.url, true)
		if got := err == nil; got != tc.want {
			t.Errorf("ValidateURL(%q, true) = %v, want allowed=%v", tc.url, err, tc.want)
		}
	}
}

// Only the last label makes a host numeric, and only an exact localhost label
// is refused by name: these are ordinary hostnames that go to DNS.
func TestValidateURLOrdinaryNames(t *testing.T) {
	stubLookup(t, map[string][]string{
		"0x.example":        {"93.184.215.14"},
		"123.example":       {"93.184.215.14"},
		"localhost.example": {"93.184.215.14"},
		"mylocalhost":       {"93.184.215.14"},
	})
	for _, u := range []string{"https://0x.example/", "https://123.example/", "https://localhost.example/", "https://mylocalhost/"} {
		if err := ValidateURL(u, true); err != nil {
			t.Errorf("ValidateURL(%q, true) = %v, want allowed", u, err)
		}
	}
}

// blockPrivate=false disables the address checks entirely; the scheme check stays.
func TestValidateURLBlockPrivateOff(t *testing.T) {
	stubLookup(t, nil)
	for _, u := range []string{"http://127.0.0.1/", "http://localhost/", "http://0x7f.1/", "http://nxdomain.example/"} {
		if err := ValidateURL(u, false); err != nil {
			t.Errorf("ValidateURL(%q, false) = %v, want allowed", u, err)
		}
	}
	if err := ValidateURL("file:///etc/passwd", false); err == nil {
		t.Error("file:// scheme should be rejected")
	}
}

func TestIsReservedAddr(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
		{"2001:4860:4860::8888", false},
		{"100.64.0.0", true},
		{"192.0.0.8", true},
		{"198.18.5.5", true},
		{"::ffff:8.8.8.8", true}, // as a literal; resolver answers are unmapped first
		{"64:ff9b::808:808", true},
		{"ff05::2", true},
		{"fe80::1%eth0", true},
	} {
		if got := IsReservedAddr(netip.MustParseAddr(tc.addr)); got != tc.want {
			t.Errorf("IsReservedAddr(%s) = %v, want %v", tc.addr, got, tc.want)
		}
	}
	if !IsReservedAddr(netip.Addr{}) {
		t.Error("the zero Addr must count as reserved")
	}
}
