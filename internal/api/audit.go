package api

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/store"
)

// auditRecord describes one change for the audit log. Before and After are
// API representations (never secrets). For updates, give both and only the
// fields that differ are kept.
type auditRecord struct {
	Action       apigen.AuditAction
	ResourceType apigen.AuditResourceType
	ResourceID   uuid.UUID
	ResourceName string
	Before       any
	After        any
}

// audit records a change made by the request's caller. The change has
// already happened, so a failure to record it is logged rather than
// returned.
func (h *Handlers) audit(ctx context.Context, r auditRecord) {
	p := caller(ctx)
	h.auditAs(ctx, p.UserID, p.Username, string(p.Method), r)
}

// auditAs records a change made by the given user, for requests that have
// no caller yet (sign-in).
func (h *Handlers) auditAs(ctx context.Context, actorID uuid.UUID, actorName, method string, r auditRecord) {
	before, after := toMap(r.Before), toMap(r.After)
	if before != nil && after != nil {
		before, after = changedFields(before, after)
	}
	e := store.AuditEntry{
		ActorName: actorName, AuthMethod: method, Action: string(r.Action), ResourceType: string(r.ResourceType),
		ResourceName: r.ResourceName, Before: before, After: after,
	}
	if actorID != uuid.Nil {
		e.ActorID = &actorID
	}
	if r.ResourceID != uuid.Nil {
		e.ResourceID = &r.ResourceID
	}
	if err := h.Store.AddAuditEntry(context.WithoutCancel(ctx), e); err != nil {
		h.Log.ErrorContext(ctx, "could not write audit log entry", "err", err, "action", r.Action,
			"resource_type", r.ResourceType, "resource_id", r.ResourceID, "by", actorName)
	}
}

// toMap returns v's JSON object form, or nil.
func toMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// changedFields returns the fields of before and after whose values
// differ, ignoring timestamps that change on every update.
func changedFields(before, after map[string]any) (map[string]any, map[string]any) {
	b, a := map[string]any{}, map[string]any{}
	for k, v := range after {
		if k == "updatedAt" {
			continue
		}
		if old, ok := before[k]; !ok || !reflect.DeepEqual(old, v) {
			a[k] = v
			if ok {
				b[k] = old
			}
		}
	}
	for k, v := range before {
		if _, ok := after[k]; !ok && k != "updatedAt" {
			b[k] = v
		}
	}
	return b, a
}

func toAPIAuditEntry(e store.AuditEntry) apigen.AuditEntry {
	out := apigen.AuditEntry{
		Id: e.ID, Time: e.Time, ActorName: e.ActorName, AuthMethod: e.AuthMethod,
		Action: apigen.AuditAction(e.Action), ResourceType: apigen.AuditResourceType(e.ResourceType),
		ResourceId: e.ResourceID, ResourceName: e.ResourceName,
	}
	if e.ActorID != nil {
		out.Actor = &apigen.UserRef{Id: *e.ActorID, Username: e.ActorName}
	}
	if e.Before != nil {
		out.Before = &e.Before
	}
	if e.After != nil {
		out.After = &e.After
	}
	return out
}

func (h *Handlers) ListAuditLog(ctx context.Context, req apigen.ListAuditLogRequestObject) (apigen.ListAuditLogResponseObject, error) {
	if err := requireAuditor(ctx); err != nil {
		return nil, err
	}
	p := req.Params
	items, total, err := h.Store.ListAuditLog(ctx, store.AuditFilter{
		ResourceType: string(deref(p.ResourceType)), ResourceID: p.ResourceId, ActorID: p.ActorId,
		Action: string(deref(p.Action)), Since: p.Since, Until: p.Until,
	}, pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListAuditLog200JSONResponse{Items: make([]apigen.AuditEntry, 0, len(items)), Total: total}
	for _, e := range items {
		out.Items = append(out.Items, toAPIAuditEntry(e))
	}
	return out, nil
}
