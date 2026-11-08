package auth

import (
	"context"

	"github.com/google/uuid"
)

// Method is how a request authenticated.
type Method string

const (
	MethodSession  Method = "session"
	MethodAPIToken Method = "api_token"
)

// Principal is the authenticated caller of a request.
type Principal struct {
	UserID   uuid.UUID
	Username string
	IsAdmin  bool
	Method   Method
	// TokenID is the session or API token used, so a session can be ended.
	TokenID uuid.UUID
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the request's principal, or nil if it is anonymous.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}
