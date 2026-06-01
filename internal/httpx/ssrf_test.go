package httpx

import "testing"

// TestValidateURL_BlocksPrivate locks the SSRF guard, especially the IPv6
// ranges the old IPv4-only check let through. Literal-IP hosts resolve without
// DNS, so this is hermetic.
func TestValidateURL_BlocksPrivate(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1/",            // loopback v4
		"http://[::1]/",                // loopback v6
		"http://10.0.0.1/",             // RFC1918
		"http://172.16.0.1/",           // RFC1918
		"http://192.168.1.1/",          // RFC1918
		"http://169.254.169.254/",      // link-local v4 (cloud metadata)
		"http://[fc00::1]/",            // ULA v6 (was bypassing the old check)
		"http://[fe80::1]/",            // link-local v6
		"http://0.0.0.0/",              // unspecified
	}
	for _, u := range blocked {
		if err := ValidateURL(u, true); err == nil {
			t.Errorf("ValidateURL(%q, true) = nil, want blocked", u)
		}
	}

	allowed := []string{
		"http://8.8.8.8/",                  // public v4
		"https://[2606:4700:4700::1111]/",  // public v6 (Cloudflare)
	}
	for _, u := range allowed {
		if err := ValidateURL(u, true); err != nil {
			t.Errorf("ValidateURL(%q, true) = %v, want allowed", u, err)
		}
	}

	// blockPrivate=false disables the IP check entirely.
	if err := ValidateURL("http://127.0.0.1/", false); err != nil {
		t.Errorf("blockPrivate=false should allow private: %v", err)
	}
	// non-http scheme rejected regardless.
	if err := ValidateURL("file:///etc/passwd", true); err == nil {
		t.Error("file:// scheme should be rejected")
	}
}
