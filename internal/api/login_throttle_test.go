package api_test

import (
	"context"
	"crypto/rand"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"dispatch-ops/internal/api"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/store"
	"dispatch-ops/internal/testdb"
)

func TestLoginThrottlesByUsername(t *testing.T) {
	srv, st, username, password := startServerWith(t, func(h *api.Handlers) {
		h.LoginLimits = store.LoginLimits{Window: time.Minute, MaxPerUsername: 3}
	})
	anon := &client{t: t, base: srv.URL}
	login := func(user, pw string) (*http.Response, map[string]any) {
		return anon.do("POST", "/api/v1/auth/login", map[string]string{"username": user, "password": pw})
	}

	// A success clears earlier failures.
	for range 2 {
		resp, body := login(username, "wrong password")
		expectStatus(t, resp, body, http.StatusUnauthorized)
	}
	resp, body := login(username, password)
	expectStatus(t, resp, body, http.StatusOK)

	for range 3 {
		resp, body := login(username, "wrong password")
		expectStatus(t, resp, body, http.StatusUnauthorized)
	}
	// Now even the right password is turned away, in any letter case.
	for _, u := range []string{username, strings.ToUpper(username)} {
		resp, body = login(u, password)
		expectStatus(t, resp, body, http.StatusTooManyRequests)
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || s < 1 || s > 60 {
			t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
		}
	}

	// Another account is unaffected.
	other := "user-" + rand.Text()[:10]
	hash, _ := auth.HashPassword(password)
	if _, err := st.CreateLocalUser(context.Background(), store.NewLocalUser{Username: other, PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	resp, body = login(other, password)
	expectStatus(t, resp, body, http.StatusOK)

	// Unknown usernames are throttled the same way, so throttling doesn't
	// reveal which exist.
	ghost := "ghost-" + rand.Text()[:10]
	for range 3 {
		resp, body := login(ghost, "x")
		expectStatus(t, resp, body, http.StatusUnauthorized)
	}
	resp, body = login(ghost, "x")
	expectStatus(t, resp, body, http.StatusTooManyRequests)
}

func TestLoginThrottlesByClientIP(t *testing.T) {
	pool, _ := testdb.Open(t, "api")
	if _, err := pool.Exec(context.Background(), `DELETE FROM login_failures WHERE client_ip = '127.0.0.1'`); err != nil {
		t.Fatal(err)
	}
	srv, _, username, password := startServerWith(t, func(h *api.Handlers) {
		h.LoginLimits = store.LoginLimits{Window: time.Minute, MaxPerIP: 3}
	})
	anon := &client{t: t, base: srv.URL}
	for range 3 {
		resp, body := anon.do("POST", "/api/v1/auth/login", map[string]string{"username": "ghost-" + rand.Text()[:10], "password": "x"})
		expectStatus(t, resp, body, http.StatusUnauthorized)
	}
	resp, body := anon.do("POST", "/api/v1/auth/login", map[string]string{"username": username, "password": password})
	expectStatus(t, resp, body, http.StatusTooManyRequests)
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM login_failures WHERE client_ip = '127.0.0.1'`); err != nil {
		t.Fatal(err)
	}
}
