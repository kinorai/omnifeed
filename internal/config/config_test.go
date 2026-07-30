package config

import "testing"

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
