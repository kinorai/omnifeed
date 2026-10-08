package fetchcache

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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

// ErrPermission wraps a Redis error that retrying will not fix: the ACL user
// may not touch the cache's keys (NOPERM), or the connection is not
// authenticated (NOAUTH, WRONGPASS). It does not trip the cooldown — a
// Failover answers it by switching to its in-process backend for good.
var ErrPermission = errors.New("fetch cache: redis permission denied")

// Kinds of backend error, the values of the
// omnifeed_cache_backend_errors_total `kind` label.
const (
	ErrKindNoPerm    = "noperm"    // permission/auth: permanent until the ACL changes
	ErrKindTransient = "transient" // timeouts, refused connections, LOADING…: cooldown and retry
	ErrKindOther     = "other"     // startup probe only: Redis answered, but not as a cache can use
)

// probeTTL bounds how long the startup probe's key outlives a crashed probe.
const probeTTL = 10 * time.Second

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
	onError  func(op, kind string)

	downUntil atomic.Int64 // unix nanos; 0 = healthy
}

// RedisConfig configures a Redis backend.
type RedisConfig struct {
	Client       redis.UniversalClient
	Prefix       string           // key namespace, e.g. "omnifeed:cache"
	MaxItemBytes int              // compressed size cap per entry; <= 0 = no cap
	Cooldown     time.Duration    // defaults to 30s
	Now          func() time.Time // defaults to time.Now
	// OnError, when set, is called once per Redis error with the operation
	// ("get", "set", "probe") and its kind (ErrKindNoPerm, ErrKindTransient,
	// ErrKindOther). Cooldown fast-fails, oversize entries, corrupt values
	// and callers hanging up are not Redis errors and are not reported.
	OnError func(op, kind string)
}

// NewRedis builds a Redis backend.
func NewRedis(cfg RedisConfig) *Redis {
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = defaultCooldown
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.OnError == nil {
		cfg.OnError = func(string, string) {}
	}
	return &Redis{client: cfg.Client, prefix: cfg.Prefix, maxItem: cfg.MaxItemBytes,
		cooldown: cfg.Cooldown, now: cfg.Now, onError: cfg.OnError}
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
		return Entry{}, false, r.fail("get", err)
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
		return r.fail("set", err)
	}
	return nil
}

// fail classifies a Redis error from op. A permission error is permanent and
// is returned wrapped in ErrPermission without a cooldown (the Failover above
// stops calling Redis); anything else trips the cooldown, as before.
func (r *Redis) fail(op string, err error) error {
	if isPermission(err) {
		r.onError(op, ErrKindNoPerm)
		return fmt.Errorf("redis %s: %w: %w", op, ErrPermission, err)
	}
	r.onError(op, ErrKindTransient)
	r.trip()
	return fmt.Errorf("redis %s: %w", op, err)
}

// isPermission reports an ACL or authentication refusal: retrying the same
// command as the same user gets the same answer until an operator steps in.
func isPermission(err error) bool {
	if redis.IsPermissionError(err) || redis.IsAuthError(err) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "no permissions")
}

// errProbeMismatch: the probe's SET was accepted but its GET did not return
// the value — something between omnifeed and Redis (a proxy, a renamed
// command) is not storing what the cache writes.
var errProbeMismatch = errors.New("probe value not read back")

// Probe checks, once at startup, that this Redis user can do what the cache
// does: SET a key under the prefix (with a short TTL, so nothing needs
// deleting) and GET it back. Each command is bounded by timeout. The returned
// kind is "" on success, ErrKindNoPerm for a permission error, ErrKindOther for
// any other answer Redis will keep giving (an error reply such as READONLY or
// an unknown command, or a value not read back), and ErrKindTransient for a
// Redis that did not answer (timeout, refused connection) or answered with a
// retryable error (LOADING, TRYAGAIN, BUSY, CLUSTERDOWN, MASTERDOWN, and OOM or
// MISCONF: a full or unsaveable Redis refuses writes until memory frees or the
// disk recovers, which the runtime path already treats as transient).
func (r *Redis) Probe(ctx context.Context, timeout time.Duration) (kind string, err error) {
	key := r.prefix + ":probe:" + fmt.Sprint(r.now().UnixNano())
	want := "ok"
	defer func() {
		if kind != "" {
			r.onError("probe", kind)
		}
	}()
	setCtx, cancel := context.WithTimeout(ctx, timeout)
	err = r.client.Set(setCtx, key, want, probeTTL).Err()
	cancel()
	if err != nil {
		return probeKind(err), fmt.Errorf("redis probe set %s: %w", key, err)
	}
	getCtx, cancel := context.WithTimeout(ctx, timeout)
	got, err := r.client.Get(getCtx, key).Result()
	cancel()
	if err != nil && !errors.Is(err, redis.Nil) {
		return probeKind(err), fmt.Errorf("redis probe get %s: %w", key, err)
	}
	if got != want {
		return ErrKindOther, fmt.Errorf("redis probe get %s: %w", key, errProbeMismatch)
	}
	return "", nil
}

func probeKind(err error) string {
	if isPermission(err) {
		return ErrKindNoPerm
	}
	var rerr redis.Error
	if !errors.As(err, &rerr) {
		return ErrKindTransient // no reply at all: network, timeout, dial
	}
	msg := rerr.Error()
	for _, p := range []string{"LOADING", "TRYAGAIN", "BUSY", "CLUSTERDOWN", "MASTERDOWN", "OOM", "MISCONF"} {
		if strings.HasPrefix(msg, p) {
			return ErrKindTransient
		}
	}
	return ErrKindOther
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
