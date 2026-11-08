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

type jobKey struct{}

// WithJob returns a context carrying the job whose runner token
// authenticated the request.
func WithJob(ctx context.Context, jobID uuid.UUID) context.Context {
	return context.WithValue(ctx, jobKey{}, jobID)
}

// JobFromContext returns the job a runner request authenticated as.
func JobFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(jobKey{}).(uuid.UUID)
	return id, ok
}
