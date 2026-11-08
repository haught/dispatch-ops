package api

import (
	"context"
	"errors"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/store"
)

func (h *Handlers) ListTeams(ctx context.Context, req apigen.ListTeamsRequestObject) (apigen.ListTeamsResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListTeams(ctx, deref(p.Search), p.UserId, pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListTeams200JSONResponse{Items: make([]apigen.Team, 0, len(items)), Total: total}
	for _, t := range items {
		out.Items = append(out.Items, toAPITeam(t))
	}
	return out, nil
}

func (h *Handlers) CreateTeam(ctx context.Context, req apigen.CreateTeamRequestObject) (apigen.CreateTeamResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	b := req.Body
	t, err := h.Store.CreateTeam(ctx, store.TeamFields{
		Name: b.Name, Description: deref(b.Description), OIDCGroups: deref(b.OidcGroups),
	})
	if err != nil {
		return nil, err
	}
	out := toAPITeam(t)
	h.audit(ctx, auditRecord{
		Action: apigen.Create, ResourceType: apigen.AuditResourceTypeTeam, ResourceID: t.ID, ResourceName: t.Name,
		After: teamAudit(out),
	})
	return apigen.CreateTeam201JSONResponse(out), nil
}

func (h *Handlers) GetTeam(ctx context.Context, req apigen.GetTeamRequestObject) (apigen.GetTeamResponseObject, error) {
	t, err := h.Store.GetTeam(ctx, req.TeamId)
	if err != nil {
		return nil, orNotFound(err, "Team")
	}
	return apigen.GetTeam200JSONResponse(toAPITeam(t)), nil
}

func (h *Handlers) UpdateTeam(ctx context.Context, req apigen.UpdateTeamRequestObject) (apigen.UpdateTeamResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	b := req.Body
	before, err := h.Store.GetTeam(ctx, req.TeamId)
	if err != nil {
		return nil, orNotFound(err, "Team")
	}
	t, err := h.Store.UpdateTeam(ctx, req.TeamId, func(f *store.TeamFields) error {
		setIf(&f.Name, b.Name)
		setIf(&f.Description, b.Description)
		setIf(&f.OIDCGroups, b.OidcGroups)
		return nil
	})
	if err != nil {
		return nil, orNotFound(err, "Team")
	}
	out := toAPITeam(t)
	h.audit(ctx, auditRecord{
		Action: apigen.Update, ResourceType: apigen.AuditResourceTypeTeam, ResourceID: t.ID, ResourceName: t.Name,
		Before: teamAudit(toAPITeam(before)), After: teamAudit(out),
	})
	return apigen.UpdateTeam200JSONResponse(out), nil
}

func (h *Handlers) DeleteTeam(ctx context.Context, req apigen.DeleteTeamRequestObject) (apigen.DeleteTeamResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	t, err := h.Store.GetTeam(ctx, req.TeamId)
	if err != nil {
		return nil, orNotFound(err, "Team")
	}
	if err := h.Store.DeleteTeam(ctx, req.TeamId); err != nil {
		return nil, orNotFound(err, "Team")
	}
	h.audit(ctx, auditRecord{
		Action: apigen.Delete, ResourceType: apigen.AuditResourceTypeTeam, ResourceID: t.ID, ResourceName: t.Name,
		Before: teamAudit(toAPITeam(t)),
	})
	return apigen.DeleteTeam204Response{}, nil
}

func (h *Handlers) ListTeamMembers(ctx context.Context, req apigen.ListTeamMembersRequestObject) (apigen.ListTeamMembersResponseObject, error) {
	members, err := h.Store.ListTeamMembers(ctx, req.TeamId)
	if err != nil {
		return nil, orNotFound(err, "Team")
	}
	out := apigen.ListTeamMembers200JSONResponse{Items: make([]apigen.TeamMember, 0, len(members))}
	for _, m := range members {
		out.Items = append(out.Items, apigen.TeamMember{
			User: apigen.UserRef{Id: m.UserID, Username: m.Username}, DisplayName: m.DisplayName,
			Source: apigen.TeamMemberSource(m.Source), AddedAt: m.AddedAt,
		})
	}
	return out, nil
}

func (h *Handlers) AddTeamMember(ctx context.Context, req apigen.AddTeamMemberRequestObject) (apigen.AddTeamMemberResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	t, err := h.Store.GetTeam(ctx, req.TeamId)
	if err != nil {
		return nil, orNotFound(err, "Team")
	}
	u, err := h.Store.GetUser(ctx, req.UserId)
	if err != nil {
		return nil, orNotFound(err, "User")
	}
	if err := h.Store.AddTeamMember(ctx, t.ID, u.ID); errors.Is(err, store.ErrInvalidReference) {
		return nil, notFound("Team or user")
	} else if err != nil {
		return nil, err
	}
	h.audit(ctx, auditRecord{
		Action: apigen.AddMember, ResourceType: apigen.AuditResourceTypeTeam, ResourceID: t.ID, ResourceName: t.Name,
		After: map[string]any{"user": u.Username, "userId": u.ID},
	})
	return apigen.AddTeamMember204Response{}, nil
}

func (h *Handlers) RemoveTeamMember(ctx context.Context, req apigen.RemoveTeamMemberRequestObject) (apigen.RemoveTeamMemberResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	t, err := h.Store.GetTeam(ctx, req.TeamId)
	if err != nil {
		return nil, orNotFound(err, "Team")
	}
	u, err := h.Store.GetUser(ctx, req.UserId)
	if err != nil {
		return nil, orNotFound(err, "User")
	}
	if err := h.Store.RemoveTeamMember(ctx, t.ID, u.ID); err != nil {
		return nil, orNotFound(err, "Membership")
	}
	h.audit(ctx, auditRecord{
		Action: apigen.RemoveMember, ResourceType: apigen.AuditResourceTypeTeam, ResourceID: t.ID, ResourceName: t.Name,
		Before: map[string]any{"user": u.Username, "userId": u.ID},
	})
	return apigen.RemoveTeamMember204Response{}, nil
}

func toAPITeam(t store.Team) apigen.Team {
	return apigen.Team{
		Id: t.ID, Name: t.Name, Description: t.Description, OidcGroups: nonNilSlice(t.OIDCGroups),
		MemberCount: t.MemberCount, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// teamAudit is what the audit log records about a team; membership is
// recorded by add_member and remove_member entries.
func teamAudit(t apigen.Team) map[string]any {
	m := toMap(t)
	delete(m, "memberCount")
	return m
}
