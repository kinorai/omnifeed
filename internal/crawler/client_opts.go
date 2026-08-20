package crawler

import "time"

// ClientOptions configures HTTP connection timeouts for RSS/Atom feed scrapers.
type ClientOptions struct {
	Timeout         time.Duration
	IdleConnTimeout time.Duration
	MaxConnsPerHost int
}

// DefaultClientOptions provides sensible defaults for concurrent feed crawling.
func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		Timeout:         15 * time.Second,
		IdleConnTimeout: 30 * time.Second,
		MaxConnsPerHost: 4,
	}
}
