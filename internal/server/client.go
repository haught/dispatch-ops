package server

import (
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"dispatch-ops/internal/auth"
)

// DefaultBodyReadTimeout bounds how long a client may take to send a request
// body. It is generous: the largest body is a 32 MiB inventory report from a
// sync job inside the cluster.
const DefaultBodyReadTimeout = 2 * time.Minute

// Option configures New.
type Option func(*options)

type options struct {
	trustedProxies  []netip.Prefix
	bodyReadTimeout time.Duration
}

// WithTrustedProxies names the reverse proxies (load balancers, ingress
// controllers) whose X-Forwarded-For header is believed. A request from any
// other address is attributed to that address, whatever it claims.
func WithTrustedProxies(prefixes []netip.Prefix) Option {
	return func(o *options) { o.trustedProxies = prefixes }
}

// WithBodyReadTimeout sets how long a client may take to send a request body
// (default DefaultBodyReadTimeout); 0 turns the limit off.
func WithBodyReadTimeout(d time.Duration) Option {
	return func(o *options) { o.bodyReadTimeout = d }
}

// withClientIP records the request's client address in its context, for
// sign-in throttling.
func withClientIP(trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r, trusted)
		next.ServeHTTP(w, r.WithContext(auth.WithClientIP(r.Context(), ip)))
	})
}

// ClientIP returns the address r came from. X-Forwarded-For is believed only
// as far as it was written by trusted proxies: it is read from the right,
// skipping trusted addresses, and the first untrusted one is the client. So
// a client can't choose its address by sending the header itself.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if !isTrusted(addr, trusted) {
		return addr.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// A malformed entry: stop at the last address we believe.
			break
		}
		addr = hop.Unmap()
		if !isTrusted(addr, trusted) {
			break
		}
	}
	return addr.String()
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// limitBodyReadTime gives a client d to send its request body, so a slow
// client can't hold a connection and its handler indefinitely.
//
// http.Server's ReadTimeout can't be used: it leaves the connection's read
// deadline in place while the handler runs, and when it passes, the server's
// background read fails and cancels the request's context, which would cut
// off job output streams. Here the deadline is set only on requests that
// have a body and is cleared as soon as the body has been read, before the
// server starts its background read.
func limitBodyReadTime(d time.Duration, next http.Handler) http.Handler {
	if d <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody || (r.ContentLength == 0 && len(r.TransferEncoding) == 0) {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(d)); err != nil {
			next.ServeHTTP(w, r)
			return
		}
		body := &deadlineBody{ReadCloser: r.Body, clear: func() { _ = rc.SetReadDeadline(time.Time{}) }}
		r.Body = body
		defer body.done()
		next.ServeHTTP(w, r)
	})
}

// deadlineBody clears the read deadline once the body is used up.
type deadlineBody struct {
	io.ReadCloser
	clear func()
	once  sync.Once
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.done()
	}
	return n, err
}

func (b *deadlineBody) done() { b.once.Do(b.clear) }
