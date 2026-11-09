package auth

import "context"

type clientIPKey struct{}

// WithClientIP returns a context carrying the address the request came
// from, as the server determined it (see server.WithTrustedProxies).
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIPFromContext returns the request's client address, or "" if none
// was recorded.
func ClientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}
