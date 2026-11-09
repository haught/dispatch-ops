package api

import (
	"errors"
	"fmt"
	"net/http"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/credentials"
	"dispatch-ops/internal/store"
)

// apiError is an error a handler returns to send a specific problem
// response. Handlers may also return store sentinel errors directly; see
// problemFor.
type apiError struct {
	status int
	title  string
	detail string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.status, e.title, e.detail) }

func badRequest(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, "Invalid request", fmt.Sprintf(format, args...)}
}

func notFound(what string) error {
	return &apiError{http.StatusNotFound, "Not found", what + " not found"}
}

// orNotFound names the missing resource when err is store.ErrNotFound.
func orNotFound(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return notFound(what)
	}
	return err
}

func conflict(format string, args ...any) error {
	return &apiError{http.StatusConflict, "Conflict", fmt.Sprintf(format, args...)}
}

func forbidden(detail string) error {
	return &apiError{http.StatusForbidden, "Forbidden", detail}
}

// problemFor maps an error returned by a handler to the problem response
// to send. ok is false for unexpected errors, which become a 500.
func problemFor(err error) (p apigen.Problem, ok bool) {
	var ae *apiError
	var ve *credentials.ValidationError
	var le *store.LaunchError
	switch {
	case errors.As(err, &ae):
		return problemDetail(ae.status, ae.title, ae.detail), true
	case errors.As(err, &ve):
		return problemDetail(http.StatusBadRequest, "Invalid request", ve.Error()), true
	case errors.As(err, &le):
		return problemDetail(http.StatusBadRequest, "Invalid request", le.Error()), true
	case errors.Is(err, store.ErrNotFound):
		return problem(http.StatusNotFound, "Not found"), true
	case errors.Is(err, store.ErrConflict):
		return problemDetail(http.StatusConflict, "Conflict", "The name is already in use"), true
	case errors.Is(err, store.ErrInUse):
		return problemDetail(http.StatusConflict, "In use", "Job templates or inventory sources still use it; change or delete them first"), true
	case errors.Is(err, store.ErrInvalidReference):
		return problemDetail(http.StatusBadRequest, "Invalid request", "A referenced resource does not exist (or is in another inventory)"), true
	case errors.Is(err, store.ErrCycle):
		return problemDetail(http.StatusBadRequest, "Invalid request", "A group cannot be its own descendant"), true
	case errors.Is(err, store.ErrJobState):
		return problemDetail(http.StatusConflict, "Conflict", "The job is not in a state that allows this"), true
	}
	return apigen.Problem{}, false
}
