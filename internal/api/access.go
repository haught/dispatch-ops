package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/rbac"
	"dispatch-ops/internal/store"
)

// Authorization checks. Handlers call these before acting; the request
// pipeline only checks that there is a caller (see authenticate).
//
// A caller who may not read a resource is told it was not found, so they
// can't probe for names and IDs. One who may read it but lacks the role for
// the action is told it is forbidden.

// caller returns the request's principal. Every operation that isn't
// public (`security: []` in the spec) has one by the time its handler runs.
func caller(ctx context.Context) *auth.Principal {
	if p := auth.FromContext(ctx); p != nil {
		return p
	}
	// Unreachable through the router; fail closed if a handler is misused.
	return &auth.Principal{}
}

// resourceLabels name resource types in messages.
var resourceLabels = map[rbac.ResourceType]string{
	rbac.Inventory:   "Inventory",
	rbac.Credential:  "Credential",
	rbac.Project:     "Project",
	rbac.JobTemplate: "Job template",
}

// authorize checks that the caller holds want on a resource.
func (h *Handlers) authorize(ctx context.Context, t rbac.ResourceType, id uuid.UUID, want rbac.Role) error {
	p := caller(ctx)
	if p.IsAdmin || (p.IsAuditor && want == rbac.Read) {
		return nil
	}
	held, err := h.Store.ResourceRoles(ctx, p.UserID, t, &id)
	if err != nil {
		return err
	}
	if rbac.Allows(held, want) {
		return nil
	}
	if !rbac.Allows(held, rbac.Read) && !p.IsAuditor {
		return notFound(resourceLabels[t])
	}
	return forbidden(fmt.Sprintf("You need the %s role on this %s", want, strings.ToLower(resourceLabels[t])))
}

// authorizeUse checks that the caller may reference a resource (from a job
// template, or a project's credential). A reference the caller can't see
// is reported as missing, like one that doesn't exist.
func (h *Handlers) authorizeUse(ctx context.Context, t rbac.ResourceType, id uuid.UUID) error {
	err := h.authorize(ctx, t, id, rbac.Use)
	var ae *apiError
	if errors.As(err, &ae) && ae.status == 404 {
		return badRequest("%s %s does not exist", strings.ToLower(resourceLabels[t]), id)
	}
	return err
}

// authorizeCreate checks that the caller may create resources of type t:
// system administrators, and holders of admin on every resource of the type.
func (h *Handlers) authorizeCreate(ctx context.Context, t rbac.ResourceType) error {
	p := caller(ctx)
	if p.IsAdmin {
		return nil
	}
	held, err := h.Store.ResourceRoles(ctx, p.UserID, t, nil)
	if err != nil {
		return err
	}
	if rbac.Allows(held, rbac.Admin) {
		return nil
	}
	return forbidden(fmt.Sprintf("Creating a %s needs the admin role on all of them", strings.ToLower(resourceLabels[t])))
}

// grantCreator gives the caller admin on a resource they created, unless
// they are a system administrator, who needs no grant.
func (h *Handlers) grantCreator(ctx context.Context, t rbac.ResourceType, id uuid.UUID) error {
	p := caller(ctx)
	if p.IsAdmin {
		return nil
	}
	_, err := h.Store.CreateRoleGrant(ctx, store.NewRoleGrant{
		Role: rbac.Admin, ResourceType: &t, ResourceID: &id, UserID: &p.UserID,
	})
	if err != nil {
		return fmt.Errorf("grant admin on new %s to its creator: %w", t, err)
	}
	return nil
}

// requireSystemAdmin checks that the caller is a system administrator.
func requireSystemAdmin(ctx context.Context) error {
	if caller(ctx).IsAdmin {
		return nil
	}
	return forbidden("Only system administrators can do this")
}

// requireAuditor checks that the caller may read everything: a system
// administrator or an auditor.
func requireAuditor(ctx context.Context) error {
	if p := caller(ctx); p.IsAdmin || p.IsAuditor {
		return nil
	}
	return forbidden("Only system administrators and auditors can see this")
}

// readableBy returns the user whose roles limit what lists show the
// caller, or nil if the caller may read everything.
func readableBy(ctx context.Context) *uuid.UUID {
	p := caller(ctx)
	if p.IsAdmin || p.IsAuditor {
		return nil
	}
	return &p.UserID
}

// authorizeJob checks that the caller may see a job (want is rbac.Read) or
// cancel it (rbac.Execute): whoever launched it may do both, and roles on
// its template apply. For an inventory sync, roles on the inventory apply:
// read to see it, admin to cancel it.
func (h *Handlers) authorizeJob(ctx context.Context, j store.Job, want rbac.Role) error {
	p := caller(ctx)
	if p.IsAdmin || (p.IsAuditor && want == rbac.Read) {
		return nil
	}
	if j.LaunchedBy != nil && j.LaunchedBy.ID == p.UserID {
		return nil
	}
	var held []rbac.Role
	switch {
	case j.TemplateID != nil:
		var err error
		if held, err = h.Store.ResourceRoles(ctx, p.UserID, rbac.JobTemplate, j.TemplateID); err != nil {
			return err
		}
	case j.SyncInventoryID != nil:
		inv, err := h.Store.ResourceRoles(ctx, p.UserID, rbac.Inventory, j.SyncInventoryID)
		if err != nil {
			return err
		}
		// Admin on the inventory is what launches its syncs, so it is
		// what cancels them; any role on it lets them be seen.
		if rbac.Allows(inv, rbac.Admin) {
			held = []rbac.Role{rbac.Execute}
		} else if rbac.Allows(inv, rbac.Read) {
			held = []rbac.Role{rbac.Read}
		}
	}
	if rbac.Allows(held, want) {
		return nil
	}
	if !rbac.Allows(held, rbac.Read) && !p.IsAuditor {
		return notFound("Job")
	}
	return forbidden("Only whoever launched the job and those with the execute role on its template can cancel it")
}

// getJob loads a job the caller may see.
func (h *Handlers) getJob(ctx context.Context, id uuid.UUID) (store.Job, error) {
	j, err := h.Store.GetJob(ctx, id)
	if err != nil {
		return store.Job{}, orNotFound(err, "Job")
	}
	return j, h.authorizeJob(ctx, j, rbac.Read)
}
