// Package oidc signs users in with an OpenID Connect provider, using the
// authorization code flow with PKCE.
package oidc

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config configures the provider.
type Config struct {
	// Issuer is the provider's issuer URL, where its discovery document is.
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL is the callback URL registered with the provider.
	RedirectURL string
	// Scopes are requested in addition to openid.
	Scopes []string
	// UsernameClaim names the claim to use as the username. Dots separate
	// the keys of nested claims.
	UsernameClaim string
	// GroupsClaim names the claim listing the user's groups. Dots separate
	// the keys of nested claims (e.g. realm_access.roles).
	GroupsClaim string
	// DisplayName names the provider on the sign-in page.
	DisplayName string
	// HTTPClient is used to talk to the provider. Nil uses a client with a
	// 10s timeout.
	HTTPClient *http.Client
}

// Identity is who the provider says signed in.
type Identity struct {
	Issuer      string
	Subject     string
	Username    string
	DisplayName string
	Email       string
	Groups      []string
}

// Login is the state of one sign-in, kept by the browser between Start and
// Finish.
type Login struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

// Provider signs users in with one OIDC provider. Its discovery document
// is fetched on first use, so the server starts even if the provider is
// unreachable.
type Provider struct {
	cfg    Config
	client *http.Client

	mu       sync.Mutex
	provider *gooidc.Provider
}

// New returns a provider for cfg.
func New(cfg Config) *Provider {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Provider{cfg: cfg, client: client}
}

// DisplayName names the provider for the sign-in page.
func (p *Provider) DisplayName() string { return p.cfg.DisplayName }

// clientContext carries the HTTP client to go-oidc and oauth2.
func (p *Provider) clientContext(ctx context.Context) context.Context {
	return context.WithValue(gooidc.ClientContext(ctx, p.client), oauth2.HTTPClient, p.client)
}

// discover returns the provider, fetching its discovery document if this
// hasn't succeeded yet.
func (p *Provider) discover() (*gooidc.Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return p.provider, nil
	}
	// Not the request's context: go-oidc keeps this one for fetching the
	// provider's signing keys later.
	prov, err := gooidc.NewProvider(p.clientContext(context.Background()), p.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for %s: %w", p.cfg.Issuer, err)
	}
	p.provider = prov
	return prov, nil
}

func (p *Provider) oauth2Config(prov *gooidc.Provider) oauth2.Config {
	return oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		RedirectURL:  p.cfg.RedirectURL,
		Endpoint:     prov.Endpoint(),
		Scopes:       append([]string{gooidc.ScopeOpenID}, p.cfg.Scopes...),
	}
}

// Start begins a sign-in. It returns the provider URL to send the browser
// to, and the state to keep until Finish.
func (p *Provider) Start() (string, Login, error) {
	prov, err := p.discover()
	if err != nil {
		return "", Login{}, err
	}
	l := Login{State: rand.Text(), Nonce: rand.Text(), Verifier: oauth2.GenerateVerifier()}
	cfg := p.oauth2Config(prov)
	return cfg.AuthCodeURL(l.State, gooidc.Nonce(l.Nonce), oauth2.S256ChallengeOption(l.Verifier)), l, nil
}

// ErrLogin is wrapped by errors that mean the sign-in itself was invalid,
// rather than that the provider couldn't be reached.
var ErrLogin = errors.New("sign-in failed")

// Finish completes a sign-in: it exchanges the authorization code and
// verifies the ID token against l.
func (p *Provider) Finish(ctx context.Context, l Login, code string) (Identity, error) {
	prov, err := p.discover()
	if err != nil {
		return Identity{}, err
	}
	ctx = p.clientContext(ctx)
	cfg := p.oauth2Config(prov)
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(l.Verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: exchanging the authorization code: %w", ErrLogin, err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return Identity{}, fmt.Errorf("%w: the provider returned no ID token", ErrLogin)
	}
	idTok, err := prov.Verifier(&gooidc.Config{ClientID: p.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: invalid ID token: %w", ErrLogin, err)
	}
	if idTok.Nonce != l.Nonce {
		return Identity{}, fmt.Errorf("%w: the ID token's nonce does not match", ErrLogin)
	}
	var claims map[string]any
	if err := idTok.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: reading ID token claims: %w", ErrLogin, err)
	}

	// Some providers only put profile and group claims in the userinfo
	// response. Without the groups, the user would leave their OIDC teams,
	// so a userinfo failure fails the sign-in.
	missing := lookup(claims, p.cfg.GroupsClaim) == nil || lookup(claims, p.cfg.UsernameClaim) == nil
	if missing && prov.UserInfoEndpoint() != "" {
		info, err := prov.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		if err != nil {
			return Identity{}, fmt.Errorf("fetching OIDC userinfo: %w", err)
		}
		if info.Subject != idTok.Subject {
			return Identity{}, fmt.Errorf("%w: the userinfo subject does not match the ID token", ErrLogin)
		}
		var extra map[string]any
		if err := info.Claims(&extra); err != nil {
			return Identity{}, fmt.Errorf("%w: reading userinfo claims: %w", ErrLogin, err)
		}
		for k, v := range extra {
			if _, ok := claims[k]; !ok {
				claims[k] = v
			}
		}
	}

	id := Identity{
		Issuer:      idTok.Issuer,
		Subject:     idTok.Subject,
		Username:    firstString(claims, p.cfg.UsernameClaim, "email"),
		DisplayName: firstString(claims, "name"),
		Email:       firstString(claims, "email"),
		Groups:      stringList(lookup(claims, p.cfg.GroupsClaim)),
	}
	if id.Username == "" {
		id.Username = id.Subject
	}
	return id, nil
}

// lookup returns the claim at a dotted path, or nil.
func lookup(claims map[string]any, path string) any {
	if path == "" {
		return nil
	}
	var v any = claims
	for key := range strings.SplitSeq(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		if v, ok = m[key]; !ok {
			return nil
		}
	}
	return v
}

// firstString returns the first of the claims that is a non-empty string.
func firstString(claims map[string]any, paths ...string) string {
	for _, p := range paths {
		if s, ok := lookup(claims, p).(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// stringList reads a groups claim: a list of strings, or one string.
func stringList(v any) []string {
	switch v := v.(type) {
	case string:
		if v != "" {
			return []string{v}
		}
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
