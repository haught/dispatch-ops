package api

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/notify"
	"dispatch-ops/internal/rbac"
	"dispatch-ops/internal/store"
)

func (h *Handlers) notifyVault() notify.Vault { return notify.Vault{Keys: h.Vault.Keys} }

func (h *Handlers) notifier() *notify.Sender {
	if h.Notifier != nil {
		return h.Notifier
	}
	return &notify.Sender{Version: h.Version}
}

// toAPINotificationTarget shows a target. Only system administrators and
// auditors see its config: a webhook URL can reveal more than its name.
func toAPINotificationTarget(ctx context.Context, n store.NotificationTarget) apigen.NotificationTarget {
	out := apigen.NotificationTarget{
		Id: n.ID, Name: n.Name, Description: n.Description, Kind: apigen.NotificationKind(n.Kind),
		Config: map[string]any{}, SecretFields: nonNilSlice(n.SecretFields), CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
	}
	if p := caller(ctx); (p.IsAdmin || p.IsAuditor) && n.Config != nil {
		out.Config = n.Config
	}
	return out
}

func (h *Handlers) ListNotificationTargets(ctx context.Context, req apigen.ListNotificationTargetsRequestObject) (apigen.ListNotificationTargetsResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListNotificationTargets(ctx, deref(p.Search), pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListNotificationTargets200JSONResponse{Items: make([]apigen.NotificationTarget, 0, len(items)), Total: total}
	for _, n := range items {
		out.Items = append(out.Items, toAPINotificationTarget(ctx, n))
	}
	return out, nil
}

func (h *Handlers) GetNotificationTarget(ctx context.Context, req apigen.GetNotificationTargetRequestObject) (apigen.GetNotificationTargetResponseObject, error) {
	n, err := h.Store.GetNotificationTarget(ctx, req.TargetId)
	if err != nil {
		return nil, orNotFound(err, "Notification target")
	}
	return apigen.GetNotificationTarget200JSONResponse(toAPINotificationTarget(ctx, n)), nil
}

func (h *Handlers) CreateNotificationTarget(ctx context.Context, req apigen.CreateNotificationTargetRequestObject) (apigen.CreateNotificationTargetResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	b := req.Body
	n := store.NotificationTarget{ID: uuid.New(), Name: b.Name, Description: deref(b.Description), Kind: string(b.Kind)}
	var err error
	if n.Config, err = notify.NormalizeConfig(n.Kind, deref(b.Config)); err != nil {
		return nil, badRequest("%s", err.Error())
	}
	secrets := notify.MergeSecrets(nil, deref(b.Secrets))
	if err := notify.CheckSecrets(n.Kind, n.Config, secrets); err != nil {
		return nil, badRequest("%s", err.Error())
	}
	if err := h.notifyVault().Seal(&n, secrets); err != nil {
		return nil, err
	}
	n, err = h.Store.CreateNotificationTarget(ctx, n)
	if errors.Is(err, store.ErrConflict) {
		return nil, conflict("a notification target named %q already exists", b.Name)
	}
	if err != nil {
		return nil, err
	}
	out := toAPINotificationTarget(ctx, n)
	h.audit(ctx, auditRecord{
		Action: apigen.Create, ResourceType: apigen.AuditResourceTypeNotificationTarget, ResourceID: n.ID, ResourceName: n.Name,
		After: out,
	})
	return apigen.CreateNotificationTarget201JSONResponse(out), nil
}

func (h *Handlers) UpdateNotificationTarget(ctx context.Context, req apigen.UpdateNotificationTargetRequestObject) (apigen.UpdateNotificationTargetResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	b := req.Body
	var before apigen.NotificationTarget
	n, err := h.Store.UpdateNotificationTarget(ctx, req.TargetId, func(n *store.NotificationTarget) error {
		before = toAPINotificationTarget(ctx, *n)
		current, err := h.notifyVault().Open(*n)
		if err != nil {
			return err
		}
		setIf(&n.Name, b.Name)
		setIf(&n.Description, b.Description)
		if b.Config != nil {
			if n.Config, err = notify.NormalizeConfig(n.Kind, *b.Config); err != nil {
				return badRequest("%s", err.Error())
			}
		}
		secrets := notify.MergeSecrets(current, deref(b.Secrets))
		if err := notify.CheckSecrets(n.Kind, n.Config, secrets); err != nil {
			return badRequest("%s", err.Error())
		}
		// Sealing again also moves the values to the current key.
		return h.notifyVault().Seal(n, secrets)
	})
	if errors.Is(err, store.ErrConflict) {
		return nil, conflict("a notification target with that name already exists")
	}
	if err != nil {
		return nil, orNotFound(err, "Notification target")
	}
	out := toAPINotificationTarget(ctx, n)
	after := toMap(out)
	if b.Secrets != nil && len(*b.Secrets) > 0 {
		// Record which secrets were changed, never their values.
		after["secretsChanged"] = slices.Sorted(maps.Keys(*b.Secrets))
	}
	h.audit(ctx, auditRecord{
		Action: apigen.Update, ResourceType: apigen.AuditResourceTypeNotificationTarget, ResourceID: n.ID, ResourceName: n.Name,
		Before: before, After: after,
	})
	return apigen.UpdateNotificationTarget200JSONResponse(out), nil
}

func (h *Handlers) DeleteNotificationTarget(ctx context.Context, req apigen.DeleteNotificationTargetRequestObject) (apigen.DeleteNotificationTargetResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	n, err := h.Store.GetNotificationTarget(ctx, req.TargetId)
	if err != nil {
		return nil, orNotFound(err, "Notification target")
	}
	if err := h.Store.DeleteNotificationTarget(ctx, n.ID); err != nil {
		return nil, orNotFound(err, "Notification target")
	}
	h.audit(ctx, auditRecord{
		Action: apigen.Delete, ResourceType: apigen.AuditResourceTypeNotificationTarget, ResourceID: n.ID, ResourceName: n.Name,
		Before: toAPINotificationTarget(ctx, n),
	})
	return apigen.DeleteNotificationTarget204Response{}, nil
}

func (h *Handlers) TestNotificationTarget(ctx context.Context, req apigen.TestNotificationTargetRequestObject) (apigen.TestNotificationTargetResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	n, err := h.Store.GetNotificationTarget(ctx, req.TargetId)
	if err != nil {
		return nil, orNotFound(err, "Notification target")
	}
	secrets, err := h.notifyVault().Open(n)
	if err != nil {
		return nil, err
	}
	out := apigen.TestNotificationTarget200JSONResponse{Ok: true}
	if err := h.notifier().Send(ctx, n, secrets, notify.Event{Name: notify.EventTest}); err != nil {
		out.Ok, out.Error = false, err.Error()
	}
	h.Log.InfoContext(ctx, "test notification", "target", n.Name, "ok", out.Ok, "err", out.Error, "by", principalName(ctx))
	return out, nil
}

// --- On job templates ---

func toAPITemplateNotifications(items []store.TemplateNotification) apigen.TemplateNotificationList {
	out := apigen.TemplateNotificationList{Items: make([]apigen.TemplateNotification, 0, len(items))}
	for _, it := range items {
		events := make([]apigen.NotificationEvent, len(it.Events))
		for i, e := range it.Events {
			events[i] = apigen.NotificationEvent(e)
		}
		out.Items = append(out.Items, apigen.TemplateNotification{
			TargetId: it.TargetID, TargetName: it.TargetName, TargetKind: apigen.NotificationKind(it.TargetKind), Events: events,
		})
	}
	return out
}

func (h *Handlers) ListJobTemplateNotifications(ctx context.Context, req apigen.ListJobTemplateNotificationsRequestObject) (apigen.ListJobTemplateNotificationsResponseObject, error) {
	if err := h.authorize(ctx, rbac.JobTemplate, req.JobTemplateId, rbac.Read); err != nil {
		return nil, err
	}
	if _, err := h.Store.GetJobTemplate(ctx, req.JobTemplateId); err != nil {
		return nil, orNotFound(err, "Job template")
	}
	items, err := h.Store.ListTemplateNotifications(ctx, req.JobTemplateId)
	if err != nil {
		return nil, err
	}
	return apigen.ListJobTemplateNotifications200JSONResponse(toAPITemplateNotifications(items)), nil
}

func (h *Handlers) SetJobTemplateNotifications(ctx context.Context, req apigen.SetJobTemplateNotificationsRequestObject) (apigen.SetJobTemplateNotificationsResponseObject, error) {
	if err := h.authorize(ctx, rbac.JobTemplate, req.JobTemplateId, rbac.Admin); err != nil {
		return nil, err
	}
	t, err := h.Store.GetJobTemplate(ctx, req.JobTemplateId)
	if err != nil {
		return nil, orNotFound(err, "Job template")
	}
	before, err := h.Store.ListTemplateNotifications(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	var items []store.TemplateNotification
	seen := map[uuid.UUID]bool{}
	for _, it := range req.Body.Items {
		if seen[it.TargetId] {
			return nil, badRequest("notification target %s is listed twice", it.TargetId)
		}
		seen[it.TargetId] = true
		events := make([]string, len(it.Events))
		for i, e := range it.Events {
			events[i] = string(e)
		}
		items = append(items, store.TemplateNotification{TargetID: it.TargetId, Events: events})
	}
	after, err := h.Store.SetTemplateNotifications(ctx, t.ID, items)
	if errors.Is(err, store.ErrInvalidReference) {
		return nil, badRequest("a notification target in items does not exist")
	}
	if err != nil {
		return nil, orNotFound(err, "Job template")
	}
	out := toAPITemplateNotifications(after)
	h.audit(ctx, auditRecord{
		Action: apigen.Update, ResourceType: apigen.AuditResourceTypeJobTemplate, ResourceID: t.ID, ResourceName: t.Name,
		Before: map[string]any{"notifications": toAPITemplateNotifications(before).Items},
		After:  map[string]any{"notifications": out.Items},
	})
	return apigen.SetJobTemplateNotifications200JSONResponse(out), nil
}
