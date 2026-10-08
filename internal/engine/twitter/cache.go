package twitter

import (
	"sync"
	"time"
)

// ttlCache is a small in-process cache of fetched bundles by post id. It
// holds the parsed bundle, not the rendered document, so a repeat read in
// another format costs no upstream call.
type ttlCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]cacheEntry
	now     func() time.Time
}

type cacheEntry struct {
	b       bundle
	expires time.Time
}

func newTTLCache(ttl time.Duration, maxEntries int) *ttlCache {
	return &ttlCache{ttl: ttl, max: maxEntries, entries: map[string]cacheEntry{}, now: time.Now}
}

func (c *ttlCache) get(id string) (bundle, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	en, ok := c.entries[id]
	if !ok {
		return bundle{}, false
	}
	if c.now().After(en.expires) {
		delete(c.entries, id)
		return bundle{}, false
	}
	return en.b, true
}

func (c *ttlCache) put(id string, b bundle) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= c.max {
		for k, en := range c.entries {
			if now.After(en.expires) {
				delete(c.entries, k)
			}
		}
		// Still full: drop an arbitrary entry. A bounded footprint matters
		// more here than which post gets refetched.
		for k := range c.entries {
			if len(c.entries) < c.max {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[id] = cacheEntry{b: b, expires: now.Add(c.ttl)}
}
