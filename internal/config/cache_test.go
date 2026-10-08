package config

import (
	"testing"
	"time"
)

func TestLoad_CacheDefaults(t *testing.T) {
	cfg, err := loadWith(t, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CacheEnabled || cfg.CacheTTLThreads != 10*time.Minute || cfg.CacheTTLPages != 30*time.Minute ||
		cfg.CacheMaxBytes != 64<<20 || cfg.CacheMaxItemBytes != 4<<20 || cfg.CacheKeyPrefix != "omnifeed:cache" {
		t.Errorf("cache defaults = enabled %v threads %s pages %s max %d item %d prefix %q",
			cfg.CacheEnabled, cfg.CacheTTLThreads, cfg.CacheTTLPages, cfg.CacheMaxBytes, cfg.CacheMaxItemBytes, cfg.CacheKeyPrefix)
	}
}

func TestLoad_CacheKnobs(t *testing.T) {
	cases := []struct {
		key, value string
		wantErr    bool
		check      func(Config) bool
	}{
		{"OMNIFEED_CACHE_ENABLED", "false", false, func(c Config) bool { return !c.CacheEnabled }},
		{"OMNIFEED_CACHE_TTL_THREADS", "2m", false, func(c Config) bool { return c.CacheTTLThreads == 2*time.Minute }},
		{"OMNIFEED_CACHE_TTL_PAGES", "0s", false, func(c Config) bool { return c.CacheTTLPages == 0 }},
		{"OMNIFEED_CACHE_MAX_BYTES", "1024", false, func(c Config) bool { return c.CacheMaxBytes == 1024 }},
		{"OMNIFEED_CACHE_TTL_THREADS", "-1m", true, nil},
		{"OMNIFEED_CACHE_MAX_ITEM_BYTES", "-5", true, nil},
		{"OMNIFEED_CACHE_ENABLED", "maybe", true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			cfg, err := loadWith(t, tc.key, tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want a config error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.check(cfg) {
				t.Errorf("%s=%s not applied", tc.key, tc.value)
			}
		})
	}
}
