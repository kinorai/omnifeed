package redisstate

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	m := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: m.Addr(), MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = c.Close() })
	return New(c, "omnifeed:ratelimit"), m
}

func TestHoldIsReadByEveryReplica(t *testing.T) {
	s, m := newStore(t)
	ctx := context.Background()
	if err := s.Hold(ctx, "session", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if !m.Exists("omnifeed:ratelimit:{reddit-method}:session") {
		t.Fatalf("keys = %v", m.Keys())
	}
	holds, err := s.Holds(ctx, []string{"client_id", "session"})
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 || holds["session"] != 10*time.Minute {
		t.Errorf("holds = %v, want only session, 10m", holds)
	}
}

// A shorter hold never cuts a longer one another replica recorded.
func TestHoldNeverShortens(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	_ = s.Hold(ctx, "client_id", 15*time.Minute)
	_ = s.Hold(ctx, "client_id", 10*time.Minute)
	holds, _ := s.Holds(ctx, []string{"client_id"})
	if holds["client_id"] != 15*time.Minute {
		t.Errorf("hold = %v, want 15m kept", holds["client_id"])
	}
	_ = s.Hold(ctx, "client_id", 20*time.Minute)
	holds, _ = s.Holds(ctx, []string{"client_id"})
	if holds["client_id"] != 20*time.Minute {
		t.Errorf("hold = %v, want 20m", holds["client_id"])
	}
}

// The hold ends by itself on Redis' clock, and Clear ends it at once.
func TestHoldExpiresAndClears(t *testing.T) {
	s, m := newStore(t)
	ctx := context.Background()
	_ = s.Hold(ctx, "browser", time.Minute)
	_ = s.Hold(ctx, "session", time.Hour)
	m.FastForward(2 * time.Minute)
	if err := s.Clear(ctx, "session"); err != nil {
		t.Fatal(err)
	}
	holds, _ := s.Holds(ctx, []string{"browser", "session"})
	if len(holds) != 0 {
		t.Errorf("holds = %v, want none", holds)
	}
}

func TestUnreachableRedisReturnsAnError(t *testing.T) {
	s, m := newStore(t)
	m.Close()
	if _, err := s.Holds(context.Background(), []string{"session"}); err == nil {
		t.Error("Holds on a closed Redis must fail")
	}
	if err := s.Hold(context.Background(), "session", time.Minute); err == nil {
		t.Error("Hold on a closed Redis must fail")
	}
}
