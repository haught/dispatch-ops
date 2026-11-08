// Package api implements the HTTP API defined in api/openapi.yaml.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/credentials"
	"dispatch-ops/internal/jobstream"
	"dispatch-ops/internal/notify"
	"dispatch-ops/internal/oidc"
	"dispatch-ops/internal/store"
)

// SessionCookieName must match the cookieAuth scheme in api/openapi.yaml.
const SessionCookieName = "dispatch_session"

// Handlers implements apigen.StrictServerInterface.
type Handlers struct {
	Store *store.Store
	// Vault encrypts and decrypts credential values.
	Vault credentials.Vault
	// Hub wakes live job streams; it must be running (see jobstream.Hub.Run).
	Hub *jobstream.Hub
	// OIDC signs users in with single sign-on; nil if it isn't configured.
	OIDC          *oidc.Provider
	Log           *slog.Logger
	SessionTTL    time.Duration
	SecureCookies bool
	Version       string
	// Notifier sends test notifications; nil uses a default Sender.
	Notifier *notify.Sender
}

var _ apigen.StrictServerInterface = (*Handlers)(nil)

func (h *Handlers) GetHealth(ctx context.Context, _ apigen.GetHealthRequestObject) (apigen.GetHealthResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	health := apigen.Health{Status: apigen.HealthStatusOk, Database: apigen.HealthDatabaseOk, Version: &h.Version}
	if err := h.Store.Ping(ctx); err != nil {
		h.Log.WarnContext(ctx, "health check: database unavailable", "err", err)
		health.Status, health.Database = apigen.HealthStatusDegraded, apigen.HealthDatabaseUnavailable
		return apigen.GetHealth503JSONResponse(health), nil
	}
	return apigen.GetHealth200JSONResponse(health), nil
}

func (h *Handlers) Login(ctx context.Context, req apigen.LoginRequestObject) (apigen.LoginResponseObject, error) {
	invalid := apigen.Login401ApplicationProblemPlusJSONResponse{
		UnauthorizedApplicationProblemPlusJSONResponse: apigen.UnauthorizedApplicationProblemPlusJSONResponse(
			problem(http.StatusUnauthorized, "Invalid username or password")),
	}

	user, err := h.Store.GetUserByUsername(ctx, req.Body.Username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		auth.BurnPasswordCheck(req.Body.Password)
		return invalid, nil
	case err != nil:
		return nil, err
	}
	if !user.IsActive || user.AuthSource != "local" || user.PasswordHash == nil {
		auth.BurnPasswordCheck(req.Body.Password)
		return invalid, nil
	}
	ok, err := auth.VerifyPassword(req.Body.Password, *user.PasswordHash)
	if err != nil {
		return nil, err
	}
	if !ok {
		h.Log.InfoContext(ctx, "sign-in failed", "username", user.Username)
		return invalid, nil
	}

	cookie, err := h.startSession(ctx, user)
	if err != nil {
		return nil, err
	}
	if err := h.Store.RecordLogin(ctx, user.ID); err != nil {
		return nil, err
	}
	now := time.Now()
	user.LastLoginAt = &now
	h.Log.InfoContext(ctx, "signed in", "username", user.Username)
	h.auditAs(ctx, user.ID, user.Username, "password", auditRecord{
		Action: apigen.Login, ResourceType: apigen.AuditResourceTypeUser, ResourceID: user.ID, ResourceName: user.Username,
	})

	c := cookie.String()
	return apigen.Login200JSONResponse{
		Body:    toAPIUser(user),
		Headers: apigen.Login200ResponseHeaders{SetCookie: &c},
	}, nil
}

// startSession creates a browser session for user and returns its cookie.
func (h *Handlers) startSession(ctx context.Context, user store.User) (*http.Cookie, error) {
	secret, hash, prefix, err := auth.NewToken(auth.SessionTokenPrefix)
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(h.SessionTTL)
	if _, err := h.Store.CreateToken(ctx, store.NewToken{
		UserID: user.ID, Kind: store.TokenKindSession, Hash: hash, Prefix: prefix, ExpiresAt: &expires,
	}); err != nil {
		return nil, err
	}
	return h.sessionCookie(secret, expires), nil
}

func (h *Handlers) GetAuthConfig(_ context.Context, _ apigen.GetAuthConfigRequestObject) (apigen.GetAuthConfigResponseObject, error) {
	out := apigen.GetAuthConfig200JSONResponse{}
	if h.OIDC != nil {
		out.OidcEnabled = true
		out.OidcDisplayName = h.OIDC.DisplayName()
	}
	return out, nil
}

func (h *Handlers) Logout(ctx context.Context, _ apigen.LogoutRequestObject) (apigen.LogoutResponseObject, error) {
	p := auth.FromContext(ctx)
	if p == nil {
		return apigen.Logout401ApplicationProblemPlusJSONResponse{
			UnauthorizedApplicationProblemPlusJSONResponse: unauthorized(),
		}, nil
	}
	if p.Method == auth.MethodSession {
		if _, err := h.Store.DeleteToken(ctx, p.UserID, p.TokenID, store.TokenKindSession); err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	cleared := h.sessionCookie("", time.Unix(0, 0))
	cleared.MaxAge = -1
	s := cleared.String()
	return apigen.Logout204Response{Headers: apigen.Logout204ResponseHeaders{SetCookie: &s}}, nil
}

func (h *Handlers) GetCurrentUser(ctx context.Context, _ apigen.GetCurrentUserRequestObject) (apigen.GetCurrentUserResponseObject, error) {
	p := auth.FromContext(ctx)
	if p == nil {
		return apigen.GetCurrentUser401ApplicationProblemPlusJSONResponse{
			UnauthorizedApplicationProblemPlusJSONResponse: unauthorized(),
		}, nil
	}
	user, err := h.Store.GetUser(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	return apigen.GetCurrentUser200JSONResponse(toAPIUser(user)), nil
}

func (h *Handlers) ListApiTokens(ctx context.Context, _ apigen.ListApiTokensRequestObject) (apigen.ListApiTokensResponseObject, error) {
	items, err := h.listTokens(ctx, caller(ctx).UserID)
	if err != nil {
		return nil, err
	}
	return apigen.ListApiTokens200JSONResponse{Items: items}, nil
}

func (h *Handlers) CreateApiToken(ctx context.Context, req apigen.CreateApiTokenRequestObject) (apigen.CreateApiTokenResponseObject, error) {
	p := caller(ctx)
	user, err := h.Store.GetUser(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	t, err := h.createToken(ctx, user, *req.Body)
	if err != nil {
		return nil, err
	}
	return apigen.CreateApiToken201JSONResponse(t), nil
}

func (h *Handlers) DeleteApiToken(ctx context.Context, req apigen.DeleteApiTokenRequestObject) (apigen.DeleteApiTokenResponseObject, error) {
	p := caller(ctx)
	if err := h.deleteToken(ctx, p.UserID, p.Username, req.TokenId); err != nil {
		return nil, err
	}
	return apigen.DeleteApiToken204Response{}, nil
}

func (h *Handlers) ListUserApiTokens(ctx context.Context, req apigen.ListUserApiTokensRequestObject) (apigen.ListUserApiTokensResponseObject, error) {
	if err := requireAuditor(ctx); err != nil {
		return nil, err
	}
	if _, err := h.Store.GetUser(ctx, req.UserId); err != nil {
		return nil, orNotFound(err, "User")
	}
	items, err := h.listTokens(ctx, req.UserId)
	if err != nil {
		return nil, err
	}
	return apigen.ListUserApiTokens200JSONResponse{Items: items}, nil
}

func (h *Handlers) CreateUserApiToken(ctx context.Context, req apigen.CreateUserApiTokenRequestObject) (apigen.CreateUserApiTokenResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	user, err := h.Store.GetUser(ctx, req.UserId)
	if err != nil {
		return nil, orNotFound(err, "User")
	}
	t, err := h.createToken(ctx, user, *req.Body)
	if err != nil {
		return nil, err
	}
	return apigen.CreateUserApiToken201JSONResponse(t), nil
}

func (h *Handlers) DeleteUserApiToken(ctx context.Context, req apigen.DeleteUserApiTokenRequestObject) (apigen.DeleteUserApiTokenResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	user, err := h.Store.GetUser(ctx, req.UserId)
	if err != nil {
		return nil, orNotFound(err, "User")
	}
	if err := h.deleteToken(ctx, user.ID, user.Username, req.TokenId); err != nil {
		return nil, err
	}
	return apigen.DeleteUserApiToken204Response{}, nil
}

func (h *Handlers) listTokens(ctx context.Context, userID uuid.UUID) ([]apigen.ApiToken, error) {
	tokens, err := h.Store.ListTokens(ctx, userID, store.TokenKindAPI)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.ApiToken, 0, len(tokens))
	for _, t := range tokens {
		items = append(items, toAPIToken(t))
	}
	return items, nil
}

// createToken creates an API token for user. Only browser sessions may
// create tokens, so a leaked token can't be used to mint more.
func (h *Handlers) createToken(ctx context.Context, user store.User, b apigen.CreateApiTokenRequest) (apigen.CreatedApiToken, error) {
	if caller(ctx).Method != auth.MethodSession {
		return apigen.CreatedApiToken{}, forbidden("API tokens can only be created from a browser session")
	}
	if !user.IsActive {
		return apigen.CreatedApiToken{}, badRequest("user %q is deactivated", user.Username)
	}
	var expires *time.Time
	if b.ExpiresInDays != nil {
		t := time.Now().AddDate(0, 0, *b.ExpiresInDays)
		expires = &t
	}
	scope := store.ScopeWrite
	if b.Scope != nil {
		scope = store.TokenScope(*b.Scope)
	}
	secret, hash, prefix, err := auth.NewToken(auth.APITokenPrefix)
	if err != nil {
		return apigen.CreatedApiToken{}, err
	}
	t, err := h.Store.CreateToken(ctx, store.NewToken{
		UserID: user.ID, Kind: store.TokenKindAPI, Name: b.Name, Scope: scope, Hash: hash, Prefix: prefix, ExpiresAt: expires,
	})
	if err != nil {
		return apigen.CreatedApiToken{}, err
	}
	h.Log.InfoContext(ctx, "API token created", "username", user.Username, "token_id", t.ID, "by", principalName(ctx))
	at := toAPIToken(t)
	h.audit(ctx, auditRecord{
		Action: apigen.Create, ResourceType: apigen.AuditResourceTypeApiToken, ResourceID: t.ID, ResourceName: t.Name,
		After: tokenAudit(at, user.Username),
	})
	return apigen.CreatedApiToken{
		Id: at.Id, Name: at.Name, Scope: at.Scope, Prefix: at.Prefix, CreatedAt: at.CreatedAt, ExpiresAt: at.ExpiresAt,
		LastUsedAt: at.LastUsedAt, Token: secret,
	}, nil
}

func (h *Handlers) deleteToken(ctx context.Context, userID uuid.UUID, username string, tokenID uuid.UUID) error {
	t, err := h.Store.DeleteToken(ctx, userID, tokenID, store.TokenKindAPI)
	if err != nil {
		return orNotFound(err, "Token")
	}
	h.Log.InfoContext(ctx, "API token revoked", "username", username, "token_id", tokenID, "by", principalName(ctx))
	h.audit(ctx, auditRecord{
		Action: apigen.Delete, ResourceType: apigen.AuditResourceTypeApiToken, ResourceID: t.ID, ResourceName: t.Name,
		Before: tokenAudit(toAPIToken(t), username),
	})
	return nil
}

// tokenAudit is what the audit log records about a token.
func tokenAudit(t apigen.ApiToken, username string) map[string]any {
	m := toMap(t)
	delete(m, "lastUsedAt")
	m["user"] = username
	return m
}

func (h *Handlers) sessionCookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   h.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

func toAPIUser(u store.User) apigen.User {
	return apigen.User{
		Id:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Email:       u.Email,
		AuthSource:  apigen.UserAuthSource(u.AuthSource),
		IsAdmin:     u.IsAdmin,
		IsAuditor:   u.IsAuditor,
		IsActive:    u.IsActive,
		CreatedAt:   u.CreatedAt,
		LastLoginAt: u.LastLoginAt,
	}
}

func toAPIToken(t store.Token) apigen.ApiToken {
	return apigen.ApiToken{
		Id:         t.ID,
		Name:       t.Name,
		Scope:      apigen.TokenScope(t.Scope),
		Prefix:     t.Prefix,
		CreatedAt:  t.CreatedAt,
		ExpiresAt:  t.ExpiresAt,
		LastUsedAt: t.LastUsedAt,
	}
}
