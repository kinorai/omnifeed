package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestKindForStatus(t *testing.T) {
	cases := map[int]FailureKind{
		403: KindHTTP403,
		429: KindHTTP429,
		500: KindUpstreamError,
		503: KindUpstreamError,
		504: KindTimeout,
		404: KindError,
		200: KindError,
	}
	for code, want := range cases {
		if got := KindForStatus(code); got != want {
			t.Errorf("KindForStatus(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestFetchErrorUnwrap(t *testing.T) {
	inner := errors.New("boom")
	fe := &FetchError{Kind: KindHTTP403, StatusCode: 403, Err: inner}

	if !errors.Is(fe, inner) {
		t.Error("errors.Is should find the wrapped error")
	}
	var target *FetchError
	if !errors.As(fmt.Errorf("ctx: %w", fe), &target) {
		t.Fatal("errors.As should find FetchError through a wrap")
	}
	if target.Kind != KindHTTP403 {
		t.Errorf("Kind = %q, want http_403", target.Kind)
	}
	// Error() must embed the underlying error so existing substring-based
	// expectations (and logs) keep working.
	if got := fe.Error(); got != "http_403: boom" {
		t.Errorf("Error() = %q, want %q", got, "http_403: boom")
	}
}

// NoFallback keeps the message and the FetchError reachable, and wrapping it
// further (fmt.Errorf %w) does not lose the mark.
func TestNoFallback(t *testing.T) {
	fe := &FetchError{Kind: KindHTTP429, StatusCode: 429}
	err := fmt.Errorf("crawl: %w", NoFallback(fe))
	if !IsNoFallback(err) {
		t.Fatal("IsNoFallback = false through a %w wrap")
	}
	var got *FetchError
	if !errors.As(err, &got) || got.Kind != KindHTTP429 {
		t.Fatalf("FetchError not reachable through NoFallback: %v", err)
	}
	if err.Error() != "crawl: http_429" {
		t.Fatalf("message = %q, want unchanged", err.Error())
	}
	if IsNoFallback(fe) || NoFallback(nil) != nil {
		t.Fatal("unmarked error reported as final, or NoFallback(nil) != nil")
	}
}
