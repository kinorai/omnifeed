// Package auth defines an Authenticator interface for inbound transports.
// v0.1 ships a single shared bearer-token implementation, but the interface
// leaves room for multi-key, JWT, or DB-backed implementations later without
// touching transport code.
package auth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// TenantID identifies the caller. Single-tenant deployments use "self".
type TenantID string

// SelfTenant is the default tenant used in single-tenant deployments.
const SelfTenant TenantID = "self"

var (
	// ErrUnauthenticated is returned when no valid credential is present.
	ErrUnauthenticated = errors.New("unauthenticated")
)

// Authenticator validates an HTTP request and returns the caller's tenant.
type Authenticator interface {
	Authenticate(r *http.Request) (TenantID, error)
}

// AlwaysAllow is the dev-mode authenticator: no credential required, every
// request runs as the self-tenant. Intended for local docker-run tryouts.
type AlwaysAllow struct{}

// Authenticate always returns SelfTenant and no error.
func (AlwaysAllow) Authenticate(*http.Request) (TenantID, error) { return SelfTenant, nil }

// SharedBearer accepts a single shared bearer token and compares it in
// constant time to avoid timing side-channels.
type SharedBearer struct {
	expected []byte
}

// NewSharedBearer returns an authenticator that accepts exactly the given
// token. If token is empty, callers should use AlwaysAllow instead.
func NewSharedBearer(token string) *SharedBearer {
	return &SharedBearer{expected: []byte(token)}
}

// Authenticate validates the Authorization: Bearer <token> header against
// the configured shared token in constant time.
func (s *SharedBearer) Authenticate(r *http.Request) (TenantID, error) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", ErrUnauthenticated
	}
	got := []byte(strings.TrimPrefix(h, prefix))
	if subtle.ConstantTimeCompare(got, s.expected) != 1 {
		return "", ErrUnauthenticated
	}
	return SelfTenant, nil
}
