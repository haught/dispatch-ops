package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"dispatch-ops/internal/api"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/credentials"
	"dispatch-ops/internal/jobstream"
	"dispatch-ops/internal/secrets"
	"dispatch-ops/internal/server"
	"dispatch-ops/internal/store"
	"dispatch-ops/internal/testdb"
)

// testServer starts the full HTTP stack against the database named by
// DISPATCH_TEST_DATABASE_URL, and creates a fresh local user.
func testServer(t *testing.T) (srv *httptest.Server, username, password string) {
	t.Helper()
	srv, _, username, password = startServer(t)
	return srv, username, password
}

// startServer is testServer that also returns the store.
func startServer(t *testing.T) (srv *httptest.Server, st *store.Store, username, password string) {
	t.Helper()
	pool, _ := testdb.Open(t, "api")
	ctx := context.Background()
	st = store.New(pool)

	username = "user-" + rand.Text()[:10]
	password = "a-long-test-password"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateLocalUser(ctx, store.NewLocalUser{Username: username, PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	keys, err := secrets.NewKeyring(secrets.GenerateKey())
	if err != nil {
		t.Fatal(err)
	}
	hubCtx, stopHub := context.WithCancel(context.Background())
	t.Cleanup(stopHub)
	hub := jobstream.NewHub()
	go hub.Run(hubCtx, st, log)
	h, err := api.NewHandler(&api.Handlers{
		Store: st, Vault: credentials.Vault{Keys: keys}, Hub: hub, Log: log, SessionTTL: time.Hour, Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ui := fstest.MapFS{"200.html": {Data: []byte("<html>app shell</html>")}}
	srv = httptest.NewServer(server.New(h, st, ui, log))
	t.Cleanup(srv.Close)
	return srv, st, username, password
}

type client struct {
	t    *testing.T
	base string
	// cookie and bearer are sent when set.
	cookie *http.Cookie
	bearer string
}

func (c *client) do(method, path string, body any) (*http.Response, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func expectStatus(t *testing.T, resp *http.Response, body map[string]any, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d (body %v)", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, body)
	}
}

func TestSessionAndTokenLifecycle(t *testing.T) {
	srv, username, password := testServer(t)
	anon := &client{t: t, base: srv.URL}

	// Health is public.
	resp, body := anon.do("GET", "/api/v1/health", nil)
	expectStatus(t, resp, body, 200)
	if body["database"] != "ok" {
		t.Errorf("health database = %v", body["database"])
	}

	// Protected endpoints need credentials, and errors are problem+json.
	resp, body = anon.do("GET", "/api/v1/me", nil)
	expectStatus(t, resp, body, 401)
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("401 content type = %q", ct)
	}

	// Wrong password, and unknown user, both 401.
	resp, body = anon.do("POST", "/api/v1/auth/login", map[string]string{"username": username, "password": "nope"})
	expectStatus(t, resp, body, 401)
	resp, body = anon.do("POST", "/api/v1/auth/login", map[string]string{"username": "no-such-user", "password": "nope"})
	expectStatus(t, resp, body, 401)

	// Spec validation rejects a body missing required fields.
	resp, body = anon.do("POST", "/api/v1/auth/login", map[string]string{"username": username})
	expectStatus(t, resp, body, 400)

	// Sign in; username matching is case-insensitive.
	resp, body = anon.do("POST", "/api/v1/auth/login", map[string]string{"username": strings.ToUpper(username), "password": password})
	expectStatus(t, resp, body, 200)
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == api.SessionCookieName {
			session = c
		}
	}
	if session == nil || !session.HttpOnly {
		t.Fatalf("login did not set an HttpOnly session cookie: %v", resp.Cookies())
	}
	browser := &client{t: t, base: srv.URL, cookie: session}

	resp, body = browser.do("GET", "/api/v1/me", nil)
	expectStatus(t, resp, body, 200)
	if body["username"] != username {
		t.Errorf("me.username = %v, want %s", body["username"], username)
	}

	// Requests are validated against the spec.
	resp, body = browser.do("POST", "/api/v1/tokens", map[string]any{"name": ""})
	expectStatus(t, resp, body, 400)
	resp, body = browser.do("DELETE", "/api/v1/tokens/not-a-uuid", nil)
	expectStatus(t, resp, body, 400)

	// Create an API token from the session.
	resp, body = browser.do("POST", "/api/v1/tokens", map[string]any{"name": "ci", "expiresInDays": 30})
	expectStatus(t, resp, body, 201)
	secret, _ := body["token"].(string)
	tokenID, _ := body["id"].(string)
	if !strings.HasPrefix(secret, auth.APITokenPrefix) {
		t.Fatalf("created token %q lacks prefix", secret)
	}

	// The API token works as a bearer token, and the secret is never listed.
	robot := &client{t: t, base: srv.URL, bearer: secret}
	resp, body = robot.do("GET", "/api/v1/tokens", nil)
	expectStatus(t, resp, body, 200)
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("listed %d tokens, want 1", len(items))
	}
	if _, leaked := items[0].(map[string]any)["token"]; leaked {
		t.Error("token list includes the secret")
	}

	// An API token cannot mint more tokens.
	resp, body = robot.do("POST", "/api/v1/tokens", map[string]any{"name": "escalate"})
	expectStatus(t, resp, body, 403)

	// A session token is not accepted as a bearer token.
	resp, body = (&client{t: t, base: srv.URL, bearer: session.Value}).do("GET", "/api/v1/me", nil)
	expectStatus(t, resp, body, 401)

	// Revoke the token; it stops working.
	resp, body = browser.do("DELETE", "/api/v1/tokens/"+tokenID, nil)
	expectStatus(t, resp, body, 204)
	resp, body = robot.do("GET", "/api/v1/me", nil)
	expectStatus(t, resp, body, 401)
	resp, body = browser.do("DELETE", "/api/v1/tokens/"+tokenID, nil)
	expectStatus(t, resp, body, 404)

	// Sign out ends the session server-side.
	resp, body = browser.do("POST", "/api/v1/auth/logout", nil)
	expectStatus(t, resp, body, 204)
	resp, body = browser.do("GET", "/api/v1/me", nil)
	expectStatus(t, resp, body, 401)
}

func TestRoutingAndCrossOrigin(t *testing.T) {
	srv, _, _ := testServer(t)
	anon := &client{t: t, base: srv.URL}

	// Unknown API routes are 404 problems, not the SPA shell.
	resp, body := anon.do("GET", "/api/v1/nope", nil)
	expectStatus(t, resp, body, 404)

	// Auth is checked before parameters, so anonymous callers learn nothing
	// about request validation.
	resp, body = anon.do("DELETE", "/api/v1/tokens/not-a-uuid", nil)
	expectStatus(t, resp, body, 401)

	// Client-side routes get the SPA fallback page; missing assets 404.
	for path, want := range map[string]int{"/": 200, "/tokens": 200, "/_app/missing.js": 404, "/.gitkeep": 200} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
		if want == 200 && !strings.Contains(string(b), "app shell") {
			t.Errorf("GET %s did not serve the app shell", path)
		}
	}

	// A cross-site browser POST is rejected (CSRF protection).
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/auth/login", strings.NewReader(`{"username":"x","password":"y"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", resp.StatusCode)
	}
}
