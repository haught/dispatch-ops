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
	if loc := resp.Header.Get("Location"); loc != "/login?error=username_taken" {
		t.Errorf("conflicting username redirects to %q", loc)
	}

	// Deactivated users can't sign in.
	admin.mustDo("PATCH", "/api/v1/users/"+me["id"].(string), map[string]any{"isActive": false}, 200)
	resp = idp.signIn(srv.URL, claims)
	if loc := resp.Header.Get("Location"); loc != "/login?error=deactivated" {
		t.Errorf("deactivated user redirects to %q", loc)
	}

	// A callback without the state cookie (e.g. a forged link) is refused.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(srv.URL + "/api/v1/auth/oidc/callback?code=x&state=y")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != 302 || loc != "/login?error=expired" {
		t.Errorf("forged callback = %d %q", resp.StatusCode, loc)
	}

	// The provider's error text isn't passed on to the sign-in page; only a
	// fixed code is.
	resp, err = noRedirect.Get(srv.URL + "/api/v1/auth/oidc/callback?error=access_denied&error_description=" +
		url.QueryEscape("Your account is suspended, call +1-555-0100"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/login?error=provider_refused" {
		t.Errorf("provider error redirects to %q", loc)
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

// testSSO is a provider configuration for idp.
func testSSO(idp *fakeIDP) *oidc.Provider {
	return oidc.New(oidc.Config{
		Issuer: idp.srv.URL, ClientID: "dispatch", ClientSecret: "s", RedirectURL: "http://dispatch.test/cb",
		Scopes: []string{"profile", "email", "groups"}, UsernameClaim: "preferred_username",
		GroupsClaim: "groups", DisplayName: "Corp SSO",
	})
}

// An administrator can add a single sign-on user before they sign in; the
// first sign-in with that username takes the user over.
func TestOIDCUserAddedBeforeSignIn(t *testing.T) {
	idp := newFakeIDP(t)
	srv, _, username, password := startServerWith(t, func(h *api.Handlers) { h.OIDC = testSSO(idp) })
	admin := login(t, srv.URL, username, password)

	name := unique("carol")
	// Single sign-on users have no password, and local users need one.
	admin.mustDo("POST", "/api/v1/users", map[string]any{"authSource": "oidc", "username": name, "password": "another-long-password"}, 400)
	admin.mustDo("POST", "/api/v1/users", map[string]any{"authSource": "local", "username": name}, 400)
	admin.mustDo("POST", "/api/v1/users", map[string]any{"username": name}, 400)

	added := admin.mustDo("POST", "/api/v1/users", map[string]any{"authSource": "oidc", "username": name, "displayName": "Carol"}, 201)
	id := added["id"].(string)
	if added["authSource"] != "oidc" || added["oidcPending"] != true || added["lastLoginAt"] != nil {
		t.Errorf("added user = %v", added)
	}
	admin.mustDo("POST", "/api/v1/users", map[string]any{"authSource": "oidc", "username": name}, 409)
	// They can't sign in with a password.
	anon := &client{t: t, base: srv.URL}
	anon.mustDo("POST", "/api/v1/auth/login", map[string]string{"username": name, "password": "another-long-password"}, 401)
	admin.mustDo("PATCH", "/api/v1/users/"+id, map[string]any{"password": "another-long-password"}, 400)
	_, bob := newUser(t, srv, admin)
	if u := admin.mustDo("GET", "/api/v1/users/"+bob, nil, 200); u["oidcPending"] != false {
		t.Errorf("local user = %v", u)
	}

	// Roles can be granted in advance, and a mistyped username corrected.
	grant(admin, map[string]any{"role": "auditor", "userId": id})
	admin.mustDo("PATCH", "/api/v1/users/"+id, map[string]any{"username": name + "-typo"}, 200)
	admin.mustDo("PATCH", "/api/v1/users/"+id, map[string]any{"username": name}, 200)

	// A deactivated pending user is refused, and stays pending.
	claims := map[string]any{"sub": unique("sub"), "preferred_username": strings.ToUpper(name), "name": "Carol C.", "email": "carol@example.com"}
	admin.mustDo("PATCH", "/api/v1/users/"+id, map[string]any{"isActive": false}, 200)
	if loc := idp.signIn(srv.URL, claims).Header.Get("Location"); loc != "/login?error=deactivated" {
		t.Errorf("deactivated pending user redirects to %q", loc)
	}
	if u := admin.mustDo("PATCH", "/api/v1/users/"+id, map[string]any{"isActive": true}, 200); u["oidcPending"] != true {
		t.Errorf("after a refused sign-in: %v", u)
	}

	// The first sign-in, whatever the username's case, is the added user.
	carol := sessionFrom(t, srv.URL, idp.signIn(srv.URL, claims))
	me := carol.mustDo("GET", "/api/v1/me", nil, 200)
	if me["id"] != id || me["oidcPending"] != false || me["isAuditor"] != true ||
		me["username"] != strings.ToUpper(name) || me["displayName"] != "Carol C." || me["email"] != "carol@example.com" {
		t.Errorf("me = %v", me)
	}
	// From then on the provider names the user.
	admin.mustDo("PATCH", "/api/v1/users/"+id, map[string]any{"username": name + "-2"}, 400)

	// Later sign-ins go by subject, so a new username keeps the user, and
	// someone else with the old username is a different user.
	claims["preferred_username"] = name + "-renamed"
	carol = sessionFrom(t, srv.URL, idp.signIn(srv.URL, claims))
	if me := carol.mustDo("GET", "/api/v1/me", nil, 200); me["id"] != id || me["username"] != name+"-renamed" {
		t.Errorf("after a rename: %v", me)
	}
	other := sessionFrom(t, srv.URL, idp.signIn(srv.URL, map[string]any{"sub": unique("sub"), "preferred_username": name}))
	if me := other.mustDo("GET", "/api/v1/me", nil, 200); me["id"] == id || me["isAuditor"] != false {
		t.Errorf("a second user with the old username: %v", me)
	}
}

// With OIDCAddedOnly, single sign-on is only for users an administrator
// added.
func TestOIDCAddedUsersOnly(t *testing.T) {
	idp := newFakeIDP(t)
	srv, _, username, password := startServerWith(t, func(h *api.Handlers) {
		h.OIDC = testSSO(idp)
		h.OIDCAddedOnly = true
	})
	admin := login(t, srv.URL, username, password)

	name := unique("dave")
	claims := map[string]any{"sub": unique("sub"), "preferred_username": name}
	if loc := idp.signIn(srv.URL, claims).Header.Get("Location"); loc != "/login?error=not_provisioned" {
		t.Errorf("unknown user redirects to %q", loc)
	}
	if list := admin.mustDo("GET", "/api/v1/users?search="+name, nil, 200); list["total"].(float64) != 0 {
		t.Errorf("a refused sign-in created a user: %v", list)
	}

	added := admin.mustDo("POST", "/api/v1/users", map[string]any{"authSource": "oidc", "username": name}, 201)
	dave := sessionFrom(t, srv.URL, idp.signIn(srv.URL, claims))
	if me := dave.mustDo("GET", "/api/v1/me", nil, 200); me["id"] != added["id"] {
		t.Errorf("me = %v", me)
	}
	// And again, now by subject.
	sessionFrom(t, srv.URL, idp.signIn(srv.URL, claims))
}

// Without single sign-on, there are no single sign-on users to add.
func TestAddOIDCUserNeedsOIDC(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	admin.mustDo("POST", "/api/v1/users", map[string]any{"authSource": "oidc", "username": unique("erin")}, 400)
	user, _ := newUser(t, srv, admin)
	user.mustDo("POST", "/api/v1/users", map[string]any{"authSource": "oidc", "username": unique("erin")}, 403)
}
