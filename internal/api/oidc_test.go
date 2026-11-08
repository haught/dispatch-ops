package api_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"dispatch-ops/internal/api"
	"dispatch-ops/internal/oidc"
)

// fakeIDP is a minimal OIDC provider. login decides who signs in next.
type fakeIDP struct {
	t      *testing.T
	srv    *httptest.Server
	signer jose.Signer
	jwks   jose.JSONWebKeySet

	mu sync.Mutex
	// codes maps issued authorization codes to their sign-in.
	codes map[string]idpLogin
}

type idpLogin struct {
	claims    map[string]any
	challenge string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIDP{
		t: t, signer: signer, codes: map[string]idpLogin{},
		jwks: jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.srv.URL,
			"authorization_endpoint":                idp.srv.URL + "/authorize",
			"token_endpoint":                        idp.srv.URL + "/token",
			"jwks_uri":                              idp.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(idp.jwks)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		idp.mu.Lock()
		l, ok := idp.codes[r.Form.Get("code")]
		delete(idp.codes, r.Form.Get("code"))
		idp.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != l.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		claims := map[string]any{"iss": idp.srv.URL, "aud": "dispatch", "exp": time.Now().Add(time.Hour).Unix(),
			"iat": time.Now().Unix()}
		for k, v := range l.claims {
			claims[k] = v
		}
		payload, _ := json.Marshal(claims)
		sig, err := idp.signer.Sign(payload)
		if err != nil {
			t.Error(err)
			return
		}
		raw, _ := sig.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": raw,
		})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

// signIn runs the browser's side of a sign-in: start at the API, "sign in"
// at the provider with claims, and come back to the callback. It returns
// the callback's response.
func (idp *fakeIDP) signIn(base string, claims map[string]any) *http.Response {
	t := idp.t
	t.Helper()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := noRedirect.Get(base + "/api/v1/auth/oidc/login?next=/jobs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login start = %d", resp.StatusCode)
	}
	authURL, _ := url.Parse(resp.Header.Get("Location"))
	q := authURL.Query()
	if !strings.HasPrefix(authURL.String(), idp.srv.URL+"/authorize") || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("login redirects to %s", authURL)
	}
	var stateCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "dispatch_oidc" {
			stateCookie = c
		}
	}
	if stateCookie == nil || !stateCookie.HttpOnly {
		t.Fatalf("no HttpOnly state cookie: %v", resp.Cookies())
	}

	code := rand.Text()
	all := map[string]any{"nonce": q.Get("nonce")}
	for k, v := range claims {
		all[k] = v
	}
	idp.mu.Lock()
	idp.codes[code] = idpLogin{claims: all, challenge: q.Get("code_challenge")}
	idp.mu.Unlock()

	req, _ := http.NewRequest("GET", base+"/api/v1/auth/oidc/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), nil)
	req.AddCookie(stateCookie)
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func sessionFrom(t *testing.T, base string, resp *http.Response) *client {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == api.SessionCookieName && c.Value != "" {
			return &client{t: t, base: base, cookie: c}
		}
	}
	t.Fatalf("no session cookie; redirected to %s", resp.Header.Get("Location"))
	return nil
}

func TestOIDCSignIn(t *testing.T) {
	idp := newFakeIDP(t)
	srv, _, username, password := startServerWith(t, func(h *api.Handlers) {
		h.OIDC = oidc.New(oidc.Config{
			Issuer: idp.srv.URL, ClientID: "dispatch", ClientSecret: "s", RedirectURL: "http://dispatch.test/cb",
			Scopes: []string{"profile", "email", "groups"}, UsernameClaim: "preferred_username",
			GroupsClaim: "groups", DisplayName: "Corp SSO",
		})
	})
	admin := login(t, srv.URL, username, password)
	cfg := (&client{t: t, base: srv.URL}).mustDo("GET", "/api/v1/auth/config", nil, 200)
	if cfg["oidcEnabled"] != true || cfg["oidcDisplayName"] != "Corp SSO" {
		t.Errorf("auth config = %v", cfg)
	}

	// Members of the "ops" group join the team, and get its roles.
	group := unique("ops")
	team := admin.mustDo("POST", "/api/v1/teams", map[string]any{"name": unique("ops"), "oidcGroups": []string{group}}, 201)
	teamID := team["id"].(string)
	inv := admin.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)
	grant(admin, map[string]any{"role": "read", "resourceType": "inventory", "resourceId": inv["id"], "teamId": teamID})

	subject := unique("sub")
	name := unique("alice")
	claims := map[string]any{
		"sub": subject, "preferred_username": name, "name": "Alice", "email": "alice@example.com",
		"groups": []string{group, "unrelated"},
	}
	resp := idp.signIn(srv.URL, claims)
	if loc := resp.Header.Get("Location"); loc != "/jobs" {
		t.Fatalf("callback redirects to %q", loc)
	}
	alice := sessionFrom(t, srv.URL, resp)
	me := alice.mustDo("GET", "/api/v1/me", nil, 200)
	if me["username"] != name || me["authSource"] != "oidc" || me["displayName"] != "Alice" || me["email"] != "alice@example.com" {
		t.Errorf("me = %v", me)
	}
	alice.mustDo("GET", "/api/v1/inventories/"+inv["id"].(string), nil, 200)
	members := admin.mustDo("GET", "/api/v1/teams/"+teamID+"/members", nil, 200)["items"].([]any)
	if len(members) != 1 || members[0].(map[string]any)["source"] != "oidc" {
		t.Errorf("team members = %v", members)
	}

	// Signing in again is the same user, with the new profile; leaving the
	// group leaves the team.
	claims["name"] = "Alice A."
	claims["groups"] = []string{"unrelated"}
	alice = sessionFrom(t, srv.URL, idp.signIn(srv.URL, claims))
	me2 := alice.mustDo("GET", "/api/v1/me", nil, 200)
	if me2["id"] != me["id"] || me2["displayName"] != "Alice A." {
		t.Errorf("second sign-in: %v", me2)
	}
	alice.mustDo("GET", "/api/v1/inventories/"+inv["id"].(string), nil, 404)

	// A manual membership survives sign-in.
	admin.mustDo("PUT", "/api/v1/teams/"+teamID+"/members/"+me["id"].(string), nil, 204)
	alice = sessionFrom(t, srv.URL, idp.signIn(srv.URL, claims))
	alice.mustDo("GET", "/api/v1/inventories/"+inv["id"].(string), nil, 200)

	// A username that belongs to a local account is refused.
	resp = idp.signIn(srv.URL, map[string]any{"sub": unique("sub"), "preferred_username": username})
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login?error=") || !strings.Contains(loc, "another+account") {
		t.Errorf("conflicting username redirects to %q", loc)
	}

	// Deactivated users can't sign in.
	admin.mustDo("PATCH", "/api/v1/users/"+me["id"].(string), map[string]any{"isActive": false}, 200)
	resp = idp.signIn(srv.URL, claims)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "deactivated") {
		t.Errorf("deactivated user redirects to %q", loc)
	}

	// A callback without the state cookie (e.g. a forged link) is refused.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(srv.URL + "/api/v1/auth/oidc/callback?code=x&state=y")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != 302 || !strings.Contains(loc, "expired") {
		t.Errorf("forged callback = %d %q", resp.StatusCode, loc)
	}

	// Sign-ins are in the audit log.
	log := admin.mustDo("GET", "/api/v1/audit-log?action=login&resourceId="+me["id"].(string), nil, 200)
	if log["total"].(float64) != 3 || log["items"].([]any)[0].(map[string]any)["authMethod"] != "oidc" {
		t.Errorf("OIDC sign-ins in the audit log: %v", log)
	}
}

func TestOIDCDisabled(t *testing.T) {
	srv, _, _ := testServer(t)
	anon := &client{t: t, base: srv.URL}
	if cfg := anon.mustDo("GET", "/api/v1/auth/config", nil, 200); cfg["oidcEnabled"] != false {
		t.Errorf("auth config = %v", cfg)
	}
	anon.mustDo("GET", "/api/v1/auth/oidc/login", nil, 404)
}
