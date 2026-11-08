package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3filter"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/store"
)

// maxBodyBytes caps API request bodies.
const maxBodyBytes = 1 << 20

// NewHandler returns the http.Handler serving everything under /api/ and
// /runner/.
//
// Request flow: identify the caller (cookie, API token or job token) → validate the
// request against the OpenAPI spec, which also enforces each operation's
// `security` requirements → generated router → strict handlers.
func NewHandler(h *Handlers) (http.Handler, error) {
	spec, err := apigen.GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("load OpenAPI spec: %w", err)
	}
	spec.Servers = nil

	validate := nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
		Options: openapi3filter.Options{
			AuthenticationFunc: authenticate,
		},
		ErrorHandlerWithOpts:  validationErrorHandler(h.Log),
		SilenceServersWarning: true,
		DoNotValidateServers:  true,
	})

	strict := apigen.NewStrictHandlerWithOptions(h, nil, apigen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeProblem(w, problemDetail(http.StatusBadRequest, "Invalid request", err.Error()))
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			if p, ok := problemFor(err); ok {
				writeProblem(w, p)
				return
			}
			if r.Context().Err() != nil {
				return // the client went away
			}
			h.Log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			writeProblem(w, problem(http.StatusInternalServerError, "Internal server error"))
		},
	})

	mux := http.NewServeMux()
	apigen.HandlerWithOptions(strict, apigen.StdHTTPServerOptions{
		BaseRouter: mux,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeProblem(w, problemDetail(http.StatusBadRequest, "Invalid request", err.Error()))
		},
	})

	return identify(h.Store, h.Log, validate(mux)), nil
}

// identify resolves the caller from a bearer token (API or job token) or the
// session cookie and stores it in the request context. It never rejects a
// request itself: whether an operation needs a caller is decided by the spec
// (see authenticate). A request carrying invalid credentials is treated as
// anonymous.
func identify(st *store.Store, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

		if secret := jobTokenFrom(r); secret != "" && auth.LooksLikeToken(secret) {
			jobID, err := st.LookupJobToken(r.Context(), auth.HashToken(secret))
			switch {
			case err == nil:
				r = r.WithContext(auth.WithJob(r.Context(), jobID))
			case !errors.Is(err, store.ErrNotFound):
				log.ErrorContext(r.Context(), "job token lookup failed", "err", err)
				writeProblem(w, problem(http.StatusServiceUnavailable, "Service unavailable"))
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		secret, kind, method := credentialsFrom(r)
		if secret != "" && auth.LooksLikeToken(secret) {
			tok, user, err := st.LookupToken(r.Context(), auth.HashToken(secret))
			switch {
			case err == nil && tok.Kind == kind:
				r = r.WithContext(auth.WithPrincipal(r.Context(), &auth.Principal{
					UserID: user.ID, Username: user.Username, IsAdmin: user.IsAdmin,
					Method: method, TokenID: tok.ID,
				}))
			case err != nil && !errors.Is(err, store.ErrNotFound):
				log.ErrorContext(r.Context(), "token lookup failed", "err", err)
				writeProblem(w, problem(http.StatusServiceUnavailable, "Service unavailable"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// credentialsFrom returns the token secret the request presents. API tokens
// come in the Authorization header; sessions only in the cookie.
func credentialsFrom(r *http.Request) (secret string, kind store.TokenKind, method auth.Method) {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, token, ok := strings.Cut(h, " ")
		if ok && strings.EqualFold(scheme, "Bearer") && strings.HasPrefix(token, auth.APITokenPrefix) {
			return strings.TrimSpace(token), store.TokenKindAPI, auth.MethodAPIToken
		}
		return "", "", ""
	}
	if c, err := r.Cookie(SessionCookieName); err == nil && strings.HasPrefix(c.Value, auth.SessionTokenPrefix) {
		return c.Value, store.TokenKindSession, auth.MethodSession
	}
	return "", "", ""
}

// jobTokenFrom returns the runner job token the request presents, if any.
func jobTokenFrom(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if ok && strings.EqualFold(scheme, "Bearer") && strings.HasPrefix(token, auth.JobTokenPrefix) {
		return strings.TrimSpace(token)
	}
	return ""
}

var errNotAuthenticated = errors.New("not authenticated")

// authenticate is called by the OpenAPI validator for each security scheme an
// operation accepts. It succeeds when identify found a caller that used that
// scheme.
func authenticate(_ context.Context, in *openapi3filter.AuthenticationInput) error {
	ctx := in.RequestValidationInput.Request.Context()
	if in.SecuritySchemeName == "jobToken" {
		if _, ok := auth.JobFromContext(ctx); ok {
			return nil
		}
		return errNotAuthenticated
	}
	p := auth.FromContext(ctx)
	if p == nil {
		return errNotAuthenticated
	}
	switch in.SecuritySchemeName {
	case "cookieAuth":
		if p.Method == auth.MethodSession {
			return nil
		}
	case "bearerAuth":
		if p.Method == auth.MethodAPIToken {
			return nil
		}
	}
	return errNotAuthenticated
}

func validationErrorHandler(log *slog.Logger) nethttpmiddleware.ErrorHandlerWithOpts {
	return func(ctx context.Context, err error, w http.ResponseWriter, r *http.Request, opts nethttpmiddleware.ErrorHandlerOpts) {
		var secErr *openapi3filter.SecurityRequirementsError
		if errors.As(err, &secErr) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dispatch-ops"`)
			writeProblem(w, problem(http.StatusUnauthorized, "Authentication required"))
			return
		}
		switch opts.StatusCode {
		case http.StatusNotFound:
			writeProblem(w, problem(http.StatusNotFound, "Not found"))
		case http.StatusMethodNotAllowed:
			writeProblem(w, problem(http.StatusMethodNotAllowed, "Method not allowed"))
		case http.StatusBadRequest, 0:
			writeProblem(w, problemDetail(http.StatusBadRequest, "Invalid request", requestErrorDetail(err)))
		default:
			log.WarnContext(ctx, "request validation failed", "status", opts.StatusCode, "err", err)
			writeProblem(w, problemDetail(opts.StatusCode, http.StatusText(opts.StatusCode), err.Error()))
		}
	}
}

// requestErrorDetail turns a validation error into a message for API clients.
func requestErrorDetail(err error) string {
	var reqErr *openapi3filter.RequestError
	if errors.As(err, &reqErr) {
		msg := reqErr.Reason
		if reqErr.Err != nil {
			msg = reqErr.Err.Error()
		}
		if reqErr.Parameter != nil {
			return fmt.Sprintf("parameter %q: %s", reqErr.Parameter.Name, msg)
		}
		if reqErr.RequestBody != nil {
			return "request body: " + msg
		}
		return msg
	}
	return err.Error()
}

func problem(status int, title string) apigen.Problem {
	return apigen.Problem{Status: status, Title: title}
}

func problemDetail(status int, title, detail string) apigen.Problem {
	p := problem(status, title)
	p.Detail = &detail
	return p
}

func unauthorized() apigen.UnauthorizedApplicationProblemPlusJSONResponse {
	return apigen.UnauthorizedApplicationProblemPlusJSONResponse(problem(http.StatusUnauthorized, "Authentication required"))
}

func writeProblem(w http.ResponseWriter, p apigen.Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
