package fetchcache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// entryOverhead approximates the per-entry bookkeeping (list element, map
// slot, key, timestamps) so many tiny documents cannot blow past MaxBytes.
const entryOverhead = 256

// Memory is the in-process LRU backend, used when OMNIFEED_REDIS_URL is unset,
// and as the Failover's fallback when Redis refuses the cache's keys.
// It is bounded by the approximate byte size of what it holds; expired entries
// are dropped on read and are first in line for eviction by recency.
type Memory struct {
	maxBytes int
	maxItem  int
	now      func() time.Time
	onSize   func(bytes int)

	mu    sync.Mutex
	ll    *list.List // front = most recently used
	items map[string]*list.Element
	bytes int
}

type memItem struct {
	key     string
	entry   Entry
	expires time.Time
	size    int
}

// MemoryConfig configures a Memory backend.
type MemoryConfig struct {
	MaxBytes     int              // total budget; <= 0 stores nothing
	MaxItemBytes int              // one entry's cap; <= 0 means MaxBytes
	Now          func() time.Time // defaults to time.Now
	OnSize       func(bytes int)  // called with the new total after every change
}

// NewMemory builds an in-process LRU backend.
func NewMemory(cfg MemoryConfig) *Memory {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxItemBytes <= 0 || cfg.MaxItemBytes > cfg.MaxBytes {
		cfg.MaxItemBytes = cfg.MaxBytes
	}
	return &Memory{
		maxBytes: cfg.MaxBytes,
		maxItem:  cfg.MaxItemBytes,
		now:      cfg.Now,
		onSize:   cfg.OnSize,
		ll:       list.New(),
		items:    make(map[string]*list.Element),
	}
}

// Get returns the live entry for key, if any.
func (m *Memory) Get(_ context.Context, key string) (Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return Entry{}, false, nil
	}
	it := el.Value.(*memItem)
	if !m.now().Before(it.expires) {
		m.remove(el)
		m.reportLocked()
		return Entry{}, false, nil
	}
	m.ll.MoveToFront(el)
	return it.entry, true, nil
}

// Set stores e for ttl, evicting least-recently-used entries to stay within
// the byte budget. An entry over the item cap is skipped (ErrTooLarge).
func (m *Memory) Set(_ context.Context, key string, e Entry, ttl time.Duration) error {
	size := entrySize(key, e)
	if size > m.maxItem {
		return ErrTooLarge
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.remove(el)
	}
	m.items[key] = m.ll.PushFront(&memItem{key: key, entry: e, expires: m.now().Add(ttl), size: size})
	m.bytes += size
	for m.bytes > m.maxBytes {
		m.remove(m.ll.Back())
	}
	m.reportLocked()
	return nil
}

// Bytes is the approximate size of everything held.
func (m *Memory) Bytes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bytes
}

func (m *Memory) remove(el *list.Element) {
	it := el.Value.(*memItem)
	m.ll.Remove(el)
	delete(m.items, it.key)
	m.bytes -= it.size
}

func (m *Memory) reportLocked() {
	if m.onSize != nil {
		m.onSize(m.bytes)
	}
}

func entrySize(key string, e Entry) int {
	n := entryOverhead + len(key) + len(e.Doc.PageContent)
	for k, v := range e.Doc.Metadata {
		n += len(k) + len(v)
	}
	return n
}
