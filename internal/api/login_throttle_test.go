package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dispatch-ops/internal/api"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/store"
)

// throttleServer starts a server with limits, and returns a function that
// signs in as if from the client address from. Failures persist in the test
// database for the window, so tests use addresses no other run shares.
func throttleServer(t *testing.T, limits store.LoginLimits) (
	signIn func(from, username, password string) (int, http.Header), st *store.Store, admin *client, username, password string,
) {
	t.Helper()
	srv, st, username, password := startServerWith(t, func(h *api.Handlers) { h.LoginLimits = limits })
	admin = login(t, srv.URL, username, password)
	// The server takes the client address from the connection; this
	// front end sets it from a test header.
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if from := r.Header.Get("X-Test-Remote"); from != "" {
			r.RemoteAddr = from
		}
		srv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	signIn = func(from, user, pw string) (int, http.Header) {
		b, _ := json.Marshal(map[string]string{"username": user, "password": pw})
		req, err := http.NewRequest("POST", front.URL+"/api/v1/auth/login", bytes.NewReader(b))
		if err != nil {
			panic(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Remote", from)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			panic(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode, resp.Header
	}
	return signIn, st, admin, username, password
}

// testAddr returns a fresh client address (host:port) from 198.18.0.0/15,
// which is reserved for testing.
func testAddr() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<17))
	v := n.Int64()
	return fmt.Sprintf("198.%d.%d.%d:1234", 18+v>>16, (v>>8)&0xff, v&0xff)
}

// testAddr6 returns a fresh /64 from 2001:db8::/32 as a prefix that hosts
// can be appended to (e.g. testAddr6()+"1").
func testAddr6() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("2001:db8:%x:%x::", uint16(b[0])<<8|uint16(b[1]), uint16(b[2])<<8|uint16(b[3]))
}

func expectCode(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: status %d, want %d", what, got, want)
	}
}

// R2-L1: the tight limit is per username and client address, so a stranger
// can't lock the account's owner out of their own address.
func TestLoginThrottlesByUsernameAndIP(t *testing.T) {
	signIn, st, _, username, password := throttleServer(t, store.LoginLimits{Window: time.Minute, MaxPerUsernameIP: 3})
	attacker, owner := testAddr(), testAddr()

	// A success clears earlier failures.
	for range 2 {
		code, _ := signIn(attacker, username, "wrong password")
		expectCode(t, "wrong password", code, http.StatusUnauthorized)
	}
	code, _ := signIn(attacker, username, password)
	expectCode(t, "right password", code, http.StatusOK)

	for range 3 {
		code, _ := signIn(attacker, username, "wrong password")
		expectCode(t, "wrong password", code, http.StatusUnauthorized)
	}
	// Now even the right password is turned away from that address, in any
	// letter case.
	for _, u := range []string{username, strings.ToUpper(username)} {
		code, h := signIn(attacker, u, password)
		expectCode(t, "right password after the limit", code, http.StatusTooManyRequests)
		if s, err := strconv.Atoi(h.Get("Retry-After")); err != nil || s < 1 || s > 60 {
			t.Errorf("Retry-After = %q", h.Get("Retry-After"))
		}
	}
	// But not from the owner's address.
	code, _ = signIn(owner, username, password)
	expectCode(t, "right password from another address", code, http.StatusOK)

	// Another account is unaffected, even from the attacker's address.
	other := "user-" + rand.Text()[:10]
	hash, _ := auth.HashPassword(password)
	if _, err := st.CreateLocalUser(context.Background(), store.NewLocalUser{Username: other, PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	code, _ = signIn(attacker, other, password)
	expectCode(t, "another account", code, http.StatusOK)

	// Unknown usernames are throttled the same way, so throttling doesn't
	// reveal which exist.
	ghost := "ghost-" + rand.Text()[:10]
	for range 3 {
		code, _ := signIn(attacker, ghost, "x")
		expectCode(t, "unknown user", code, http.StatusUnauthorized)
	}
	code, _ = signIn(attacker, ghost, "x")
	expectCode(t, "unknown user after the limit", code, http.StatusTooManyRequests)
}

// R2-L1: a username's failures from all addresses have a far higher limit,
// which still refuses, and the first refusal is audited, once.
func TestLoginThrottlesByUsernameTotal(t *testing.T) {
	signIn, st, admin, username, password := throttleServer(t, store.LoginLimits{
		Window: time.Minute, MaxPerUsernameIP: 3, MaxPerUsername: 6,
	})
	victim := "user-" + rand.Text()[:10]
	hash, _ := auth.HashPassword(password)
	u, err := st.CreateLocalUser(context.Background(), store.NewLocalUser{Username: victim, PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		from := testAddr()
		for range 3 {
			code, _ := signIn(from, victim, "wrong password")
			expectCode(t, "wrong password", code, http.StatusUnauthorized)
		}
	}
	for range 3 {
		code, _ := signIn(testAddr(), victim, password)
		expectCode(t, "right password from a new address after the total limit", code, http.StatusTooManyRequests)
	}
	// Other accounts are unaffected.
	code, _ := signIn(testAddr(), username, password)
	expectCode(t, "another account", code, http.StatusOK)

	log := admin.mustDo("GET", "/api/v1/audit-log?action=login_throttled&resourceId="+u.ID.String(), nil, 200)
	items := log["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("throttle audit entries = %v", items)
	}
	e := items[0].(map[string]any)
	after, _ := e["after"].(map[string]any)
	if e["resourceName"] != victim || e["actor"] != nil || fmt.Sprint(after["limits"]) != "[username]" {
		t.Errorf("throttle audit entry = %v", e)
	}
}

// The username's total limit doesn't lock its owner out of an address they
// have signed in from: one IPv6 /56 holds enough /64s to reach the total,
// and would otherwise lock the account out everywhere.
func TestLoginTotalLimitSparesKnownClients(t *testing.T) {
	signIn, _, _, username, password := throttleServer(t, store.LoginLimits{
		Window: time.Minute, MaxPerUsernameIP: 3, MaxPerUsername: 6,
	})
	owner := "[" + testAddr6() + "1]:1234"
	code, _ := signIn(owner, username, password)
	expectCode(t, "owner signs in", code, http.StatusOK)

	// The attacker trips the total from /64s of one /56.
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	for i := range 2 {
		// 2001:db8:XXXX:YY00::/56, a /64 each.
		from := fmt.Sprintf("[2001:db8:%x:%x::1]:1234", uint16(b[0])<<8|uint16(b[1]), uint16(b[2])<<8|uint16(i))
		for range 3 {
			code, _ := signIn(from, username, "wrong password")
			expectCode(t, "wrong password", code, http.StatusUnauthorized)
		}
	}
	code, _ = signIn(testAddr(), username, password)
	expectCode(t, "right password from a fresh address", code, http.StatusTooManyRequests)

	// The owner's address (any host in its /64) still gets in.
	known := strings.Replace(owner, "::1]", "::2]", 1)
	for range 2 {
		code, _ = signIn(known, username, password)
		expectCode(t, "right password from the owner's address", code, http.StatusOK)
	}
	// It is still held to the limit per username and address.
	for range 3 {
		code, _ := signIn(known, username, "wrong password")
		expectCode(t, "wrong password from the owner's address", code, http.StatusUnauthorized)
	}
	code, _ = signIn(known, username, password)
	expectCode(t, "owner's address over its own limit", code, http.StatusTooManyRequests)
}

func TestLoginThrottlesByClientIP(t *testing.T) {
	signIn, _, _, username, password := throttleServer(t, store.LoginLimits{Window: time.Minute, MaxPerIP: 3})
	from := testAddr()
	for range 3 {
		code, _ := signIn(from, "ghost-"+rand.Text()[:10], "x")
		expectCode(t, "unknown user", code, http.StatusUnauthorized)
	}
	code, h := signIn(from, username, password)
	expectCode(t, "after the address's limit", code, http.StatusTooManyRequests)
	if h.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	code, _ = signIn(testAddr(), username, password)
	expectCode(t, "another address", code, http.StatusOK)
}

// R2-L2: IPv6 clients are counted by /64, so one site can't use each of its
// addresses for a fresh limit.
func TestLoginThrottlesIPv6ByPrefix(t *testing.T) {
	signIn, _, _, username, password := throttleServer(t, store.LoginLimits{Window: time.Minute, MaxPerIP: 3})
	net64 := testAddr6()
	for i := range 3 {
		code, _ := signIn(fmt.Sprintf("[%s%x]:1234", net64, i+1), "ghost-"+rand.Text()[:10], "x")
		expectCode(t, "unknown user", code, http.StatusUnauthorized)
	}
	code, _ := signIn("["+net64+"ffff:1:2]:1234", username, password)
	expectCode(t, "another address in the /64", code, http.StatusTooManyRequests)
	code, _ = signIn("["+testAddr6()+"1]:1234", username, password)
	expectCode(t, "another /64", code, http.StatusOK)
}

// R2-M1: concurrent attempts can't all pass the check before any of them
// is recorded. Each attempt is counted before its password is checked.
func TestLoginThrottleIsAtomic(t *testing.T) {
	const limit, attempts = 10, 100
	signIn, _, _, username, _ := throttleServer(t, store.LoginLimits{Window: time.Minute, MaxPerUsernameIP: limit})
	from := testAddr()
	var (
		mu    sync.Mutex
		codes = map[int]int{}
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	for range attempts {
		wg.Go(func() {
			<-start
			code, _ := signIn(from, username, "wrong password")
			mu.Lock()
			codes[code]++
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	// Every 401 is a password that was checked.
	if codes[http.StatusUnauthorized] > limit || codes[http.StatusUnauthorized]+codes[http.StatusTooManyRequests] != attempts {
		t.Errorf("statuses = %v; want at most %d checked (401) and the rest refused (429)", codes, limit)
	}
}
