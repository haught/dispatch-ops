package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

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
	var iu *store.InUseError
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
	case errors.As(err, &iu):
		return problemDetail(http.StatusConflict, "In use", inUseDetail(iu)), true
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

// inUseDetail says what still uses a resource, e.g. `Still used by job
// template "deploy", and by the sources of inventories "prod", "staging";
// change or delete them first`.
func inUseDetail(e *store.InUseError) string {
	var parts []string
	if e.JobTemplateCount > 0 {
		parts = append(parts, namedList("job template", "job templates", e.JobTemplates, e.JobTemplateCount))
	}
	if e.InventorySourceCount > 0 {
		parts = append(parts, namedList("the source of inventory", "the sources of inventories", e.InventorySources, e.InventorySourceCount))
	}
	them := "them"
	if e.JobTemplateCount+e.InventorySourceCount == 1 {
		them = "it"
	}
	return fmt.Sprintf("Still used by %s; change or delete %s first", strings.Join(parts, ", and by "), them)
}

// namedList lists quoted names, noting how many more there are than names.
func namedList(one, many string, names []string, total int) string {
	label := many
	if total == 1 {
		label = one
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	s := label + " " + strings.Join(quoted, ", ")
	if more := total - len(names); more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	return s
}
