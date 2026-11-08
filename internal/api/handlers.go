// Package api implements the HTTP API defined in api/openapi.yaml.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/credentials"
	"dispatch-ops/internal/jobstream"
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
	Hub           *jobstream.Hub
	Log           *slog.Logger
	SessionTTL    time.Duration
	SecureCookies bool
	Version       string
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
	if err := h.Store.RecordLogin(ctx, user.ID); err != nil {
		return nil, err
	}
	now := time.Now()
	user.LastLoginAt = &now
	h.Log.InfoContext(ctx, "signed in", "username", user.Username)

	cookie := h.sessionCookie(secret, expires).String()
	return apigen.Login200JSONResponse{
		Body:    toAPIUser(user),
		Headers: apigen.Login200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

func (h *Handlers) Logout(ctx context.Context, _ apigen.LogoutRequestObject) (apigen.LogoutResponseObject, error) {
	p := auth.FromContext(ctx)
	if p == nil {
		return apigen.Logout401ApplicationProblemPlusJSONResponse{
			UnauthorizedApplicationProblemPlusJSONResponse: unauthorized(),
		}, nil
	}
	if p.Method == auth.MethodSession {
		if err := h.Store.DeleteToken(ctx, p.UserID, p.TokenID, store.TokenKindSession); err != nil && !errors.Is(err, store.ErrNotFound) {
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
	p := auth.FromContext(ctx)
	if p == nil {
		return apigen.ListApiTokens401ApplicationProblemPlusJSONResponse{
			UnauthorizedApplicationProblemPlusJSONResponse: unauthorized(),
		}, nil
	}
	tokens, err := h.Store.ListTokens(ctx, p.UserID, store.TokenKindAPI)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.ApiToken, 0, len(tokens))
	for _, t := range tokens {
		items = append(items, toAPIToken(t))
	}
	return apigen.ListApiTokens200JSONResponse{Items: items}, nil
}

func (h *Handlers) CreateApiToken(ctx context.Context, req apigen.CreateApiTokenRequestObject) (apigen.CreateApiTokenResponseObject, error) {
	p := auth.FromContext(ctx)
	if p == nil {
		return apigen.CreateApiToken401ApplicationProblemPlusJSONResponse{
			UnauthorizedApplicationProblemPlusJSONResponse: unauthorized(),
		}, nil
	}
	if p.Method != auth.MethodSession {
		return apigen.CreateApiToken403ApplicationProblemPlusJSONResponse{
			ForbiddenApplicationProblemPlusJSONResponse: apigen.ForbiddenApplicationProblemPlusJSONResponse(
				problemDetail(http.StatusForbidden, "Forbidden", "API tokens can only be created from a browser session")),
		}, nil
	}

	var expires *time.Time
	if req.Body.ExpiresInDays != nil {
		t := time.Now().AddDate(0, 0, *req.Body.ExpiresInDays)
		expires = &t
	}
	secret, hash, prefix, err := auth.NewToken(auth.APITokenPrefix)
	if err != nil {
		return nil, err
	}
	t, err := h.Store.CreateToken(ctx, store.NewToken{
		UserID: p.UserID, Kind: store.TokenKindAPI, Name: req.Body.Name, Hash: hash, Prefix: prefix, ExpiresAt: expires,
	})
	if err != nil {
		return nil, err
	}
	h.Log.InfoContext(ctx, "API token created", "username", p.Username, "token_id", t.ID)
	at := toAPIToken(t)
	return apigen.CreateApiToken201JSONResponse{
		Id: at.Id, Name: at.Name, Prefix: at.Prefix, CreatedAt: at.CreatedAt, ExpiresAt: at.ExpiresAt,
		LastUsedAt: at.LastUsedAt, Token: secret,
	}, nil
}

func (h *Handlers) DeleteApiToken(ctx context.Context, req apigen.DeleteApiTokenRequestObject) (apigen.DeleteApiTokenResponseObject, error) {
	p := auth.FromContext(ctx)
	if p == nil {
		return apigen.DeleteApiToken401ApplicationProblemPlusJSONResponse{
			UnauthorizedApplicationProblemPlusJSONResponse: unauthorized(),
		}, nil
	}
	err := h.Store.DeleteToken(ctx, p.UserID, req.TokenId, store.TokenKindAPI)
	if errors.Is(err, store.ErrNotFound) {
		return apigen.DeleteApiToken404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: apigen.NotFoundApplicationProblemPlusJSONResponse(
				problem(http.StatusNotFound, "Token not found")),
		}, nil
	}
	if err != nil {
		return nil, err
	}
	h.Log.InfoContext(ctx, "API token revoked", "username", p.Username, "token_id", req.TokenId)
	return apigen.DeleteApiToken204Response{}, nil
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
		CreatedAt:   u.CreatedAt,
		LastLoginAt: u.LastLoginAt,
	}
}

func toAPIToken(t store.Token) apigen.ApiToken {
	return apigen.ApiToken{
		Id:         t.ID,
		Name:       t.Name,
		Prefix:     t.Prefix,
		CreatedAt:  t.CreatedAt,
		ExpiresAt:  t.ExpiresAt,
		LastUsedAt: t.LastUsedAt,
	}
}
