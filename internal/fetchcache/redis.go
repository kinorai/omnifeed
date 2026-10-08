package fetchcache

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrUnavailable is returned while the Redis backend is cooling down after a
// failure: the cache serves a miss without paying another timeout.
var ErrUnavailable = errors.New("fetch cache backend unavailable (cooling down)")

// ErrTooLarge is returned by Set for an entry over the backend's item cap. The
// response is still served; it just is not cached.
var ErrTooLarge = errors.New("fetch cache entry over the item size cap")

// defaultCooldown matches the rate limiter's fail-open cooldown: a dead Redis
// costs one operation timeout per cooldown, not one per request.
const defaultCooldown = 30 * time.Second

// Redis is the shared backend, used when OMNIFEED_REDIS_URL is set, so every
// replica serves every other replica's fetches. Values are gzip-compressed
// JSON; Redis expires them by TTL. Any Redis error trips a cooldown during
// which Get/Set fail fast with ErrUnavailable — the cache then serves misses
// and requests go upstream as if there were no cache.
type Redis struct {
	client   redis.UniversalClient
	prefix   string
	maxItem  int
	cooldown time.Duration
	now      func() time.Time

	downUntil atomic.Int64 // unix nanos; 0 = healthy
}

// RedisConfig configures a Redis backend.
type RedisConfig struct {
	Client       redis.UniversalClient
	Prefix       string           // key namespace, e.g. "omnifeed:cache"
	MaxItemBytes int              // compressed size cap per entry; <= 0 = no cap
	Cooldown     time.Duration    // defaults to 30s
	Now          func() time.Time // defaults to time.Now
}

// NewRedis builds a Redis backend.
func NewRedis(cfg RedisConfig) *Redis {
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = defaultCooldown
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Redis{client: cfg.Client, prefix: cfg.Prefix, maxItem: cfg.MaxItemBytes, cooldown: cfg.Cooldown, now: cfg.Now}
}

// Get reads and decodes key.
func (r *Redis) Get(ctx context.Context, key string) (Entry, bool, error) {
	if r.coolingDown() {
		return Entry{}, false, ErrUnavailable
	}
	b, err := r.client.Get(ctx, r.prefix+":"+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return Entry{}, false, nil
	}
	if err != nil {
		// A caller that hung up mid-GET is not a Redis failure: the client
		// reports the caller's dead ctx as its own error, and tripping on it
		// would turn the cache off for every other request. Discriminate on
		// ctx: the client's own ReadTimeout also surfaces as DeadlineExceeded
		// while ctx is still alive, and that one IS a backend failure.
		if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return Entry{}, false, ctx.Err()
		}
		r.trip()
		return Entry{}, false, fmt.Errorf("redis get: %w", err)
	}
	e, err := decode(b)
	if err != nil {
		// A corrupt or foreign value is a miss to overwrite, not an outage.
		return Entry{}, false, fmt.Errorf("redis decode: %w", err)
	}
	return e, true, nil
}

// Set encodes and stores e under key with ttl.
func (r *Redis) Set(ctx context.Context, key string, e Entry, ttl time.Duration) error {
	if r.coolingDown() {
		return ErrUnavailable
	}
	b, err := encode(e)
	if err != nil {
		return err
	}
	if r.maxItem > 0 && len(b) > r.maxItem {
		return ErrTooLarge
	}
	if err := r.client.Set(ctx, r.prefix+":"+key, b, ttl).Err(); err != nil {
		r.trip()
		return fmt.Errorf("redis set: %w", err)
	}
	return nil
}

func (r *Redis) coolingDown() bool {
	until := r.downUntil.Load()
	return until != 0 && r.now().UnixNano() < until
}

func (r *Redis) trip() { r.downUntil.Store(r.now().Add(r.cooldown).UnixNano()) }

func encode(e Entry) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(zw).Encode(e); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decode(b []byte) (Entry, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return Entry{}, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return Entry{}, err
	}
	return e, nil
}
