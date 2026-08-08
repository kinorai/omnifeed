package config

import (
	"slices"
	"testing"
)

// Load requires crawl4ai, so every case sets it.
func loadWith(t *testing.T, key, value string) (Config, error) {
	t.Helper()
	t.Setenv("OMNIFEED_CRAWL4AI_URL", "http://crawl4ai:11235/crawl")
	if key != "" {
		t.Setenv(key, value)
	}
	return Load()
}

func TestLoad_FetchMaxChars(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "default when unset", value: "", want: 120000},
		{name: "explicit value", value: "42000", want: 42000},
		{name: "zero means unlimited", value: "0", want: 0},
		{name: "negative is a config error", value: "-1", wantErr: true},
		{name: "non-numeric is a config error", value: "lots", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := ""
			if tc.value != "" {
				key = "OMNIFEED_FETCH_MAX_CHARS"
			}
			cfg, err := loadWith(t, key, tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got FetchMaxChars=%d", cfg.FetchMaxChars)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.FetchMaxChars != tc.want {
				t.Errorf("FetchMaxChars: got %d, want %d", cfg.FetchMaxChars, tc.want)
			}
		})
	}
}

// OMNIFEED_LIGHTPANDA_CDP_URL is optional; when set it must be a ws://|wss:// URL.
func TestLoad_LightpandaCDPURL(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "unset means disabled", value: "", want: ""},
		{name: "ws url ok", value: "ws://lightpanda:9222", want: "ws://lightpanda:9222"},
		{name: "wss url ok", value: "wss://lightpanda:9222", want: "wss://lightpanda:9222"},
		{name: "http scheme is a config error", value: "http://lightpanda:9222", wantErr: true},
		{name: "bare host is a config error", value: "lightpanda:9222", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := ""
			if tc.value != "" {
				key = "OMNIFEED_LIGHTPANDA_CDP_URL"
			}
			cfg, err := loadWith(t, key, tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got LightpandaCDPURL=%q", cfg.LightpandaCDPURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LightpandaCDPURL != tc.want {
				t.Errorf("LightpandaCDPURL: got %q, want %q", cfg.LightpandaCDPURL, tc.want)
			}
		})
	}
}

// OMNIFEED_DISCOURSE_HOSTS is tri-state: unset keeps the shipped list, a value
// replaces it, and an explicitly empty value disables the engine.
func TestLoad_DiscourseHosts(t *testing.T) {
	cases := []struct {
		name  string
		set   bool
		value string
		want  []string
	}{
		{name: "default when unset", want: splitHosts(defaultDiscourseHosts)},
		{name: "explicit list replaces the default", set: true,
			value: "forum.example.com,Forum.Two.Org", want: []string{"forum.example.com", "forum.two.org"}},
		{name: "whitespace and empty entries are dropped", set: true,
			value: " a.example.com , , b.example.com ", want: []string{"a.example.com", "b.example.com"}},
		{name: "explicitly empty disables the engine", set: true, value: "", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OMNIFEED_CRAWL4AI_URL", "http://crawl4ai:11235/crawl")
			if tc.set {
				t.Setenv("OMNIFEED_DISCOURSE_HOSTS", tc.value)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !slices.Equal(cfg.DiscourseHosts, tc.want) {
				t.Errorf("DiscourseHosts = %q, want %q", cfg.DiscourseHosts, tc.want)
			}
		})
	}
}
