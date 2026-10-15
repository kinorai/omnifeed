// Package redisstate shares the Reddit method chain's skip state between
// replicas through Redis (see reddit.SharedState).
//
// A held method is one key whose TTL is the time left on the hold. Reddit's
// budget belongs to the credential, not to the pod, so a back-off one replica
// receives applies to all of them. The TTL is Redis' own clock, so the
// replicas' clocks never need to agree.
//
// Every operation is a script, like the rate limiter's, so a Redis ACL that
// already admits the limiter needs no new command. The keys share one hash
// tag, so the multi-key read stays in one slot on a Redis Cluster.
package redisstate

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// extend sets the hold to at least ARGV[1] ms and never shortens it: a 10m
// cooldown from one pod must not cut a 15m Retry-After hold from another.
// PTTL is -2 for a missing key and -1 for one without a TTL (never written
// here), so both are replaced.
var extend = redis.NewScript(`
local left = redis.call('PTTL', KEYS[1])
if left < tonumber(ARGV[1]) then
  redis.call('SET', KEYS[1], '1', 'PX', ARGV[1])
end
return 0
`)

// read returns the time left in ms on each key, -2 or -1 when it is not held.
var read = redis.NewScript(`
local out = {}
for i, k in ipairs(KEYS) do
  out[i] = redis.call('PTTL', k)
end
return out
`)

var clear = redis.NewScript(`return redis.call('DEL', KEYS[1])`)

// Store is a reddit.SharedState on Redis.
type Store struct {
	client redis.UniversalClient
	prefix string
}

// New returns a Store whose keys start with prefix, e.g.
// "omnifeed:ratelimit" gives "omnifeed:ratelimit:{reddit-method}:session".
func New(client redis.UniversalClient, prefix string) *Store {
	return &Store{client: client, prefix: prefix}
}

func (s *Store) key(method string) string {
	return s.prefix + ":{reddit-method}:" + method
}

// Holds returns the time left on each held method. A method that is not held
// is absent from the map. One round trip for all methods.
func (s *Store) Holds(ctx context.Context, methods []string) (map[string]time.Duration, error) {
	keys := make([]string, len(methods))
	for i, m := range methods {
		keys[i] = s.key(m)
	}
	left, err := read.Run(ctx, s.client, keys).Int64Slice()
	if err != nil {
		return nil, err
	}
	holds := map[string]time.Duration{}
	for i, m := range methods {
		if i < len(left) && left[i] > 0 {
			holds[m] = time.Duration(left[i]) * time.Millisecond
		}
	}
	return holds, nil
}

// Hold skips method on every replica for at least d.
func (s *Store) Hold(ctx context.Context, method string, d time.Duration) error {
	if d < time.Millisecond {
		return nil
	}
	return extend.Run(ctx, s.client, []string{s.key(method)}, d.Milliseconds()).Err()
}

// Clear ends the hold on method for every replica.
func (s *Store) Clear(ctx context.Context, method string) error {
	return clear.Run(ctx, s.client, []string{s.key(method)}).Err()
}
