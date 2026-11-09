package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"dispatch-ops/internal/auth"
)

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	for _, c := range []struct {
		remote string
		xff    []string
		want   string
	}{
		// Not from a trusted proxy: the header is ignored.
		{"203.0.113.5:1234", []string{"198.51.100.1"}, "203.0.113.5"},
		{"[::ffff:203.0.113.5]:1234", nil, "203.0.113.5"},
		// From a trusted proxy: the rightmost untrusted hop is the client,
		// so a forged leftmost entry doesn't help.
		{"10.1.2.3:80", []string{"198.51.100.1"}, "198.51.100.1"},
		{"10.1.2.3:80", []string{"6.6.6.6, 198.51.100.1, 10.9.9.9"}, "198.51.100.1"},
		{"10.1.2.3:80", []string{"6.6.6.6", "198.51.100.1"}, "198.51.100.1"},
		{"[::1]:80", []string{"198.51.100.1"}, "198.51.100.1"},
		// All hops trusted: the leftmost.
		{"10.1.2.3:80", []string{"10.4.4.4"}, "10.4.4.4"},
		// No header, or a malformed hop: the last address believed.
		{"10.1.2.3:80", nil, "10.1.2.3"},
		{"10.1.2.3:80", []string{"6.6.6.6, garbage"}, "10.1.2.3"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := ClientIP(r, trusted); got != c.want {
			t.Errorf("ClientIP(%s, %q) = %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.2.3:80"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := ClientIP(r, nil); got != "10.1.2.3" {
		t.Errorf("with no trusted proxies, ClientIP = %s", got)
	}
}

func TestWithClientIPSetsContext(t *testing.T) {
	var got string
	h := withClientIP(nil, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = auth.ClientIPFromContext(r.Context())
	}))
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.7:5555"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "192.0.2.7" {
		t.Errorf("client IP in context = %q", got)
	}
}

func TestLimitBodyReadTime(t *testing.T) {
	const limit = 200 * time.Millisecond
	type result struct {
		readErr error
		ctxErr  error
	}
	results := make(chan result, 1)
	srv := httptest.NewServer(limitBodyReadTime(limit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err == nil {
			// The body is in; a slow handler (or a stream) must keep its
			// context past the body deadline.
			time.Sleep(2 * limit)
		}
		results <- result{err, r.Context().Err()}
		w.WriteHeader(http.StatusNoContent)
	})))
	defer srv.Close()

	// A complete body, then a handler that outlives the deadline.
	resp, err := http.Post(srv.URL, "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if r := <-results; r.readErr != nil || r.ctxErr != nil {
		t.Errorf("fast body: read err %v, ctx err %v", r.readErr, r.ctxErr)
	}

	// A body that never finishes arriving.
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\npartial")
	select {
	case r := <-results:
		if r.readErr == nil {
			t.Error("a stalled body was read without error")
		}
	case <-time.After(10 * limit):
		t.Fatal("a stalled body wasn't cut off")
	}

	// A request without a body gets no deadline at all.
	resp, err = http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if r := <-results; r.readErr != nil || r.ctxErr != nil {
		t.Errorf("no body: read err %v, ctx err %v", r.readErr, r.ctxErr)
	}
}
