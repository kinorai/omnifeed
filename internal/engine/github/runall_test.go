package github

import (
	"errors"
	"strings"
	"testing"
)

// A panicking fan-out call fails the crawl with an error; it must not take the
// process down (it runs outside the request goroutine, where net/http's own
// recover does not reach).
func TestRunAllRecoversPanics(t *testing.T) {
	sentinel := errors.New("second")
	err := runAll(
		func() error { panic("nil deref upstream") },
		func() error { return sentinel },
	)
	if err == nil || !strings.Contains(err.Error(), "panic in github_fanout") {
		t.Fatalf("runAll err = %v, want the recovered panic (first by argument order)", err)
	}
	if err := runAll(func() error { return nil }); err != nil {
		t.Fatalf("runAll(ok) = %v", err)
	}
}
