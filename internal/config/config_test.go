package config

import (
	"slices"
	"testing"
	"time"
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

// The crawl4ai latency knobs default to crawl4ai's own defaults (no full-page
// scan, 0.1s settle) and reject out-of-range values — a settle longer than the
// 60s page budget is a misconfiguration, not a preference.
func TestLoad_Crawl4AILatencyKnobs(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := loadWith(t, "", "")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Crawl4AIScanFullPage {
			t.Errorf("Crawl4AIScanFullPage default = true, want false")
		}
		if cfg.Crawl4AIScrollDelay != 0.5 {
			t.Errorf("Crawl4AIScrollDelay default = %v, want 0.5", cfg.Crawl4AIScrollDelay)
		}
		if cfg.Crawl4AIDelayBeforeHTML != 0.1 {
			t.Errorf("Crawl4AIDelayBeforeHTML default = %v, want 0.1", cfg.Crawl4AIDelayBeforeHTML)
		}
	})
	t.Run("explicit values", func(t *testing.T) {
		t.Setenv("OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE", "true")
		t.Setenv("OMNIFEED_CRAWL4AI_DELAY_BEFORE_HTML", "1.0")
		cfg, err := loadWith(t, "OMNIFEED_CRAWL4AI_SCROLL_DELAY", "0.25")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.Crawl4AIScanFullPage || cfg.Crawl4AIScrollDelay != 0.25 || cfg.Crawl4AIDelayBeforeHTML != 1.0 {
			t.Errorf("got scan=%v scroll=%v delay=%v, want true/0.25/1.0",
				cfg.Crawl4AIScanFullPage, cfg.Crawl4AIScrollDelay, cfg.Crawl4AIDelayBeforeHTML)
		}
	})
	for _, tc := range []struct{ key, value string }{
		{"OMNIFEED_CRAWL4AI_DELAY_BEFORE_HTML", "-0.1"},
		{"OMNIFEED_CRAWL4AI_DELAY_BEFORE_HTML", "61"},
		{"OMNIFEED_CRAWL4AI_SCROLL_DELAY", "-1"},
		{"OMNIFEED_CRAWL4AI_SCROLL_DELAY", "61"},
	} {
		t.Run(tc.key+"="+tc.value+" is a config error", func(t *testing.T) {
			if _, err := loadWith(t, tc.key, tc.value); err == nil {
				t.Fatalf("want error for %s=%s", tc.key, tc.value)
			}
		})
	}
}

func TestSearchAuditMode(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
		wantErr           bool
	}{
		{name: "defaults to off", value: "", want: "off"},
		{name: "summary", value: "summary", want: "summary"},
		{name: "full", value: "full", want: "full"},
		{name: "case and space tolerant", value: "  FULL  ", want: "full"},
		{name: "unknown mode is a startup error", value: "verbose", wantErr: true},
		// A level name is exactly the mistake this setting exists to prevent:
		// it is a data feed, not a severity.
		{name: "a log level is not a mode", value: "debug", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "OMNIFEED_SEARCH_AUDIT"
			if tc.value == "" {
				key = ""
			}
			c, err := loadWith(t, key, tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() = nil error, want one for %q", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.SearchAudit != tc.want {
				t.Errorf("SearchAudit = %q, want %q", c.SearchAudit, tc.want)
			}
		})
	}
}

// Regression, code review of #35: the audit log is emitted at INFO through the
// shared logger, which drops INFO at warn/error. Honouring the request while
// discarding every line is the worst outcome — the operator sees no error and
// concludes the searcher is broken. Startup must refuse instead.
func TestSearchAuditRejectsALevelThatDiscardsIt(t *testing.T) {
	for _, tc := range []struct {
		level   string
		wantErr bool
	}{
		{level: "info"},
		{level: "debug"},
		{level: "DEBUG"},
		{level: "warn", wantErr: true},
		{level: "warning", wantErr: true},
		{level: "error", wantErr: true},
	} {
		t.Run(tc.level, func(t *testing.T) {
			t.Setenv("OMNIFEED_SEARCH_AUDIT", "full")
			c, err := loadWith(t, "OMNIFEED_LOG_LEVEL", tc.level)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() = nil error at level %q, want a refusal", tc.level)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.SearchAudit != "full" {
				t.Errorf("SearchAudit = %q, want full", c.SearchAudit)
			}
		})
	}
}

// The refusal must not fire when the audit log is off — the default deployment
// runs at whatever level it likes.
func TestSearchAuditOffAllowsAnyLevel(t *testing.T) {
	if _, err := loadWith(t, "OMNIFEED_LOG_LEVEL", "error"); err != nil {
		t.Fatalf("Load with audit off at error level: %v", err)
	}
}

// OMNIFEED_REDIS_URL is shape-checked only: config carries no Redis client, so
// a scheme mistake is what Load can usefully catch.
func TestLoad_RedisURL(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "redis scheme", value: "redis://redis:6379/2"},
		{name: "rediss scheme with credentials", value: "rediss://user:pass@redis.example.com:6380/2"},
		{name: "wrong scheme is a config error", value: "http://redis:6379", wantErr: true},
		{name: "garbage is a config error", value: "redis://user:pass{@host:6379", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, "OMNIFEED_REDIS_URL", tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got RedisURL=%q", cfg.RedisURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.RedisURL != tc.value {
				t.Errorf("RedisURL: got %q, want %q", cfg.RedisURL, tc.value)
			}
		})
	}
}

// The timeout bounds one Redis operation, so a zero or negative value would
// make every operation fail instantly rather than mean "no limit".
func TestLoad_RedisTimeout(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "default when unset", value: "", want: 250 * time.Millisecond},
		{name: "explicit value", value: "1s", want: time.Second},
		{name: "zero is a config error", value: "0s", wantErr: true},
		{name: "negative is a config error", value: "-1s", wantErr: true},
		{name: "unparseable is a config error", value: "soon", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := ""
			if tc.value != "" {
				key = "OMNIFEED_REDIS_TIMEOUT"
			}
			cfg, err := loadWith(t, key, tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got RedisTimeout=%s", cfg.RedisTimeout)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.RedisTimeout != tc.want {
				t.Errorf("RedisTimeout: got %s, want %s", cfg.RedisTimeout, tc.want)
			}
		})
	}
}

// Unset means off: no URL, and the defaults that only matter once one is set.
func TestLoad_RedisDefaults(t *testing.T) {
	cfg, err := loadWith(t, "", "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RedisURL != "" {
		t.Errorf("RedisURL: got %q, want empty (distributed limiting off)", cfg.RedisURL)
	}
	if cfg.RedisKeyPrefix != "omnifeed:ratelimit" {
		t.Errorf("RedisKeyPrefix: got %q, want omnifeed:ratelimit", cfg.RedisKeyPrefix)
	}
	if cfg.RedisTimeout != 250*time.Millisecond {
		t.Errorf("RedisTimeout: got %s, want 250ms", cfg.RedisTimeout)
	}
}

// The prefix namespaces the limiter keys, so deployments can share one Redis.
func TestLoad_RedisKeyPrefix(t *testing.T) {
	cfg, err := loadWith(t, "OMNIFEED_REDIS_KEY_PREFIX", "staging:omnifeed:rl")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RedisKeyPrefix != "staging:omnifeed:rl" {
		t.Errorf("RedisKeyPrefix: got %q, want staging:omnifeed:rl", cfg.RedisKeyPrefix)
	}
}
