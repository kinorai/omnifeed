package redislimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// bookN runs n admission attempts and returns each wait, so a test can assert
// where the cap starts refusing. It also hands back the nonces, because
// releasing a slot means naming the lease that took it.
func bookN(t *testing.T, l *Limiter, m *miniredis.Miniredis, host string, n int) ([]time.Duration, []string) {
	t.Helper()
	win, next, inflight := l.keys(host)
	waits := make([]time.Duration, 0, n)
	nonces := make([]string, 0, n)
	for range n {
		wait, nonce, err := l.book(context.Background(), win, next, inflight)
		if err != nil {
			t.Fatalf("book: %v", err)
		}
		assertTTLs(t, m)
		waits = append(waits, wait)
		nonces = append(nonces, nonce)
	}
	return waits, nonces
}

// A cluster cap admits exactly N and refuses the next, with no MinDelay in play
// so nothing but the cap can be the cause of the refusal.
func TestClusterConcurrencyAdmitsExactlyN(t *testing.T) {
	l, m, _ := newFixture(t, Config{ClusterConcurrency: 3})

	waits, _ := bookN(t, l, m, "example.com", 4)

	for i, w := range waits[:3] {
		if w != 0 {
			t.Fatalf("admission %d: got wait %v, want 0 (cap is 3)", i+1, w)
		}
	}
	if waits[3] == 0 {
		t.Fatal("admission 4: got wait 0, want a refusal (cap is 3)")
	}
	if waits[3] > defaultConcurrencyRetry {
		t.Fatalf("admission 4: got wait %v, want at most the retry interval %v",
			waits[3], defaultConcurrencyRetry)
	}
}

// Spaced admissions accumulate as CONCURRENT in-flight requests: the second
// gets in while the first still holds a slot, MinDelay after it.
//
// This covers the acquire side only. Whether release re-serializes is the other
// half, and TestClusterConcurrencyReleaseDoesNotRescheduleFromCompletion is the
// test that actually discriminates there — this one passes either way.
func TestClusterConcurrencyAdmitsSpacedRequestsConcurrently(t *testing.T) {
	const delay = 100 * time.Millisecond
	l, m, c := newFixture(t, Config{ClusterConcurrency: 4, MinDelay: delay})

	// First admission is free.
	if w := book(t, l, m, "example.com"); w != 0 {
		t.Fatalf("first admission: got wait %v, want 0", w)
	}
	// Second is owed the spacing, not a serialization behind a completion.
	if w := book(t, l, m, "example.com"); w != delay {
		t.Fatalf("second admission: got wait %v, want the min delay %v", w, delay)
	}

	// After the spacing elapses the second gets in WITHOUT the first releasing.
	// That is the assertion that separates spacing from serialization.
	c.advance(delay)
	if w := book(t, l, m, "example.com"); w != 0 {
		t.Fatalf("second admission after spacing: got wait %v, want 0 with 1 still in flight", w)
	}

	_, _, inflight := l.keys("example.com")
	members, err := m.ZMembers(inflight)
	if err != nil {
		t.Fatalf("read in-flight leases: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("in-flight leases: got %d, want 2 concurrent", len(members))
	}
}

// The discriminating test for send-spacing vs gap-spacing, and the one that
// actually fails against the pre-cap release path.
//
// A release at t=50ms with a 100ms delay is the case that separates them. Gap
// spacing measures from completion and books the next admission at 150ms. Send
// spacing left it at 100ms when the request was admitted and release must not
// move it. So an attempt at t=100ms is admitted under a cap and still owes 50ms
// without one.
func TestClusterConcurrencyReleaseDoesNotRescheduleFromCompletion(t *testing.T) {
	const delay = 100 * time.Millisecond
	l, m, c := newFixture(t, Config{ClusterConcurrency: 4, MinDelay: delay})
	_, nextKey, inflightKey := l.keys("example.com")

	_, nonces := bookN(t, l, m, "example.com", 1)

	c.advance(delay / 2)
	l.release(nextKey, inflightKey, nonces[0])
	c.advance(delay / 2)

	// t = 100ms: exactly the send-spaced opening.
	if w := book(t, l, m, "example.com"); w != 0 {
		t.Fatalf("at the send-spaced opening: got wait %v, want 0. A nonzero wait here means "+
			"release rescheduled from completion, which is the serialization the cap exists to remove", w)
	}
}

// Releasing hands the slot straight back, and does not push next-allowed-at.
func TestClusterConcurrencyReleaseFreesSlot(t *testing.T) {
	l, m, _ := newFixture(t, Config{ClusterConcurrency: 1})
	_, nextKey, inflightKey := l.keys("example.com")

	waits, nonces := bookN(t, l, m, "example.com", 2)
	if waits[0] != 0 || waits[1] == 0 {
		t.Fatalf("setup: got waits %v, want the second refused", waits)
	}

	l.release(nextKey, inflightKey, nonces[0])

	if w := book(t, l, m, "example.com"); w != 0 {
		t.Fatalf("after release: got wait %v, want 0", w)
	}
	// No MinDelay configured, so a release must not have invented a hold.
	if m.Exists(nextKey) {
		t.Fatal("release wrote next-allowed-at under a cluster cap; that is the serialization the cap avoids")
	}
}

// A pod that dies mid-request never releases. The lease deadline is what returns
// its slot, so the cap recovers on its own with no heartbeat.
func TestClusterConcurrencyExpiredLeaseFreesSlot(t *testing.T) {
	const lease = 30 * time.Second
	l, m, c := newFixture(t, Config{ClusterConcurrency: 1, LeaseTTL: lease})

	if w := book(t, l, m, "example.com"); w != 0 {
		t.Fatal("first admission should be free")
	}
	if w := book(t, l, m, "example.com"); w == 0 {
		t.Fatal("second admission should be refused while the slot is held")
	}

	// Nobody released. Walk past the lease deadline.
	c.advance(lease + time.Second)

	if w := book(t, l, m, "example.com"); w != 0 {
		t.Fatalf("after the lease expired: got wait %v, want 0", w)
	}
}

// The other half of lease expiry, and the case that has no safe answer inside
// the limiter: a request that is still ALIVE when its lease expires. The cap is
// exceeded, silently, and the straggler's eventual release ZREMs a nonce that is
// already gone.
//
// This is documented, not prevented. The limiter cannot tell a live slow request
// from a dead pod — that is the price of having no heartbeat. Preventing it is
// the CALLER's job, by sizing LeaseTTL to the longest a slot can be held:
// searxngLeaseTTL in cmd/omnifeed covers the whole retry budget, not one client
// timeout, and this test is what that sizing is defending against.
func TestClusterConcurrencyLeaseExpiryUnderLiveRequestExceedsCap(t *testing.T) {
	const lease = 20 * time.Second
	l, m, c := newFixture(t, Config{ClusterConcurrency: 1, LeaseTTL: lease})
	_, nextKey, inflightKey := l.keys("example.com")

	_, nonces := bookN(t, l, m, "example.com", 1)

	// The request has not finished. Its lease deadline passes anyway.
	c.advance(lease + time.Second)

	if w := book(t, l, m, "example.com"); w != 0 {
		t.Fatalf("second admission: got wait %v, want 0 — this test pins the "+
			"documented overshoot, so a refusal here means the behaviour changed", w)
	}

	// Two requests are now in flight against a cap of 1. The straggler releases
	// late and removes nothing, because the purge already dropped its member.
	l.release(nextKey, inflightKey, nonces[0])
	members, err := m.ZMembers(inflightKey)
	if err != nil {
		t.Fatalf("read in-flight leases: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("after the late release: got %d leases, want the 1 booked by the second caller", len(members))
	}
}

// An upstream Retry-After has to hold every replica back in both modes. The
// penalty rides next-allowed-at, which the acquire script reads whether or not
// the cap is on.
func TestClusterConcurrencyStillHonoursPenalize(t *testing.T) {
	l, m, _ := newFixture(t, Config{ClusterConcurrency: 8})

	if w := book(t, l, m, "example.com"); w != 0 {
		t.Fatal("first admission should be free")
	}

	l.Penalize("https://example.com/x", 2*time.Second)

	w := book(t, l, m, "example.com")
	if w < time.Second {
		t.Fatalf("after a 2s penalty: got wait %v, want roughly the penalty", w)
	}
}

// ClusterConcurrency 0 must leave the semaphore and both scripts exactly as they
// were: no lease key, and a release that bumps next-allowed-at from completion.
func TestClusterConcurrencyOffKeepsLegacyBehaviour(t *testing.T) {
	const delay = 50 * time.Millisecond
	l, m, _ := newFixture(t, Config{MaxConcurrent: 4, MinDelay: delay})
	_, nextKey, inflightKey := l.keys("example.com")

	if w := book(t, l, m, "example.com"); w != 0 {
		t.Fatal("first admission should be free")
	}
	if m.Exists(inflightKey) {
		t.Fatal("wrote an in-flight lease with the cap disabled")
	}

	l.release(nextKey, inflightKey, "")
	if !m.Exists(nextKey) {
		t.Fatal("release did not bump next-allowed-at with the cap disabled")
	}
}

// The per-pod semaphore must not undercut the cluster cap. A pod configured for
// fewer than the cluster allows would bottleneck locally, which looks exactly
// like the cap being broken.
func TestSemaphoreRisesToClusterConcurrency(t *testing.T) {
	l, _, _ := newFixture(t, Config{MaxConcurrent: 1, ClusterConcurrency: 6})
	if got := cap(l.semFor("example.com")); got != 6 {
		t.Fatalf("semaphore capacity: got %d, want 6 (the cluster cap)", got)
	}
}
