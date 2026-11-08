package api

import (
	"context"
	"errors"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/store"
)

func (h *Handlers) ListUsers(ctx context.Context, req apigen.ListUsersRequestObject) (apigen.ListUsersResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListUsers(ctx, deref(p.Search), pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListUsers200JSONResponse{Items: make([]apigen.User, 0, len(items)), Total: total}
	for _, u := range items {
		out.Items = append(out.Items, toAPIUser(u))
	}
	return out, nil
}

func (h *Handlers) CreateUser(ctx context.Context, req apigen.CreateUserRequestObject) (apigen.CreateUserResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	b := req.Body
	hash, err := auth.HashPassword(b.Password)
	if err != nil {
		return nil, err
	}
	u, err := h.Store.CreateLocalUser(ctx, store.NewLocalUser{
		Username: b.Username, DisplayName: deref(b.DisplayName), Email: deref(b.Email), PasswordHash: hash,
	})
	if errors.Is(err, store.ErrConflict) {
		return nil, conflict("the username %q is taken", b.Username)
	}
	if err != nil {
		return nil, err
	}
	out := toAPIUser(u)
	h.Log.InfoContext(ctx, "user created", "user_id", u.ID, "username", u.Username, "by", principalName(ctx))
	h.audit(ctx, auditRecord{
		Action: apigen.Create, ResourceType: apigen.AuditResourceTypeUser, ResourceID: u.ID, ResourceName: u.Username,
		After: userAudit(out),
	})
	return apigen.CreateUser201JSONResponse(out), nil
}

func (h *Handlers) GetUser(ctx context.Context, req apigen.GetUserRequestObject) (apigen.GetUserResponseObject, error) {
	u, err := h.Store.GetUser(ctx, req.UserId)
	if err != nil {
		return nil, orNotFound(err, "User")
	}
	return apigen.GetUser200JSONResponse(toAPIUser(u)), nil
}

func (h *Handlers) UpdateUser(ctx context.Context, req apigen.UpdateUserRequestObject) (apigen.UpdateUserResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	b := req.Body
	if b.IsActive != nil && !*b.IsActive && req.UserId == caller(ctx).UserID {
		return nil, badRequest("you can't deactivate yourself")
	}
	var hash *string
	if b.Password != nil {
		h, err := auth.HashPassword(*b.Password)
		if err != nil {
			return nil, err
		}
		hash = &h
	}
	before, err := h.Store.GetUser(ctx, req.UserId)
	if err != nil {
		return nil, orNotFound(err, "User")
	}
	u, err := h.Store.UpdateUser(ctx, req.UserId, func(f *store.UserFields) error {
		if f.AuthSource != "local" && (b.Username != nil || b.Password != nil) {
			return badRequest("single sign-on users get their username from the provider and have no password")
		}
		setIf(&f.Username, b.Username)
		setIf(&f.DisplayName, b.DisplayName)
		setIf(&f.Email, b.Email)
		setIf(&f.IsActive, b.IsActive)
		if hash != nil {
			f.PasswordHash = hash
		}
		return nil
	})
	if errors.Is(err, store.ErrConflict) {
		return nil, conflict("the username %q is taken", deref(b.Username))
	}
	if err != nil {
		return nil, orNotFound(err, "User")
	}
	out := toAPIUser(u)
	after := userAudit(out)
	if b.Password != nil {
		after["passwordChanged"] = true
	}
	h.Log.InfoContext(ctx, "user updated", "user_id", u.ID, "username", u.Username, "by", principalName(ctx))
	h.audit(ctx, auditRecord{
		Action: apigen.Update, ResourceType: apigen.AuditResourceTypeUser, ResourceID: u.ID, ResourceName: u.Username,
		Before: userAudit(toAPIUser(before)), After: after,
	})
	return apigen.UpdateUser200JSONResponse(out), nil
}

func (h *Handlers) DeleteUser(ctx context.Context, req apigen.DeleteUserRequestObject) (apigen.DeleteUserResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	if req.UserId == caller(ctx).UserID {
		return nil, badRequest("you can't delete yourself")
	}
	u, err := h.Store.GetUser(ctx, req.UserId)
	if err != nil {
		return nil, orNotFound(err, "User")
	}
	if err := h.Store.DeleteUser(ctx, req.UserId); err != nil {
		return nil, orNotFound(err, "User")
	}
	h.Log.InfoContext(ctx, "user deleted", "user_id", u.ID, "username", u.Username, "by", principalName(ctx))
	h.audit(ctx, auditRecord{
		Action: apigen.Delete, ResourceType: apigen.AuditResourceTypeUser, ResourceID: u.ID, ResourceName: u.Username,
		Before: userAudit(toAPIUser(u)),
	})
	return apigen.DeleteUser204Response{}, nil
}

// userAudit is what the audit log records about a user: not sign-in times,
// which aren't changes.
func userAudit(u apigen.User) map[string]any {
	m := toMap(u)
	delete(m, "lastLoginAt")
	return m
}
