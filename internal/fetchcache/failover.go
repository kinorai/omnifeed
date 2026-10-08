package fetchcache

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Backend names, the values of the omnifeed_cache_backend `backend` label.
const (
	BackendRedis  = "redis"
	BackendMemory = "memory"
)

// Failover is the Redis backend with an in-process fallback for the failures
// retrying cannot fix. On 2026-10-08 the Redis ACL user could only touch the
// rate limiter's keys: every cache GET and SET answered NOPERM, the Redis
// backend cooled down and retried every 30 s forever, and nothing was ever
// cached, on any replica — silently, since every request still succeeded as a
// miss. A permission error (at the startup probe or on any later Get/Set) now
// switches this replica to the in-process LRU for good and says so once at
// ERROR. Transient errors keep the Redis backend and its degrade-to-miss
// cooldown: Redis coming back is the expected outcome.
type Failover struct {
	redis     *Redis
	memory    Backend
	logger    *slog.Logger
	prefix    string
	onBackend func(name string)

	onMemory atomic.Bool
	switched sync.Once
}

// FailoverConfig configures a Failover.
type FailoverConfig struct {
	Redis  *Redis
	Memory Backend // the fallback; used only after a switch
	Prefix string  // the cache key prefix, named in the ERROR log
	Logger *slog.Logger
	// OnBackend, when set, is called with the active backend's name
	// (BackendRedis at construction, BackendMemory on the switch).
	OnBackend func(name string)
}

var _ Backend = (*Failover)(nil)

// NewFailover builds a Failover that starts on Redis.
func NewFailover(cfg FailoverConfig) *Failover {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.OnBackend == nil {
		cfg.OnBackend = func(string) {}
	}
	f := &Failover{redis: cfg.Redis, memory: cfg.Memory, logger: cfg.Logger, prefix: cfg.Prefix, onBackend: cfg.OnBackend}
	f.onBackend(BackendRedis)
	return f
}

// Probe runs the Redis backend's startup check (Redis.Probe) with timeout per
// command. A permission error, or any other answer Redis will keep giving,
// switches to the in-process backend; a Redis that did not answer keeps the
// Redis backend, whose runtime cooldown covers it until it does.
func (f *Failover) Probe(ctx context.Context, timeout time.Duration) {
	kind, err := f.redis.Probe(ctx, timeout)
	switch kind {
	case "":
		f.logger.Info("fetch cache redis probe ok", "key_prefix", f.prefix)
	case ErrKindTransient:
		f.logger.Warn("fetch cache: redis did not answer the startup probe; keeping the redis backend "+
			"(lookups are served as misses until it answers)", "err", err)
	default:
		f.switchToMemory(kind, err)
	}
}

// Backend is the active backend's name: BackendRedis or BackendMemory.
func (f *Failover) Backend() string {
	if f.onMemory.Load() {
		return BackendMemory
	}
	return BackendRedis
}

// Get reads from the active backend. A permission error from Redis switches
// to the in-process backend and answers from it (a miss, the first time), so
// the caller's fetch is stored there.
func (f *Failover) Get(ctx context.Context, key string) (Entry, bool, error) {
	if f.onMemory.Load() {
		return f.memory.Get(ctx, key)
	}
	e, ok, err := f.redis.Get(ctx, key)
	if errors.Is(err, ErrPermission) {
		f.switchToMemory(ErrKindNoPerm, err)
		return f.memory.Get(ctx, key)
	}
	return e, ok, err
}

// Set writes to the active backend, switching on a permission error like Get.
func (f *Failover) Set(ctx context.Context, key string, e Entry, ttl time.Duration) error {
	if f.onMemory.Load() {
		return f.memory.Set(ctx, key, e, ttl)
	}
	err := f.redis.Set(ctx, key, e, ttl)
	if errors.Is(err, ErrPermission) {
		f.switchToMemory(ErrKindNoPerm, err)
		return f.memory.Set(ctx, key, e, ttl)
	}
	return err
}

func (f *Failover) switchToMemory(kind string, err error) {
	f.switched.Do(func() {
		f.onMemory.Store(true)
		f.onBackend(BackendMemory)
		if kind == ErrKindNoPerm {
			f.logger.Error("fetch cache: redis user cannot write "+f.prefix+":* (NOPERM); "+
				"falling back to the in-process cache — grant ~"+f.prefix+":* (commands GET and SET) to the ACL user "+
				"or set OMNIFEED_CACHE_KEY_PREFIX under an allowed key pattern",
				"err", err)
			return
		}
		f.logger.Error("fetch cache: redis cannot be used as the cache under "+f.prefix+":*; "+
			"falling back to the in-process cache — check that the redis user can GET and SET ~"+f.prefix+":* on a writable primary",
			"err", err)
	})
}
