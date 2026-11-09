package api

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/oidc"
	"dispatch-ops/internal/store"
)

// oidcCookieName holds a sign-in's state between the redirect to the
// provider and the callback; secureOIDCCookieName is its name with secure
// cookies. The __Host- prefix makes browsers refuse the cookie unless this
// host set it over HTTPS with Path=/, so a sibling subdomain or a plain
// HTTP response can't plant one. The state is also encrypted with the
// server's keyring, so it can't be forged or read.
const (
	oidcCookieName       = "dispatch_oidc"
	secureOIDCCookieName = "__Host-" + oidcCookieName
)

// oidcStateAAD binds encrypted OIDC state to its purpose, so no other
// ciphertext made with the keyring is accepted as one.
var oidcStateAAD = []byte("dispatch-ops oidc login state")

type oidcStateKey struct{}

// withOIDCState puts the OIDC state cookie's value, read by the name secure
// cookies call for, in the request context for OidcCallback.
func withOIDCState(secure bool, next http.Handler) http.Handler {
	name := oidcCookieName
	if secure {
		name = secureOIDCCookieName
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(name); err == nil {
			r = r.WithContext(context.WithValue(r.Context(), oidcStateKey{}, c.Value))
		}
		next.ServeHTTP(w, r)
	})
}

// oidcLoginTTL is how long a user has to sign in at the provider.
const oidcLoginTTL = 10 * time.Minute

var errOIDCDisabled = &apiError{http.StatusNotFound, "Not found", "Single sign-on is not configured"}

// oidcLogin is the content of the state cookie.
type oidcLogin struct {
	oidc.Login
	Next string `json:"next"`
	// Expires is when the sign-in expires (Unix seconds), checked on the
	// server rather than trusting the cookie's own expiry.
	Expires int64 `json:"exp"`
}

// sealOIDCState encrypts a sign-in's state for its cookie.
func (h *Handlers) sealOIDCState(login oidcLogin) (string, error) {
	b, err := json.Marshal(login)
	if err != nil {
		return "", err
	}
	ct, keyID, err := h.Vault.Keys.Encrypt(b, oidcStateAAD)
	if err != nil {
		return "", err
	}
	return keyID + "." + base64.RawURLEncoding.EncodeToString(ct), nil
}

// openOIDCState decrypts a state cookie, and reports false if it isn't one
// this server made or it has expired.
func (h *Handlers) openOIDCState(value string) (oidcLogin, bool) {
	var login oidcLogin
	keyID, enc, ok := strings.Cut(value, ".")
	if !ok {
		return login, false
	}
	ct, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return login, false
	}
	b, err := h.Vault.Keys.Decrypt(ct, keyID, oidcStateAAD)
	if err != nil || json.Unmarshal(b, &login) != nil || login.State == "" ||
		time.Now().Unix() > login.Expires {
		return oidcLogin{}, false
	}
	return login, true
}

// redirect is a 302 response that may set cookies. It serves both OIDC
// endpoints, which need to set more than one cookie.
type redirect struct {
	location string
	cookies  []*http.Cookie
}

func (r redirect) write(w http.ResponseWriter) error {
	for _, c := range r.cookies {
		http.SetCookie(w, c)
	}
	w.Header().Set("Location", r.location)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
	return nil
}

func (r redirect) VisitOidcLoginResponse(w http.ResponseWriter) error    { return r.write(w) }
func (r redirect) VisitOidcCallbackResponse(w http.ResponseWriter) error { return r.write(w) }

// safeNext returns next if it is a path in this app, so sign-in can't be
// used as an open redirect, and "/" otherwise.
func safeNext(next string) string {
	if isLocalPath(next) {
		return next
	}
	return "/"
}

// isLocalPath reports whether p is an absolute path (with optional query and
// fragment) on this origin. Browsers drop tabs and newlines from URLs and
// treat "\" like "/", so "/\t/evil.com" and "/\evil.com" would lead to
// evil.com: p must contain neither, nor any other control byte, before or
// after percent-decoding, and must not start with "//".
func isLocalPath(p string) bool {
	decoded, err := url.PathUnescape(p)
	if err != nil {
		return false
	}
	for _, s := range []string{p, decoded} {
		if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.ContainsRune(s, '\\') {
			return false
		}
		for i := 0; i < len(s); i++ {
			if s[i] < 0x20 || s[i] == 0x7f {
				return false
			}
		}
	}
	u, err := url.Parse(p)
	return err == nil && u.Scheme == "" && u.Host == "" && u.User == nil && u.Opaque == "" &&
		strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(u.Path, "//")
}

func (h *Handlers) oidcCookie(value string, maxAge int) *http.Cookie {
	name := oidcCookieName
	if h.SecureCookies {
		name = secureOIDCCookieName
	}
	return &http.Cookie{
		Name:  name,
		Value: value,
		// __Host- cookies must have Path=/.
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   h.SecureCookies,
		// Lax: the provider sends the browser back with a top-level GET.
		SameSite: http.SameSiteLaxMode,
	}
}

func (h *Handlers) OidcLogin(ctx context.Context, req apigen.OidcLoginRequestObject) (apigen.OidcLoginResponseObject, error) {
	if h.OIDC == nil {
		return nil, errOIDCDisabled
	}
	authURL, login, err := h.OIDC.Start()
	if err != nil {
		h.Log.ErrorContext(ctx, "OIDC sign-in could not start", "err", err)
		return redirect{location: loginError("The sign-on provider is unavailable. Try again later.")}, nil
	}
	state, err := h.sealOIDCState(oidcLogin{
		Login: login, Next: safeNext(deref(req.Params.Next)), Expires: time.Now().Add(oidcLoginTTL).Unix(),
	})
	if err != nil {
		return nil, err
	}
	cookie := h.oidcCookie(state, int(oidcLoginTTL.Seconds()))
	return redirect{location: authURL, cookies: []*http.Cookie{cookie}}, nil
}

// loginError is the UI's sign-in page showing msg.
func loginError(msg string) string {
	return "/login?error=" + url.QueryEscape(msg)
}

func (h *Handlers) OidcCallback(ctx context.Context, req apigen.OidcCallbackRequestObject) (apigen.OidcCallbackResponseObject, error) {
	if h.OIDC == nil {
		return nil, errOIDCDisabled
	}
	p := req.Params
	clear := h.oidcCookie("", -1)
	fail := func(msg string) (apigen.OidcCallbackResponseObject, error) {
		return redirect{location: loginError(msg), cookies: []*http.Cookie{clear}}, nil
	}

	if p.Error != nil {
		h.Log.InfoContext(ctx, "OIDC provider returned an error", "error", *p.Error, "description", deref(p.ErrorDescription))
		msg := "Sign-in was refused by the provider: " + *p.Error
		if d := deref(p.ErrorDescription); d != "" {
			msg += " (" + d + ")"
		}
		return fail(msg)
	}
	raw, _ := ctx.Value(oidcStateKey{}).(string)
	login, ok := h.openOIDCState(raw)
	if !ok || subtle.ConstantTimeCompare([]byte(login.State), []byte(deref(p.State))) != 1 {
		return fail("Your sign-in expired or was started in another browser. Try again.")
	}
	if deref(p.Code) == "" {
		return fail("The provider did not return an authorization code.")
	}

	id, err := h.OIDC.Finish(ctx, login.Login, *p.Code)
	if errors.Is(err, oidc.ErrLogin) {
		h.Log.WarnContext(ctx, "OIDC sign-in rejected", "err", err)
		return fail("Sign-in failed. Try again.")
	}
	if err != nil {
		h.Log.ErrorContext(ctx, "OIDC sign-in failed", "err", err)
		return fail("The sign-on provider is unavailable. Try again later.")
	}

	user, err := h.Store.SignInOIDCUser(ctx, store.OIDCIdentity{
		Issuer: id.Issuer, Subject: id.Subject, Username: id.Username, DisplayName: id.DisplayName,
		Email: id.Email, Groups: id.Groups,
	})
	if errors.Is(err, store.ErrConflict) {
		h.Log.WarnContext(ctx, "OIDC username taken", "username", id.Username, "subject", id.Subject)
		return fail(fmt.Sprintf("The username %q belongs to another account. Ask an administrator to rename it.", id.Username))
	}
	if err != nil {
		return nil, err
	}
	if !user.IsActive {
		h.Log.InfoContext(ctx, "OIDC sign-in by deactivated user", "username", user.Username)
		return fail("Your account is deactivated.")
	}

	session, err := h.startSession(ctx, user)
	if err != nil {
		return nil, err
	}
	h.Log.InfoContext(ctx, "signed in", "username", user.Username, "method", "oidc", "groups", id.Groups)
	h.auditAs(ctx, user.ID, user.Username, "oidc", auditRecord{
		Action: apigen.Login, ResourceType: apigen.AuditResourceTypeUser, ResourceID: user.ID, ResourceName: user.Username,
		After: map[string]any{"groups": nonNilSlice(id.Groups)},
	})
	return redirect{location: safeNext(login.Next), cookies: []*http.Cookie{session, clear}}, nil
}
