package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/rbac"
	"dispatch-ops/internal/store"
)

func (h *Handlers) ListRoleGrants(ctx context.Context, req apigen.ListRoleGrantsRequestObject) (apigen.ListRoleGrantsResponseObject, error) {
	p := req.Params
	f := store.RoleGrantFilter{
		ResourceType: string(deref(p.ResourceType)), ResourceID: p.ResourceId, HolderUserID: p.UserId, TeamID: p.TeamId,
	}
	if err := h.authorizeGrantList(ctx, f); err != nil {
		return nil, err
	}
	items, total, err := h.Store.ListRoleGrants(ctx, f, pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListRoleGrants200JSONResponse{Items: make([]apigen.RoleGrant, 0, len(items)), Total: total}
	for _, g := range items {
		out.Items = append(out.Items, toAPIRoleGrant(g))
	}
	return out, nil
}

// authorizeGrantList checks that the caller may list the grants f selects:
// system administrators and auditors any; others the grants on a resource
// they administer, or their own.
func (h *Handlers) authorizeGrantList(ctx context.Context, f store.RoleGrantFilter) error {
	p := caller(ctx)
	if p.IsAdmin || p.IsAuditor {
		return nil
	}
	if f.HolderUserID != nil && *f.HolderUserID == p.UserID {
		return nil
	}
	t := rbac.ResourceType(f.ResourceType)
	if f.ResourceID != nil && rbac.RolesFor(t) != nil {
		return h.authorize(ctx, t, *f.ResourceID, rbac.Admin)
	}
	return forbidden(fmt.Sprintf("Filter by a resource you administer (resourceType and resourceId), or by userId=%s", p.UserID))
}

func (h *Handlers) CreateRoleGrant(ctx context.Context, req apigen.CreateRoleGrantRequestObject) (apigen.CreateRoleGrantResponseObject, error) {
	b := req.Body
	if (b.UserId == nil) == (b.TeamId == nil) {
		return nil, badRequest("give exactly one of userId and teamId")
	}
	in := store.NewRoleGrant{Role: rbac.Role(b.Role), ResourceID: b.ResourceId, UserID: b.UserId, TeamID: b.TeamId}
	var t rbac.ResourceType
	if b.ResourceType != nil {
		t = rbac.ResourceType(*b.ResourceType)
		in.ResourceType = &t
	} else if b.ResourceId != nil {
		return nil, badRequest("resourceId needs a resourceType")
	}
	if !rbac.Valid(t, in.Role) {
		if t == "" {
			return nil, badRequest("%q is not a global role; global roles are %s", in.Role, joinRoles(rbac.GlobalRoles))
		}
		return nil, badRequest("the %s role can't be granted on a %s; use %s", in.Role, t, joinRoles(rbac.RolesFor(t)))
	}

	// Global roles and roles on every resource of a type are for system
	// administrators to grant; a resource's admins may grant roles on it.
	if b.ResourceId == nil {
		if err := requireSystemAdmin(ctx); err != nil {
			return nil, err
		}
	} else if err := h.authorize(ctx, t, *b.ResourceId, rbac.Admin); err != nil {
		return nil, err
	}

	g, err := h.Store.CreateRoleGrant(ctx, in)
	switch {
	case errors.Is(err, store.ErrConflict):
		return nil, conflict("the role is already granted")
	case errors.Is(err, store.ErrInvalidReference):
		return nil, badRequest("the user, team or resource does not exist")
	case err != nil:
		return nil, err
	}
	out := toAPIRoleGrant(g)
	h.Log.InfoContext(ctx, "role granted", "grant_id", g.ID, "role", g.Role, "resource_type", t,
		"resource_id", g.ResourceID, "user", g.Username, "team", g.TeamName, "by", principalName(ctx))
	h.audit(ctx, grantAudit(apigen.Grant, g))
	return apigen.CreateRoleGrant201JSONResponse(out), nil
}

func (h *Handlers) DeleteRoleGrant(ctx context.Context, req apigen.DeleteRoleGrantRequestObject) (apigen.DeleteRoleGrantResponseObject, error) {
	g, err := h.Store.GetRoleGrant(ctx, req.GrantId)
	if err != nil {
		return nil, orNotFound(err, "Role grant")
	}
	if g.ResourceID == nil {
		if err := requireSystemAdmin(ctx); err != nil {
			return nil, err
		}
	} else if err := h.authorize(ctx, *g.ResourceType, *g.ResourceID, rbac.Admin); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == 404 {
			return nil, notFound("Role grant")
		}
		return nil, err
	}
	if err := h.Store.DeleteRoleGrant(ctx, g.ID); err != nil {
		return nil, orNotFound(err, "Role grant")
	}
	h.Log.InfoContext(ctx, "role revoked", "grant_id", g.ID, "role", g.Role, "resource_type", g.ResourceType,
		"resource_id", g.ResourceID, "user", g.Username, "team", g.TeamName, "by", principalName(ctx))
	h.audit(ctx, grantAudit(apigen.Revoke, g))
	return apigen.DeleteRoleGrant204Response{}, nil
}

// grantAudit records a grant or revoke against the resource the role is on,
// or, for global roles, against the user or team holding it.
func grantAudit(action apigen.AuditAction, g store.RoleGrant) auditRecord {
	detail := map[string]any{"grantId": g.ID, "role": g.Role}
	if g.ResourceType != nil {
		detail["resourceType"] = *g.ResourceType
		if g.ResourceID == nil {
			detail["allResources"] = true
		}
	}
	r := auditRecord{Action: action}
	if g.UserID != nil {
		detail["user"], detail["userId"] = g.Username, *g.UserID
	} else {
		detail["team"], detail["teamId"] = g.TeamName, *g.TeamID
	}
	switch {
	case g.ResourceID != nil:
		r.ResourceType, r.ResourceID, r.ResourceName = apigen.AuditResourceType(*g.ResourceType), *g.ResourceID, g.ResourceName
	case g.UserID != nil:
		r.ResourceType, r.ResourceID, r.ResourceName = apigen.AuditResourceTypeUser, *g.UserID, g.Username
	default:
		r.ResourceType, r.ResourceID, r.ResourceName = apigen.AuditResourceTypeTeam, *g.TeamID, g.TeamName
	}
	if action == apigen.Revoke {
		r.Before = detail
	} else {
		r.After = detail
	}
	return r
}

func joinRoles(roles []rbac.Role) string {
	s := make([]string, len(roles))
	for i, r := range roles {
		s[i] = string(r)
	}
	return strings.Join(s, ", ")
}

func toAPIRoleGrant(g store.RoleGrant) apigen.RoleGrant {
	out := apigen.RoleGrant{
		Id: g.ID, Role: apigen.Role(g.Role), ResourceId: g.ResourceID, ResourceName: g.ResourceName, CreatedAt: g.CreatedAt,
	}
	if g.ResourceType != nil {
		t := apigen.ResourceType(*g.ResourceType)
		out.ResourceType = &t
	}
	if g.UserID != nil {
		out.User = &apigen.UserRef{Id: *g.UserID, Username: g.Username}
	}
	if g.TeamID != nil {
		out.Team = &apigen.TeamRef{Id: *g.TeamID, Name: g.TeamName}
	}
	return out
}
